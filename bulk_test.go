package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"yt-subscriptions/internal/store"
)

// Bulk unsubscribe must skip channels the user chose to keep and stop before
// the daily quota runs out.
func TestBulkUnsubscribeRespectsDecisionsAndQuota(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	yt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		deleted = append(deleted, r.URL.Query().Get("id"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer yt.Close()

	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.SaveToken(&oauth2.Token{AccessToken: "a", Expiry: time.Now().Add(time.Hour)})
	st.ReplaceChannels(ctx, []store.Channel{
		{ID: "UC1", SubscriptionID: "s1", Title: "One"},
		{ID: "UC2", SubscriptionID: "s2", Title: "Two"},
		{ID: "UC3", SubscriptionID: "s3", Title: "Three"},
		{ID: "UC4", SubscriptionID: "s4", Title: "Four"},
	})
	now := time.Now()
	st.SaveAnalyses(ctx, map[string]store.Analysis{
		"UC1": {Verdict: store.VerdictUnsubscribe, AnalyzedAt: now},
		"UC2": {Verdict: store.VerdictUnsubscribe, AnalyzedAt: now},
		"UC3": {Verdict: store.VerdictUnsubscribe, AnalyzedAt: now},
		"UC4": {Verdict: store.VerdictUnsubscribe, AnalyzedAt: now},
	})
	st.SetDecision(ctx, "UC2", store.VerdictKeep)

	a := &App{
		ctx:        ctx,
		store:      st,
		oauth:      &oauth2.Config{},
		quotaLimit: 100, // room for two 50-unit unsubscribes
		ytBase:     yt.URL + "/",
	}
	req := httptest.NewRequest(http.MethodPost, "/unsubscribe-suggested", nil)
	a.handleBulkUnsubscribe(httptest.NewRecorder(), req)
	a.jobs.Wait()

	if a.lastErr != "" {
		t.Fatalf("job failed: %s", a.lastErr)
	}
	// Channels are processed in title order: Four, One, (Two kept), Three.
	if strings.Join(deleted, ",") != "s4,s1" {
		t.Fatalf("deleted %v, want s4,s1 (Two kept, Three over quota)", deleted)
	}
	if !strings.Contains(a.lastWarn, "1 channel(s) are still marked Unsubscribe") {
		t.Fatalf("expected a quota warning, got %q", a.lastWarn)
	}
	snap, _ := st.Snapshot(ctx)
	if len(snap.Channels) != 2 || snap.QuotaUsed != 100 {
		t.Fatalf("want Two+Three left and 100 units used, got %d channels, %d units", len(snap.Channels), snap.QuotaUsed)
	}
}
