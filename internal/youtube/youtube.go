// Package youtube is a minimal YouTube Data API v3 client covering what the dashboard needs.
package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	Base string       // API base URL; defaults to the real API (tests override it)
	// OnCost, if set, is called with the quota units each request costs.
	OnCost func(units int)
}

// Quota costs per the YouTube Data API docs: reads are 1 unit; writes
// (insert/update/delete, e.g. subscriptions.delete, playlistItems.insert) are 50.
const (
	CostRead        = 1
	CostWrite       = 50
	CostUnsubscribe = CostWrite
)

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
	return c.send(ctx, method, endpoint, q, nil, out)
}

// send is do with an optional request body, sent as JSON when non-nil.
func (c *Client) send(ctx context.Context, method, endpoint string, q url.Values, in, out any) error {
	base := c.Base
	if base == "" {
		base = apiBase
	}
	var reqBody io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+endpoint+"?"+q.Encode(), reqBody)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.OnCost != nil {
		cost := CostRead
		if method != http.MethodGet {
			cost = CostWrite
		}
		c.OnCost(cost) // Google bills attempts, including failed ones
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
//
// uploadFailures lists channel IDs whose recent uploads couldn't be fetched; their
// LastUpload/RecentTitles are empty and the caller should keep the previous values.
// Quota and credential errors abort the whole sync with an error.
func (c *Client) FetchSubscriptions(ctx context.Context, progress func(done, total int)) (channels []store.Channel, uploadFailures []string, err error) {
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
			return nil, nil, err
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
		return nil, nil, err
	}
	progress(0, total)
	uploadFailures, err = c.fetchRecentUploads(ctx, channels, func(done int) { progress(done, total) })
	if err != nil {
		return nil, nil, err
	}
	return channels, uploadFailures, ctx.Err()
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
					SubscriberCount       string `json:"subscriberCount"`
					HiddenSubscriberCount bool   `json:"hiddenSubscriberCount"`
					VideoCount            string `json:"videoCount"`
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
			ch.SubscribersHidden = it.Statistics.HiddenSubscriberCount
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
// A channel with no uploads (404 playlist) is fine. Other per-channel errors are
// collected in failed. Quota or credential errors stop everything and are returned.
func (c *Client) fetchRecentUploads(ctx context.Context, channels []store.Channel, progress func(done int)) (failed []string, fatal error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		done int
		sem  = make(chan struct{}, 8)
	)
	finish := func(id string, err error) {
		mu.Lock()
		defer mu.Unlock()
		var apiErr *APIError
		switch {
		case err == nil:
		case errors.As(err, &apiErr) && apiErr.NotFound():
		case errors.As(err, &apiErr) && (apiErr.QuotaExceeded() || apiErr.Unauthorized()):
			if fatal == nil {
				fatal = err
				cancel()
			}
		case ctx.Err() != nil: // cancelled because another request failed fatally
		default:
			failed = append(failed, id)
		}
		done++
		progress(done)
	}
	for i := range channels {
		ch := &channels[i]
		if ch.UploadsPlaylist == "" {
			finish(ch.ID, nil)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				finish(ch.ID, ctx.Err())
				return
			}
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
			err := c.do(ctx, http.MethodGet, "playlistItems", q, &res)
			if err == nil {
				for _, it := range res.Items {
					ch.RecentTitles = append(ch.RecentTitles, it.Snippet.Title)
					if it.Snippet.PublishedAt.After(ch.LastUpload) {
						ch.LastUpload = it.Snippet.PublishedAt
					}
				}
			}
			finish(ch.ID, err)
		}()
	}
	wg.Wait()
	return failed, fatal
}

// Unsubscribe deletes a subscription by its subscription ID (not channel ID).
// A subscription that's already gone (e.g. removed on youtube.com) counts as success.
func (c *Client) Unsubscribe(ctx context.Context, subscriptionID string) error {
	err := c.do(ctx, http.MethodDelete, "subscriptions", url.Values{"id": {subscriptionID}}, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.NotFound() {
		return nil
	}
	return err
}
