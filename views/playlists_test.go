package views

import (
	"testing"

	"yt-subscriptions/internal/store"
)

func TestPlaylistListAndStats(t *testing.T) {
	target := store.Playlist{ID: "PLb", Title: "Golang", Items: []store.PlaylistItem{{VideoID: "v1"}}}
	src := store.Playlist{
		ID: "PLa", Title: "Go talks",
		Items: []store.PlaylistItem{
			{VideoID: "v1"}, {VideoID: "v2"}, {VideoID: "v2"}, {VideoID: "gone", Unavailable: true}, {VideoID: "v3"},
		},
		Unavailable: 1,
		Duplicates:  []store.PlaylistItem{{VideoID: "v2"}},
		Analysis:    &store.PlaylistAnalysis{Topic: "Programming", Verdict: store.PlaylistMerge, MergeWith: "PLb"},
	}
	kept := store.Playlist{ID: "PLc", Title: "Old", Analysis: &store.PlaylistAnalysis{Topic: "Music", Verdict: store.PlaylistDelete}, Decision: store.PlaylistKeep}
	empty := store.Playlist{ID: "PLd", Title: "Empty"}
	all := []store.Playlist{src, target, kept, empty}

	l := BuildPlaylistList(all, "verdict", "", "")
	if l.Groups[0].Name == "" || l.Shown != 4 {
		t.Fatalf("list: %+v", l)
	}
	if tgt, ok := l.MergeTarget(src); !ok || tgt.ID != "PLb" {
		t.Fatal("merge target not resolved")
	}
	if n := MergeMoves(src, target); n != 2 { // v2, v3 (v1 already there, gone skipped, dup counted once)
		t.Fatalf("MergeMoves = %d, want 2", n)
	}
	if l := BuildPlaylistList(all, "topic", "problems", ""); l.Shown != 3 { // src (problems) + the two empty ones
		t.Fatalf("problems filter shows %d, want 3", l.Shown)
	}
	if l := BuildPlaylistList(all, "topic", "delete", ""); l.Shown != 0 {
		t.Fatal("a kept playlist must not show under Delete")
	}

	s := ComputePlaylistStats(store.PlaylistState{Playlists: all, QuotaUsed: 9_900}, 10_000)
	if s.Merge != 1 || s.Keep != 1 || s.Unanalyzed != 2 || s.Cleanable() != 2 || s.Empty != 2 {
		t.Fatalf("stats: %+v", s)
	}
	if got := PlaylistProblems(src); got != "1 unavailable · 1 duplicate" {
		t.Fatalf("PlaylistProblems = %q", got)
	}
	if got := s.CleanAllNote(); got != "Removes 1 unavailable video and 1 duplicate across your playlists for about 100 quota units." {
		t.Fatalf("CleanAllNote = %q", got)
	}
}
