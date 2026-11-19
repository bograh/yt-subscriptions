package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

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
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.SaveToken(&oauth2.Token{AccessToken: "a", Expiry: time.Now().Add(time.Hour)})
	a := &App{ctx: ctx, store: st, oauth: &oauth2.Config{}, quotaLimit: 10_000, ytBase: srv.URL + "/"}
	post := func(h http.HandlerFunc, path, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if id != "" {
			req.SetPathValue("id", id)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		a.jobs.Wait()
		if a.lastErr != "" {
			t.Fatalf("%s: job failed: %s", path, a.lastErr)
		}
		return rec
	}

	post(a.handlePlaylistSync, "/playlists/sync", "")
	ps, _ := st.Playlists(ctx)
	if len(ps.Playlists) != 3 || ps.Playlists[0].Unavailable != 1 || len(ps.Playlists[0].Duplicates) != 1 {
		t.Fatalf("after sync: %+v", ps.Playlists[0])
	}

	// Clean up PLa: removes the unavailable item and the duplicate, in YouTube and locally.
	post(a.handlePlaylistClean("PLa"), "/playlists/PLa/clean", "PLa")
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

type fakePlaylistAnalyzer struct {
	calls int
	mu    sync.Mutex
}

func (f *fakePlaylistAnalyzer) AnalyzePlaylists(_ context.Context, _ string, _, batch []store.Playlist) (map[string]store.PlaylistAnalysis, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	out := map[string]store.PlaylistAnalysis{}
	for i, p := range batch {
		if i == 0 && len(batch) > 1 {
			continue // skip one; the job should ask again
		}
		out[p.ID] = store.PlaylistAnalysis{Topic: "Programming", Verdict: store.PlaylistKeep, AnalyzedAt: time.Now()}
	}
	return out, nil
}

func TestPlaylistAnalysisJob(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(t.TempDir())
	var pls []store.Playlist
	for i := range 25 {
		pls = append(pls, store.Playlist{ID: fmt.Sprintf("PL%02d", i), Title: fmt.Sprint(i)})
	}
	st.ReplacePlaylists(ctx, pls)
	st.SetPlaylistDecision(ctx, "PL00", store.PlaylistKeep)
	f := &fakePlaylistAnalyzer{}
	a := &App{ctx: ctx, store: st, quotaLimit: 10_000, testPlaylistAnalyzer: f, retryBackoff: time.Millisecond}

	ps, _ := st.Playlists(ctx)
	var todo []store.Playlist
	for _, p := range ps.Playlists {
		if p.Decision == "" {
			todo = append(todo, p)
		}
	}
	if err := a.runPlaylistAnalysis(ctx, ps, todo, func(int, int) {}); err != nil {
		t.Fatal(err)
	}
	ps, _ = st.Playlists(ctx)
	for _, p := range ps.Playlists[1:] {
		if p.Analysis == nil || p.Analysis.Basis != ps.Basis {
			t.Fatalf("%s not analyzed", p.ID)
		}
	}
	if ps.Playlists[0].Analysis != nil {
		t.Fatal("decided playlist should not be sent")
	}
	if f.calls != 4 { // batches of 20 + 4, each with one follow-up
		t.Fatalf("calls = %d, want 4", f.calls)
	}
}
