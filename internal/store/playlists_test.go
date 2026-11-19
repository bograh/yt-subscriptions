package store

import (
	"context"
	"testing"
	"time"
)

func TestPlaylistFactsAndPersistence(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	pls := []Playlist{
		{ID: "PLa", Title: "Go talks", Items: []PlaylistItem{
			{ID: "a1", VideoID: "v1", Position: 0, AddedAt: day(1)},
			{ID: "a2", VideoID: "v2", Position: 1, AddedAt: day(5)},
			{ID: "a3", VideoID: "v1", Position: 2, AddedAt: day(3)}, // duplicate of a1
			{ID: "a4", VideoID: "gone", Position: 3, Unavailable: true},
		}},
		{ID: "PLb", Title: "Golang", Items: []PlaylistItem{
			{ID: "b1", VideoID: "v1", Position: 0},
			{ID: "b2", VideoID: "v2", Position: 1},
			{ID: "b3", VideoID: "v3", Position: 2},
		}},
		{ID: "PLc", Title: "Cooking", Items: []PlaylistItem{{ID: "c1", VideoID: "v9"}}},
	}
	if err := s.ReplacePlaylists(ctx, pls); err != nil {
		t.Fatal(err)
	}
	s.SavePlaylistAnalyses(ctx, map[string]PlaylistAnalysis{
		"PLa": {Topic: "Programming", Verdict: PlaylistMerge, MergeWith: "PLb", AnalyzedAt: time.Now(), Basis: "old"},
	})
	s.SetPlaylistDecision(ctx, "PLc", PlaylistKeep)

	ps, err := s.Playlists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.Playlists) != 3 || ps.Playlists[0].ID != "PLa" || ps.SyncedAt.IsZero() {
		t.Fatalf("order or sync time wrong: %+v", ps)
	}
	a := ps.Playlists[0]
	if len(a.Items) != 4 || a.Unavailable != 1 || len(a.Duplicates) != 1 || a.Duplicates[0].ID != "a3" {
		t.Fatalf("PLa facts: items=%d unavailable=%d dups=%+v", len(a.Items), a.Unavailable, a.Duplicates)
	}
	if !a.LastAdded.Equal(day(5)) {
		t.Fatalf("LastAdded = %v", a.LastAdded)
	}
	if len(a.Overlaps) != 1 || a.Overlaps[0].PlaylistID != "PLb" || a.Overlaps[0].Shared != 2 {
		t.Fatalf("PLa overlaps: %+v", a.Overlaps)
	}
	if a.Analysis == nil || a.Analysis.MergeWith != "PLb" || !a.Analysis.Outdated {
		t.Fatalf("analysis: %+v", a.Analysis)
	}
	if c := ps.Playlists[2]; c.Verdict() != PlaylistKeep || len(c.Overlaps) != 0 {
		t.Fatalf("PLc: %+v", c)
	}

	// Cleanup + delete, then a re-sync keeps analyses and decisions.
	if err := s.RemovePlaylistItems(ctx, []string{"a3", "a4"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePlaylist(ctx, "PLb"); err != nil {
		t.Fatal(err)
	}
	ps, _ = s.Playlists(ctx)
	if len(ps.Playlists) != 2 || len(ps.Playlists[0].Items) != 2 || ps.Playlists[0].Unavailable != 0 {
		t.Fatalf("after cleanup: %+v", ps.Playlists)
	}
	s.ReplacePlaylists(ctx, pls[:1])
	ps, _ = s.Playlists(ctx)
	if ps.Playlists[0].Analysis == nil {
		t.Fatal("analysis should survive a re-sync")
	}
}
