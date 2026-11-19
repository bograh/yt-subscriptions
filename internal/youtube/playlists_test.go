package youtube

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePlaylistsAPI serves playlists PL0..PL{n-1} in two pages; items(w, r, id)
// answers playlistItems requests.
func fakePlaylistsAPI(t *testing.T, n int, items func(w http.ResponseWriter, r *http.Request, id string)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case strings.HasSuffix(r.URL.Path, "/playlists") && r.Method == http.MethodGet:
			if q.Get("mine") != "true" || q.Get("part") != "snippet,status" {
				t.Errorf("playlists query = %v", q)
			}
			start, end, next := 0, (n+1)/2, "page2"
			if q.Get("pageToken") == "page2" {
				start, end, next = (n+1)/2, n, ""
			}
			var list []map[string]any
			for i := start; i < end; i++ {
				thumbs := map[string]any{"default": map[string]string{"url": fmt.Sprintf("default%d", i)}}
				if i%2 == 0 {
					thumbs["medium"] = map[string]string{"url": fmt.Sprintf("medium%d", i)}
				}
				list = append(list, map[string]any{
					"id": fmt.Sprintf("PL%d", i),
					"snippet": map[string]any{
						"title":       fmt.Sprintf("Playlist %d", i),
						"description": "desc",
						"publishedAt": "2025-01-02T03:04:05Z",
						"thumbnails":  thumbs,
					},
					"status": map[string]string{"privacyStatus": "unlisted"},
				})
			}
			json.NewEncoder(w).Encode(map[string]any{"items": list, "nextPageToken": next})
		case strings.HasSuffix(r.URL.Path, "/playlistItems") && r.Method == http.MethodGet:
			items(w, r, q.Get("playlistId"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Base: srv.URL + "/"}
}

func item(id, video, owner string, pos int) map[string]any {
	return map[string]any{
		"id":             id,
		"contentDetails": map[string]string{"videoId": video},
		"snippet": map[string]any{
			"title":                  "Title " + video,
			"videoOwnerChannelId":    owner,
			"videoOwnerChannelTitle": strings.ToUpper(owner),
			"position":               pos,
			"publishedAt":            "2026-03-04T05:06:07Z",
		},
	}
}

// twoPageItems serves two pages of items, deliberately out of position order.
func twoPageItems(w http.ResponseWriter, r *http.Request, id string) {
	if r.URL.Query().Get("pageToken") == "" {
		json.NewEncoder(w).Encode(map[string]any{
			"nextPageToken": "p2",
			"items": []any{
				item(id+"-i2", "v2", "UCa", 2),
				item(id+"-i0", "v0", "UCb", 0),
			},
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"items": []any{
		item(id+"-i3", "v3", "", 3), // "Deleted video": no owner
		item(id+"-i1", "v1", "UCme", 1),
	}})
}

func TestFetchPlaylistsPagingAndMapping(t *testing.T) {
	c := fakePlaylistsAPI(t, 3, twoPageItems)
	var mu sync.Mutex
	var calls [][2]int
	pls, err := c.FetchPlaylists(t.Context(), func(done, total int) {
		mu.Lock()
		calls = append(calls, [2]int{done, total})
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pls) != 3 {
		t.Fatalf("got %d playlists, want 3", len(pls))
	}
	for i, p := range pls {
		if p.ID != fmt.Sprintf("PL%d", i) || p.Title != fmt.Sprintf("Playlist %d", i) {
			t.Errorf("playlist %d = %s %q, out of order or mis-mapped", i, p.ID, p.Title)
		}
		if p.Description != "desc" || p.Privacy != "unlisted" ||
			!p.PublishedAt.Equal(time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)) {
			t.Errorf("playlist %d fields = %+v", i, p)
		}
		wantThumb := fmt.Sprintf("default%d", i)
		if i%2 == 0 {
			wantThumb = fmt.Sprintf("medium%d", i)
		}
		if p.Thumbnail != wantThumb {
			t.Errorf("playlist %d thumbnail = %q, want %q", i, p.Thumbnail, wantThumb)
		}
		if len(p.Items) != 4 {
			t.Fatalf("playlist %d has %d items, want 4", i, len(p.Items))
		}
		for j, it := range p.Items {
			if it.Position != j || it.VideoID != fmt.Sprintf("v%d", j) || it.ID != fmt.Sprintf("%s-i%d", p.ID, j) {
				t.Errorf("playlist %d item %d = %+v", i, j, it)
			}
		}
		it := p.Items[1]
		if it.Title != "Title v1" || it.ChannelID != "UCme" || it.ChannelTitle != "UCME" ||
			!it.AddedAt.Equal(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)) {
			t.Errorf("item fields = %+v", it)
		}
	}
	if len(calls) != 4 || calls[0] != [2]int{0, 3} || calls[3] != [2]int{3, 3} {
		t.Errorf("progress calls = %v", calls)
	}
}

func TestFetchPlaylistsUnavailable(t *testing.T) {
	c := fakePlaylistsAPI(t, 1, func(w http.ResponseWriter, r *http.Request, id string) {
		own := item("own", "vOwn", "UCme", 0)
		own["status"] = map[string]string{"privacyStatus": "private"} // the user's own private video
		deleted := item("del", "vDel", "", 1)
		deleted["snippet"].(map[string]any)["title"] = "Deleted video"
		private := item("priv", "vPriv", "", 2)
		private["snippet"].(map[string]any)["title"] = "Private video"
		private["status"] = map[string]string{"privacyStatus": "private"}
		json.NewEncoder(w).Encode(map[string]any{"items": []any{own, deleted, private}})
	})
	pls, err := c.FetchPlaylists(t.Context(), func(int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, it := range pls[0].Items {
		got[it.ID] = it.Unavailable
	}
	want := map[string]bool{"own": false, "del": true, "priv": true}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("item %s Unavailable = %v, want %v", id, got[id], w)
		}
	}
}

func TestFetchPlaylistsQuotaAborts(t *testing.T) {
	c := fakePlaylistsAPI(t, 20, func(w http.ResponseWriter, r *http.Request, id string) {
		if id == "PL3" {
			apiError(w, http.StatusForbidden, "quotaExceeded")
			return
		}
		twoPageItems(w, r, id)
	})
	pls, err := c.FetchPlaylists(t.Context(), func(int, int) {})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.QuotaExceeded() {
		t.Fatalf("err = %v, want quota exceeded APIError", err)
	}
	if pls != nil {
		t.Errorf("got %d playlists alongside the error", len(pls))
	}
}

func TestFetchPlaylistsOtherErrorAborts(t *testing.T) {
	c := fakePlaylistsAPI(t, 4, func(w http.ResponseWriter, r *http.Request, id string) {
		if id == "PL2" {
			apiError(w, http.StatusInternalServerError, "backendError")
			return
		}
		twoPageItems(w, r, id)
	})
	if _, err := c.FetchPlaylists(t.Context(), func(int, int) {}); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestFetchPlaylistsItemsNotFound(t *testing.T) {
	c := fakePlaylistsAPI(t, 2, func(w http.ResponseWriter, r *http.Request, id string) {
		if id == "PL1" {
			apiError(w, http.StatusNotFound, "playlistNotFound")
			return
		}
		twoPageItems(w, r, id)
	})
	pls, err := c.FetchPlaylists(t.Context(), func(int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(pls) != 2 || len(pls[0].Items) != 4 || len(pls[1].Items) != 0 {
		t.Fatalf("got %+v", pls)
	}
}

type recorded struct {
	method, path, query, contentType, body string
}

// writeAPI records each request and answers with status.
func writeAPI(t *testing.T, status int) (*Client, *[]recorded) {
	t.Helper()
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs = append(reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"), string(b)})
		if status >= 300 {
			apiError(w, status, "notFound")
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Base: srv.URL + "/"}, &reqs
}

func TestDeleteAndRemoveNotFoundIsSuccess(t *testing.T) {
	c, reqs := writeAPI(t, http.StatusNotFound)
	if err := c.DeletePlaylist(t.Context(), "PL1"); err != nil {
		t.Errorf("DeletePlaylist: %v", err)
	}
	if err := c.RemovePlaylistItem(t.Context(), "item1"); err != nil {
		t.Errorf("RemovePlaylistItem: %v", err)
	}
	want := []recorded{
		{method: "DELETE", path: "/playlists", query: "id=PL1"},
		{method: "DELETE", path: "/playlistItems", query: "id=item1"},
	}
	if len(*reqs) != 2 || (*reqs)[0] != want[0] || (*reqs)[1] != want[1] {
		t.Errorf("requests = %+v", *reqs)
	}

	c, _ = writeAPI(t, http.StatusForbidden)
	if err := c.DeletePlaylist(t.Context(), "PL1"); err == nil {
		t.Error("DeletePlaylist: want error on 403")
	}
}

func TestAddPlaylistItem(t *testing.T) {
	c, reqs := writeAPI(t, http.StatusOK)
	if err := c.AddPlaylistItem(t.Context(), "PL1", "vid1"); err != nil {
		t.Fatal(err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("requests = %+v", *reqs)
	}
	r := (*reqs)[0]
	if r.method != "POST" || r.path != "/playlistItems" || r.query != "part=snippet" || r.contentType != "application/json" {
		t.Errorf("request = %+v", r)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.body), &body); err != nil {
		t.Fatal(err)
	}
	want := `{"snippet":{"playlistId":"PL1","resourceId":{"kind":"youtube#video","videoId":"vid1"}}}`
	got, _ := json.Marshal(body)
	if string(got) != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestOnCost(t *testing.T) {
	c, _ := writeAPI(t, http.StatusOK)
	var costs []int
	c.OnCost = func(u int) { costs = append(costs, u) }
	c.do(t.Context(), http.MethodGet, "playlists", nil, nil)
	c.AddPlaylistItem(t.Context(), "PL1", "v")
	c.DeletePlaylist(t.Context(), "PL1")
	if len(costs) != 3 || costs[0] != 1 || costs[1] != 50 || costs[2] != 50 {
		t.Errorf("costs = %v, want [1 50 50]", costs)
	}
}
