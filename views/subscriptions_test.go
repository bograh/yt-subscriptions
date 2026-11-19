package views

import (
	"slices"
	"sort"
	"testing"

	"yt-subscriptions/internal/store"
)

func TestNaturalLess(t *testing.T) {
	got := []string{"Channel 15", "channel 3", "Channel 100", "Alpha", "Channel 03b", "beta"}
	sort.Slice(got, func(i, j int) bool { return naturalLess(got[i], got[j]) })
	want := []string{"Alpha", "beta", "channel 3", "Channel 03b", "Channel 15", "Channel 100"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 10000: "10,000", 1234567: "1,234,567", -2500: "-2,500"} {
		if got := Thousands(n); got != want {
			t.Errorf("Thousands(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestBuildListUsesDecisions(t *testing.T) {
	chs := []store.Channel{
		{ID: "a", Title: "A", Analysis: &store.Analysis{Verdict: store.VerdictUnsubscribe, Category: "Gaming"}},
		{ID: "b", Title: "B", Analysis: &store.Analysis{Verdict: store.VerdictUnsubscribe, Category: "Gaming"}, Decision: store.VerdictKeep},
		{ID: "c", Title: "C"},
	}
	l := BuildList(chs, "verdict", "unsubscribe", "")
	if l.Shown != 1 || l.Groups[0].Channels[0].ID != "a" {
		t.Fatalf("kept channel should not appear under Unsubscribe: %+v", l)
	}
	if l := BuildList(chs, "category", "unanalyzed", ""); l.Shown != 1 || l.Groups[0].Name != "Not analyzed yet" {
		t.Fatalf("unanalyzed filter: %+v", l)
	}
	s := ComputeStats(store.State{Channels: chs, QuotaUsed: 9_960}, 10_000)
	if s.Keep != 1 || s.Unsubscribe != 1 || s.Unanalyzed != 1 || s.QuotaRemaining() != 40 {
		t.Fatalf("stats: %+v", s)
	}
}
