package youtube

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"yt-subscriptions/internal/store"
)

// FetchPlaylists lists all of the user's own playlists with every item in each,
// in the API's playlist order and with items in position order.
//
// progress(done, total) is called once with done=0 after the playlists are listed,
// then after each playlist's items are fetched. A playlist whose items 404 gets no
// items. Any other error aborts the whole fetch and is returned: partial data about
// the user's own playlists would be misleading.
func (c *Client) FetchPlaylists(ctx context.Context, progress func(done, total int)) ([]store.Playlist, error) {
	var playlists []store.Playlist
	pageToken := ""
	for {
		q := url.Values{"part": {"snippet,status"}, "mine": {"true"}, "maxResults": {"50"}}
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
				} `json:"snippet"`
				Status struct {
					PrivacyStatus string `json:"privacyStatus"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := c.do(ctx, http.MethodGet, "playlists", q, &res); err != nil {
			return nil, err
		}
		for _, it := range res.Items {
			thumb := it.Snippet.Thumbnails.Medium.URL
			if thumb == "" {
				thumb = it.Snippet.Thumbnails.Default.URL
			}
			playlists = append(playlists, store.Playlist{
				ID:          it.ID,
				Title:       it.Snippet.Title,
				Description: it.Snippet.Description,
				Thumbnail:   thumb,
				Privacy:     it.Status.PrivacyStatus,
				PublishedAt: it.Snippet.PublishedAt,
			})
		}
		if res.NextPageToken == "" {
			break
		}
		pageToken = res.NextPageToken
	}

	total := len(playlists)
	progress(0, total)
	if err := c.fetchAllPlaylistItems(ctx, playlists, func(done int) { progress(done, total) }); err != nil {
		return nil, err
	}
	return playlists, ctx.Err()
}

// fetchAllPlaylistItems fills Items for every playlist, up to 8 playlists at once.
// The first error other than a 404 cancels the rest and is returned.
func (c *Client) fetchAllPlaylistItems(ctx context.Context, playlists []store.Playlist, progress func(done int)) (fatal error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		done int
		sem  = make(chan struct{}, 8)
	)
	finish := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		var apiErr *APIError
		switch {
		case err == nil:
		case errors.As(err, &apiErr) && apiErr.NotFound():
		case fatal == nil:
			fatal = err
			cancel()
		}
		done++
		progress(done)
	}
	for i := range playlists {
		pl := &playlists[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				finish(ctx.Err())
				return
			}
			defer func() { <-sem }()
			items, err := c.fetchPlaylistItems(ctx, pl.ID)
			if err == nil {
				pl.Items = items
			}
			finish(err)
		}()
	}
	wg.Wait()
	return fatal
}

// fetchPlaylistItems returns every item of one playlist, sorted by position.
func (c *Client) fetchPlaylistItems(ctx context.Context, playlistID string) ([]store.PlaylistItem, error) {
	var items []store.PlaylistItem
	pageToken := ""
	for {
		q := url.Values{"part": {"snippet,contentDetails,status"}, "playlistId": {playlistID}, "maxResults": {"50"}}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var res struct {
			NextPageToken string `json:"nextPageToken"`
			Items         []struct {
				ID      string `json:"id"`
				Snippet struct {
					Title                  string    `json:"title"`
					VideoOwnerChannelID    string    `json:"videoOwnerChannelId"`
					VideoOwnerChannelTitle string    `json:"videoOwnerChannelTitle"`
					Position               int       `json:"position"`
					PublishedAt            time.Time `json:"publishedAt"`
				} `json:"snippet"`
				ContentDetails struct {
					VideoID string `json:"videoId"`
				} `json:"contentDetails"`
			} `json:"items"`
		}
		if err := c.do(ctx, http.MethodGet, "playlistItems", q, &res); err != nil {
			return nil, err
		}
		for _, it := range res.Items {
			items = append(items, store.PlaylistItem{
				ID:           it.ID,
				VideoID:      it.ContentDetails.VideoID,
				Title:        it.Snippet.Title,
				ChannelID:    it.Snippet.VideoOwnerChannelID,
				ChannelTitle: it.Snippet.VideoOwnerChannelTitle,
				Position:     it.Snippet.Position,
				AddedAt:      it.Snippet.PublishedAt,
				// Deleted videos, and videos made private by someone else, come back
				// with no owner. The user's own private videos still have one.
				Unavailable: it.Snippet.VideoOwnerChannelID == "",
			})
		}
		if res.NextPageToken == "" {
			break
		}
		pageToken = res.NextPageToken
	}
	slices.SortStableFunc(items, func(a, b store.PlaylistItem) int { return a.Position - b.Position })
	return items, nil
}

// DeletePlaylist deletes one of the user's playlists. Already gone counts as success.
func (c *Client) DeletePlaylist(ctx context.Context, playlistID string) error {
	return ignoreNotFound(c.do(ctx, http.MethodDelete, "playlists", url.Values{"id": {playlistID}}, nil))
}

// RemovePlaylistItem removes an entry by its playlistItem ID (not the video ID).
// Already gone counts as success.
func (c *Client) RemovePlaylistItem(ctx context.Context, itemID string) error {
	return ignoreNotFound(c.do(ctx, http.MethodDelete, "playlistItems", url.Values{"id": {itemID}}, nil))
}

// AddPlaylistItem appends a video to the end of a playlist.
func (c *Client) AddPlaylistItem(ctx context.Context, playlistID, videoID string) error {
	type resourceID struct {
		Kind    string `json:"kind"`
		VideoID string `json:"videoId"`
	}
	type snippet struct {
		PlaylistID string     `json:"playlistId"`
		ResourceID resourceID `json:"resourceId"`
	}
	body := struct {
		Snippet snippet `json:"snippet"`
	}{snippet{PlaylistID: playlistID, ResourceID: resourceID{Kind: "youtube#video", VideoID: videoID}}}
	return c.send(ctx, http.MethodPost, "playlistItems", url.Values{"part": {"snippet"}}, body, nil)
}

func ignoreNotFound(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.NotFound() {
		return nil
	}
	return err
}
