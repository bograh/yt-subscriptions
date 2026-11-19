package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"yt-subscriptions/internal/ai"
	"yt-subscriptions/internal/store"
)

// fakeAnalyzer answers per call with behaviour chosen by the test.
type fakeAnalyzer struct {
	mu    sync.Mutex
	calls int
	fn    func(call int, batch []store.Channel) (map[string]store.Analysis, error)
}

func (f *fakeAnalyzer) Analyze(_ context.Context, _ string, _ bool, batch []store.Channel) (map[string]store.Analysis, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.fn(n, batch)
}

func keepAll(batch []store.Channel) map[string]store.Analysis {
	out := map[string]store.Analysis{}
	for _, c := range batch {
		out[c.ID] = store.Analysis{Verdict: store.VerdictKeep, AnalyzedAt: time.Now()}
	}
	return out
}

func newAnalyzeApp(t *testing.T, n int, f *fakeAnalyzer) (*App, store.State, []store.Channel) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var chs []store.Channel
	for i := range n {
		chs = append(chs, store.Channel{ID: fmt.Sprintf("UC%02d", i), SubscriptionID: fmt.Sprint(i), Title: fmt.Sprint(i)})
	}
	st.ReplaceChannels(context.Background(), chs)
	snap, _ := st.Snapshot(context.Background())
	return &App{store: st, testAnalyzer: f, retryBackoff: time.Millisecond}, snap, snap.Channels
}

func TestAnalysisRetriesSkippedChannels(t *testing.T) {
	f := &fakeAnalyzer{fn: func(call int, batch []store.Channel) (map[string]store.Analysis, error) {
		out := keepAll(batch)
		if len(batch) > 1 {
			delete(out, batch[0].ID) // the model "forgets" one channel per full batch
		}
		return out, nil
	}}
	a, snap, todo := newAnalyzeApp(t, 30, f) // two batches: 25 + 5
	res := a.runAnalysis(context.Background(), snap, todo, func(int, int) {})
	if res.failed != 0 || res.saveErr != nil {
		t.Fatalf("unexpected result: %+v", res)
	}
	got, _ := a.store.Snapshot(context.Background())
	for _, c := range got.Channels {
		if c.Analysis == nil || c.Analysis.Basis != snap.Basis {
			t.Fatalf("%s not analyzed (or missing basis)", c.ID)
		}
	}
	if f.calls != 4 { // 2 batches + 1 follow-up each
		t.Fatalf("calls = %d, want 4", f.calls)
	}
}

func TestAnalysisBacksOffOnRetryableErrors(t *testing.T) {
	f := &fakeAnalyzer{fn: func(call int, batch []store.Channel) (map[string]store.Analysis, error) {
		if call <= 2 {
			return nil, &ai.HTTPError{StatusCode: 429}
		}
		return keepAll(batch), nil
	}}
	a, snap, todo := newAnalyzeApp(t, 3, f)
	if res := a.runAnalysis(context.Background(), snap, todo, func(int, int) {}); res.failed != 0 {
		t.Fatalf("should succeed on the third attempt: %+v", res)
	}
}

func TestAnalysisDoesNotRetryClientErrors(t *testing.T) {
	f := &fakeAnalyzer{fn: func(int, []store.Channel) (map[string]store.Analysis, error) {
		return nil, &ai.HTTPError{StatusCode: 401, Body: "bad key"}
	}}
	a, snap, todo := newAnalyzeApp(t, 3, f)
	res := a.runAnalysis(context.Background(), snap, todo, func(int, int) {})
	var httpErr *ai.HTTPError
	if res.failed != 3 || !errors.As(res.firstErr, &httpErr) || f.calls != 1 {
		t.Fatalf("want 3 failed after 1 call with the 401, got %+v after %d calls", res, f.calls)
	}
}
