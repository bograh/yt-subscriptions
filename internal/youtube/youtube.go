// Package youtube is a minimal YouTube Data API v3 client covering what the dashboard needs.
package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"yt-subscriptions/internal/store"
)

const apiBase = "https://www.googleapis.com/youtube/v3/"

// Scope allows reading subscriptions and deleting them (unsubscribe).
const Scope = "https://www.googleapis.com/auth/youtube"

type Client struct {
	HTTP *http.Client // must be an OAuth2-authorized client
}

// APIError is a non-2xx response from the YouTube Data API.
type APIError struct {
	Endpoint string
	Status   int
	Reason   string // e.g. "quotaExceeded", "subscriptionNotFound"
	Message  string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("youtube %s: %d %s", e.Endpoint, e.Status, msg)
}

// Unauthorized reports whether the credentials were rejected (revoked or expired).
func (e *APIError) Unauthorized() bool { return e.Status == http.StatusUnauthorized }

// QuotaExceeded reports whether the project's daily API quota is used up.
func (e *APIError) QuotaExceeded() bool {
	return e.Reason == "quotaExceeded" || e.Reason == "dailyLimitExceeded" || e.Reason == "rateLimitExceeded"
}

func (e *APIError) NotFound() bool { return e.Status == http.StatusNotFound }

type apiErrorBody struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

func (c *Client) do(ctx context.Context, method, endpoint string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, apiBase+endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		apiErr := &APIError{Endpoint: endpoint, Status: resp.StatusCode}
		var e apiErrorBody
		if json.Unmarshal(body, &e) == nil {
			apiErr.Message = e.Error.Message
			if len(e.Error.Errors) > 0 {
				apiErr.Reason = e.Error.Errors[0].Reason
			}
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

type thumbnails struct {
	Default struct{ URL string } `json:"default"`
	Medium  struct{ URL string } `json:"medium"`
}

// FetchSubscriptions lists every channel the user is subscribed to, enriched with
// channel stats and recent upload titles. progress is called as work completes.
func (c *Client) FetchSubscriptions(ctx context.Context, progress func(done, total int)) ([]store.Channel, error) {
	var channels []store.Channel
	pageToken := ""
	for {
		q := url.Values{"part": {"snippet"}, "mine": {"true"}, "maxResults": {"50"}, "order": {"alphabetical"}}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var res struct {
			NextPageToken string `json:"nextPageToken"`
			Items         []struct {
				ID      string `json:"id"`
				Snippet struct {
					Title       string     `json:"title"`
					Description string     `json:"description"`
					PublishedAt time.Time  `json:"publishedAt"`
					Thumbnails  thumbnails `json:"thumbnails"`
					ResourceID  struct {
						ChannelID string `json:"channelId"`
					} `json:"resourceId"`
				} `json:"snippet"`
			} `json:"items"`
		}
		if err := c.do(ctx, http.MethodGet, "subscriptions", q, &res); err != nil {
			return nil, err
		}
		for _, it := range res.Items {
			thumb := it.Snippet.Thumbnails.Medium.URL
			if thumb == "" {
				thumb = it.Snippet.Thumbnails.Default.URL
			}
			channels = append(channels, store.Channel{
				ID:             it.Snippet.ResourceID.ChannelID,
				SubscriptionID: it.ID,
				Title:          it.Snippet.Title,
				Description:    it.Snippet.Description,
				Thumbnail:      thumb,
				SubscribedAt:   it.Snippet.PublishedAt,
			})
		}
		if res.NextPageToken == "" {
			break
		}
		pageToken = res.NextPageToken
	}

	total := len(channels)
	if err := c.enrichChannels(ctx, channels); err != nil {
		return nil, err
	}
	progress(0, total)
	c.fetchRecentUploads(ctx, channels, func(done int) { progress(done, total) })
	return channels, ctx.Err()
}

// enrichChannels adds stats, topics, and uploads playlist IDs (50 channels per call).
func (c *Client) enrichChannels(ctx context.Context, channels []store.Channel) error {
	idx := make(map[string]int, len(channels))
	for i, ch := range channels {
		idx[ch.ID] = i
	}
	for start := 0; start < len(channels); start += 50 {
		end := min(start+50, len(channels))
		ids := make([]string, 0, end-start)
		for _, ch := range channels[start:end] {
			ids = append(ids, ch.ID)
		}
		q := url.Values{"part": {"statistics,contentDetails,topicDetails"}, "id": {strings.Join(ids, ",")}, "maxResults": {"50"}}
		var res struct {
			Items []struct {
				ID         string `json:"id"`
				Statistics struct {
					SubscriberCount string `json:"subscriberCount"`
					VideoCount      string `json:"videoCount"`
				} `json:"statistics"`
				ContentDetails struct {
					RelatedPlaylists struct {
						Uploads string `json:"uploads"`
					} `json:"relatedPlaylists"`
				} `json:"contentDetails"`
				TopicDetails struct {
					TopicCategories []string `json:"topicCategories"`
				} `json:"topicDetails"`
			} `json:"items"`
		}
		if err := c.do(ctx, http.MethodGet, "channels", q, &res); err != nil {
			return err
		}
		for _, it := range res.Items {
			i, ok := idx[it.ID]
			if !ok {
				continue
			}
			ch := &channels[i]
			ch.SubscriberCount, _ = strconv.ParseInt(it.Statistics.SubscriberCount, 10, 64)
			ch.VideoCount, _ = strconv.ParseInt(it.Statistics.VideoCount, 10, 64)
			ch.UploadsPlaylist = it.ContentDetails.RelatedPlaylists.Uploads
			ch.Topics = nil
			for _, t := range it.TopicDetails.TopicCategories {
				// e.g. https://en.wikipedia.org/wiki/Video_game_culture -> "Video game culture"
				ch.Topics = append(ch.Topics, strings.ReplaceAll(path.Base(t), "_", " "))
			}
		}
	}
	return nil
}

// fetchRecentUploads grabs the latest few upload titles per channel, concurrently.
// Per-channel failures (e.g. channels with no uploads) are ignored.
func (c *Client) fetchRecentUploads(ctx context.Context, channels []store.Channel, progress func(done int)) {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		done int
		sem  = make(chan struct{}, 8)
	)
	for i := range channels {
		ch := &channels[i]
		if ch.UploadsPlaylist == "" {
			mu.Lock()
			done++
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			q := url.Values{"part": {"snippet"}, "playlistId": {ch.UploadsPlaylist}, "maxResults": {"8"}}
			var res struct {
				Items []struct {
					Snippet struct {
						Title       string    `json:"title"`
						PublishedAt time.Time `json:"publishedAt"`
					} `json:"snippet"`
				} `json:"items"`
			}
			if err := c.do(ctx, http.MethodGet, "playlistItems", q, &res); err == nil {
				for _, it := range res.Items {
					ch.RecentTitles = append(ch.RecentTitles, it.Snippet.Title)
					if it.Snippet.PublishedAt.After(ch.LastUpload) {
						ch.LastUpload = it.Snippet.PublishedAt
					}
				}
			}
			mu.Lock()
			done++
			d := done
			mu.Unlock()
			progress(d)
		}()
	}
	wg.Wait()
}

// Unsubscribe deletes a subscription by its subscription ID (not channel ID).
func (c *Client) Unsubscribe(ctx context.Context, subscriptionID string) error {
	return c.do(ctx, http.MethodDelete, "subscriptions", url.Values{"id": {subscriptionID}}, nil)
}
