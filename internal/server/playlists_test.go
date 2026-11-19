package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"yt-subscriptions/internal/store"
)

// fakeYouTube is an in-memory playlists API: list, list items, delete, insert.
type fakeYouTube struct {
	mu        sync.Mutex
	playlists []string            // order
	titles    map[string]string   // playlist ID -> title
	items     map[string][]fakeIt // playlist ID -> items
	nextID    int
}

type fakeIt struct{ id, video string }

func (f *fakeYouTube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/playlists"):
		var items []map[string]any
		for _, id := range f.playlists {
			items = append(items, map[string]any{"id": id, "snippet": map[string]any{"title": f.titles[id]}, "status": map[string]any{"privacyStatus": "private"}})
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/playlistItems"):
		var items []map[string]any
		for i, it := range f.items[q.Get("playlistId")] {
			owner := "UCowner"
			if strings.HasPrefix(it.video, "gone") {
				owner = ""
			}
			items = append(items, map[string]any{
				"id":             it.id,
				"snippet":        map[string]any{"title": it.video, "position": i, "videoOwnerChannelId": owner},
				"contentDetails": map[string]any{"videoId": it.video},
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/playlistItems"):
		for pid, its := range f.items {
			for i, it := range its {
				if it.id == q.Get("id") {
					f.items[pid] = append(its[:i:i], its[i+1:]...)
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/playlists"):
		id := q.Get("id")
		for i, p := range f.playlists {
			if p == id {
				f.playlists = append(f.playlists[:i:i], f.playlists[i+1:]...)
			}
		}
		delete(f.items, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/playlistItems"):
		var body struct {
			Snippet struct {
				PlaylistID string `json:"playlistId"`
				ResourceID struct {
					VideoID string `json:"videoId"`
				} `json:"resourceId"`
			} `json:"snippet"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		f.nextID++
		pid := body.Snippet.PlaylistID
		f.items[pid] = append(f.items[pid], fakeIt{fmt.Sprintf("new%d", f.nextID), body.Snippet.ResourceID.VideoID})
		w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func TestPlaylistSyncCleanMergeDelete(t *testing.T) {
	yt := &fakeYouTube{
		playlists: []string{"PLa", "PLb", "PLc"},
		titles:    map[string]string{"PLa": "Go talks", "PLb": "Golang", "PLc": "Old stuff"},
		items: map[string][]fakeIt{
			"PLa": {{"a1", "v1"}, {"a2", "v2"}, {"a3", "v2"}, {"a4", "gone1"}},
			"PLb": {{"b1", "v1"}},
			"PLc": {{"c1", "v9"}},
		},
	}
	srv := httptest.NewServer(yt)
	defer srv.Close()

	ctx := context.Background()
	a := newTestServer(t, nil)
	a.ytBase = srv.URL + "/"
	connect(t, a)
	st := a.store
	post := func(h http.HandlerFunc, path, id string) { runJob(t, a, h, path, id) }

	post(a.handlePlaylistSync, "/playlists/sync", "")
	ps, _ := st.Playlists(ctx)
	if len(ps.Playlists) != 3 || ps.Playlists[0].Unavailable != 1 || len(ps.Playlists[0].Duplicates) != 1 {
		t.Fatalf("after sync: %+v", ps.Playlists[0])
	}

	// Clean up PLa: removes the unavailable item and the duplicate, in YouTube and locally.
	post(a.handlePlaylistClean, "/playlists/PLa/clean", "PLa")
	if got := len(yt.items["PLa"]); got != 2 {
		t.Fatalf("PLa on YouTube has %d items, want 2", got)
	}
	ps, _ = st.Playlists(ctx)
	if ps.Playlists[0].Unavailable != 0 || len(ps.Playlists[0].Duplicates) != 0 || ps.QuotaUsed < 100 {
		t.Fatalf("after clean: %+v quota=%d", ps.Playlists[0], ps.QuotaUsed)
	}

	// Merge PLa into PLb as the AI suggested: copies v2 (v1 is already there), deletes PLa.
	st.SavePlaylistAnalyses(ctx, map[string]store.PlaylistAnalysis{
		"PLa": {Topic: "Programming", Verdict: store.PlaylistMerge, MergeWith: "PLb", AnalyzedAt: time.Now()},
	})
	post(a.handlePlaylistMerge, "/playlists/PLa/merge", "PLa")
	if _, ok := yt.items["PLa"]; ok {
		t.Fatal("PLa should be deleted on YouTube")
	}
	if got := yt.items["PLb"]; len(got) != 2 || got[1].video != "v2" {
		t.Fatalf("PLb on YouTube = %+v, want v1 + v2", got)
	}
	ps, _ = st.Playlists(ctx)
	if len(ps.Playlists) != 2 || len(ps.Playlists[0].Items) != 2 {
		t.Fatalf("after merge re-sync: %+v", ps.Playlists)
	}

	// Delete PLc directly.
	req := httptest.NewRequest(http.MethodDelete, "/playlists/PLc", nil)
	req.SetPathValue("id", "PLc")
	rec := httptest.NewRecorder()
	a.handlePlaylistDelete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	ps, _ = st.Playlists(ctx)
	if len(ps.Playlists) != 1 || len(yt.playlists) != 1 {
		t.Fatalf("after delete: local %d, youtube %d", len(ps.Playlists), len(yt.playlists))
	}
}

func TestPlaylistBulkActions(t *testing.T) {
	yt := &fakeYouTube{
		playlists: []string{"PLa", "PLb", "PLc"},
		titles:    map[string]string{"PLa": "A", "PLb": "B", "PLc": "C"},
		items: map[string][]fakeIt{
			"PLa": {{"a1", "v1"}, {"a2", "v1"}},
			"PLb": {{"b1", "gone1"}, {"b2", "v2"}},
			"PLc": {{"c1", "v3"}},
		},
	}
	srv := httptest.NewServer(yt)
	defer srv.Close()

	ctx := context.Background()
	s := newTestServer(t, nil)
	s.ytBase = srv.URL + "/"
	connect(t, s)
	runJob(t, s, s.handlePlaylistSync, "/playlists/sync", "")

	postForm(t, s, s.handlePlaylistBulk, url.Values{"action": {"keep"}, "ids": {"PLc"}})
	ps, _ := s.store.Playlists(ctx)
	if ps.Playlists[2].Decision != store.PlaylistKeep {
		t.Fatalf("PLc not kept: %+v", ps.Playlists[2])
	}

	// Clean only the selected playlists: PLa's duplicate goes, PLb's unavailable video stays.
	postForm(t, s, s.handlePlaylistBulk, url.Values{"action": {"clean"}, "ids": {"PLa"}})
	if len(yt.items["PLa"]) != 1 || len(yt.items["PLb"]) != 2 {
		t.Fatalf("after clean: PLa %v, PLb %v", yt.items["PLa"], yt.items["PLb"])
	}

	// Delete two, with quota for only one: the second is left and a warning explains why.
	ps, _ = s.store.Playlists(ctx)
	s.quotaLimit = ps.QuotaUsed + 50
	postForm(t, s, s.handlePlaylistBulk, url.Values{"action": {"delete"}, "ids": {"PLa", "PLb"}})
	if len(yt.playlists) != 2 || !strings.Contains(s.lastWarn, "1 are left") {
		t.Fatalf("youtube has %v, warning %q", yt.playlists, s.lastWarn)
	}
	ps, _ = s.store.Playlists(ctx)
	if len(ps.Playlists) != 2 {
		t.Fatalf("local playlists: %d, want 2", len(ps.Playlists))
	}
}
