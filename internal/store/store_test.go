package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestAnalysesSurviveResyncAndUnsubscribe(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	chs := []Channel{
		{ID: "UC1", SubscriptionID: "s1", Title: "Beta", Topics: []string{"Music"}, LastUpload: time.Unix(1700000000, 0)},
		{ID: "UC2", SubscriptionID: "s2", Title: "alpha"},
	}
	if err := s.ReplaceChannels(ctx, chs); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAnalyses(ctx, map[string]Analysis{
		"UC1": {Category: "Music", ContentType: "Vlogs", Verdict: VerdictKeep, Reason: "r", AnalyzedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	// Unsubscribe, then re-sync with the channel back in the list.
	if err := s.DeleteChannel(ctx, "UC1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceChannels(ctx, chs); err != nil {
		t.Fatal(err)
	}

	st, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Channels) != 2 || st.Channels[0].Title != "alpha" {
		t.Fatalf("unexpected channels/order: %+v", st.Channels)
	}
	beta := st.Channels[1]
	if beta.Analysis == nil || beta.Analysis.Verdict != VerdictKeep {
		t.Fatalf("analysis was lost: %+v", beta.Analysis)
	}
	if len(beta.Topics) != 1 || !beta.LastUpload.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("channel fields not round-tripped: %+v", beta)
	}
	if st.SyncedAt.IsZero() {
		t.Fatal("synced_at not set")
	}
	if st.Channels[0].Analysis != nil {
		t.Fatal("UC2 should be unanalyzed")
	}
}

func TestWatchStatsAndSettings(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceChannels(ctx, []Channel{{ID: "UC1", SubscriptionID: "s1", Title: "A"}}); err != nil {
		t.Fatal(err)
	}
	matched, err := s.ImportWatchStats(ctx, map[string]WatchStat{
		"UC1": {Count: 3, LastWatched: time.Unix(1700000000, 0)},
		"UC9": {Count: 1},
	})
	if err != nil || matched != 1 {
		t.Fatalf("matched=%d err=%v", matched, err)
	}
	if err := s.SetInterests(ctx, "go"); err != nil {
		t.Fatal(err)
	}
	if s.Token() != nil {
		t.Fatal("expected no token")
	}
	if err := s.SaveToken(&oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Snapshot(ctx)
	if !st.HistoryImported || st.Interests != "go" || st.Channels[0].WatchCount != 3 {
		t.Fatalf("unexpected state: %+v", st)
	}
	if tok := s.Token(); tok == nil || tok.RefreshToken != "r" {
		t.Fatalf("token not saved: %+v", tok)
	}
	if id, _ := s.SubscriptionID(ctx, "UC1"); id != "s1" {
		t.Fatalf("subscription id = %q", id)
	}
}

func TestMigratesJSONFiles(t *testing.T) {
	dir := t.TempDir()
	state := `{"channels":[{"id":"UC1","subscription_id":"s1","title":"A",
		"analysis":{"category":"Gaming","content_type":"Vlogs","verdict":"unsubscribe","reason":"x","analyzed_at":"2026-01-01T00:00:00Z"}}],
		"interests":"cooking","synced_at":"2026-01-02T00:00:00Z","history_imported":true,
		"watch":{"UC1":{"count":5,"last_watched":"2026-01-01T00:00:00Z"}}}`
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(state), 0o600)
	os.WriteFile(filepath.Join(dir, "token.json"), []byte(`{"access_token":"a","refresh_token":"r"}`), 0o600)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Channels) != 1 || st.Channels[0].Analysis == nil || st.Channels[0].Analysis.Verdict != VerdictUnsubscribe ||
		st.Channels[0].WatchCount != 5 || st.Interests != "cooking" || !st.HistoryImported || st.SyncedAt.Year() != 2026 {
		t.Fatalf("migration incomplete: %+v", st)
	}
	if s.Token() == nil {
		t.Fatal("token not migrated")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.migrated")); err != nil {
		t.Fatal("state.json not renamed")
	}
	s.Close()

	// Reopening must not re-import.
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
}

func TestAnalysesGoOutdatedWhenBasisChanges(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.ReplaceChannels(ctx, []Channel{{ID: "UC1", SubscriptionID: "s1", Title: "A"}, {ID: "UC2", SubscriptionID: "s2", Title: "B"}})
	s.SetInterests(ctx, "go")
	st, _ := s.Snapshot(ctx)
	s.SaveAnalyses(ctx, map[string]Analysis{
		"UC1": {Verdict: VerdictKeep, AnalyzedAt: time.Now(), Basis: st.Basis},
		"UC2": {Verdict: VerdictKeep, AnalyzedAt: time.Now()}, // legacy row without a basis
	})

	st, _ = s.Snapshot(ctx)
	if st.Channels[0].Analysis.Outdated || st.Channels[1].Analysis.Outdated {
		t.Fatal("nothing changed yet; nothing should be outdated")
	}

	s.SetInterests(ctx, "cooking")
	st, _ = s.Snapshot(ctx)
	if !st.Channels[0].Analysis.Outdated {
		t.Fatal("changing interests should outdate UC1")
	}
	if st.Channels[1].Analysis.Outdated {
		t.Fatal("legacy analyses without a basis are not flagged")
	}

	s.SetInterests(ctx, "go")
	st, _ = s.Snapshot(ctx)
	if st.Channels[0].Analysis.Outdated {
		t.Fatal("restoring the same interests should match again")
	}
	if _, err := s.ImportWatchStats(ctx, map[string]WatchStat{}); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Snapshot(ctx)
	if !st.Channels[0].Analysis.Outdated {
		t.Fatal("importing watch history should outdate UC1")
	}
}

func TestDecisionsOverrideAndSurviveResync(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	chs := []Channel{{ID: "UC1", SubscriptionID: "s1", Title: "A"}}
	s.ReplaceChannels(ctx, chs)
	s.SaveAnalyses(ctx, map[string]Analysis{"UC1": {Verdict: VerdictUnsubscribe, AnalyzedAt: time.Now()}})
	if err := s.SetDecision(ctx, "UC1", VerdictKeep); err != nil {
		t.Fatal(err)
	}
	s.ReplaceChannels(ctx, chs)
	st, _ := s.Snapshot(ctx)
	if c := st.Channels[0]; c.Verdict() != VerdictKeep || c.Analysis.Verdict != VerdictUnsubscribe {
		t.Fatalf("decision should win over the AI and survive a sync: %+v", c)
	}
	s.SetDecision(ctx, "UC1", "")
	st, _ = s.Snapshot(ctx)
	if st.Channels[0].Verdict() != VerdictUnsubscribe {
		t.Fatal("clearing the decision should fall back to the AI verdict")
	}
}
