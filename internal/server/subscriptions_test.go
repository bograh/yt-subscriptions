package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

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
	a := newTestServer(t, nil)
	a.quotaLimit = 100 // room for two 50-unit unsubscribes
	a.ytBase = yt.URL + "/"
	connect(t, a)
	st := a.store
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
	if !strings.Contains(a.lastWarn, "1 channel(s) are still subscribed") {
		t.Fatalf("expected a quota warning, got %q", a.lastWarn)
	}
	snap, _ := st.Snapshot(ctx)
	if len(snap.Channels) != 2 || snap.QuotaUsed != 100 {
		t.Fatalf("want Two+Three left and 100 units used, got %d channels, %d units", len(snap.Channels), snap.QuotaUsed)
	}
}

func TestChannelBulkActions(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	yt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deleted = append(deleted, r.URL.Query().Get("id"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer yt.Close()

	ctx := context.Background()
	s := newTestServer(t, nil)
	s.ytBase = yt.URL + "/"
	connect(t, s)
	s.store.ReplaceChannels(ctx, []store.Channel{
		{ID: "UC1", SubscriptionID: "s1", Title: "One"},
		{ID: "UC2", SubscriptionID: "s2", Title: "Two"},
		{ID: "UC3", SubscriptionID: "s3", Title: "Three"},
	})
	decisions := func() map[string]string {
		snap, _ := s.store.Snapshot(ctx)
		out := map[string]string{}
		for _, c := range snap.Channels {
			out[c.ID] = c.Decision
		}
		return out
	}

	rec := postForm(t, s, s.handleChannelBulk, url.Values{"action": {"keep"}, "ids": {"UC1", "UC2"}})
	if rec.Header().Get("HX-Refresh") != "true" || decisions()["UC1"] != store.VerdictKeep || decisions()["UC2"] != store.VerdictKeep || decisions()["UC3"] != "" {
		t.Fatalf("after keep: %v", decisions())
	}
	postForm(t, s, s.handleChannelBulk, url.Values{"action": {"undo"}, "ids": {"UC2"}})
	if d := decisions(); d["UC1"] != store.VerdictKeep || d["UC2"] != "" {
		t.Fatalf("after undo: %v", d)
	}

	// Unsubscribing works whatever the verdict, including kept channels; unknown IDs are ignored.
	postForm(t, s, s.handleChannelBulk, url.Values{"action": {"unsubscribe"}, "ids": {"UC1", "UC3", "UCgone"}})
	if strings.Join(deleted, ",") != "s1,s3" {
		t.Fatalf("unsubscribed %v, want s1,s3", deleted)
	}
	if d := decisions(); len(d) != 1 || d["UC2"] != "" {
		t.Fatalf("left %v, want only UC2", d)
	}

	for _, form := range []url.Values{{"action": {"keep"}}, {"action": {"explode"}, "ids": {"UC2"}}} {
		if rec := postForm(t, s, s.handleChannelBulk, form); rec.Code != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400", form, rec.Code)
		}
	}
}
