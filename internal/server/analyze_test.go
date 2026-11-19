package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"yt-subscriptions/internal/ai"
	"yt-subscriptions/internal/store"
)

func keepAll(batch []store.Channel) map[string]store.Analysis {
	out := map[string]store.Analysis{}
	for _, c := range batch {
		out[c.ID] = store.Analysis{Verdict: store.VerdictKeep, AnalyzedAt: time.Now()}
	}
	return out
}

// newAnalyzeServer returns a server with n synced channels and their snapshot.
func newAnalyzeServer(t *testing.T, n int, f *fakeAI) (*Server, store.State) {
	t.Helper()
	s := newTestServer(t, f)
	var chs []store.Channel
	for i := range n {
		chs = append(chs, store.Channel{ID: fmt.Sprintf("UC%02d", i), SubscriptionID: fmt.Sprint(i), Title: fmt.Sprint(i)})
	}
	s.store.ReplaceChannels(context.Background(), chs)
	snap, _ := s.store.Snapshot(context.Background())
	return s, snap
}

func TestAnalysisRetriesSkippedChannels(t *testing.T) {
	f := &fakeAI{channels: func(call int, batch []store.Channel) (map[string]store.Analysis, error) {
		out := keepAll(batch)
		if len(batch) > 1 {
			delete(out, batch[0].ID) // the model "forgets" one channel per full batch
		}
		return out, nil
	}}
	s, snap := newAnalyzeServer(t, 30, f) // two batches: 25 + 5
	res := runAnalysis(context.Background(), s.channelAnalysis(snap), snap.Channels, func(int, int) {})
	if res.err() != nil {
		t.Fatalf("unexpected result: %+v", res)
	}
	got, _ := s.store.Snapshot(context.Background())
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
	f := &fakeAI{channels: func(call int, batch []store.Channel) (map[string]store.Analysis, error) {
		if call <= 2 {
			return nil, &ai.HTTPError{StatusCode: 429}
		}
		return keepAll(batch), nil
	}}
	s, snap := newAnalyzeServer(t, 3, f)
	if res := runAnalysis(context.Background(), s.channelAnalysis(snap), snap.Channels, func(int, int) {}); res.failed != 0 {
		t.Fatalf("should succeed on the third attempt: %+v", res)
	}
}

func TestAnalysisDoesNotRetryClientErrors(t *testing.T) {
	f := &fakeAI{channels: func(int, []store.Channel) (map[string]store.Analysis, error) {
		return nil, &ai.HTTPError{StatusCode: 401, Body: "bad key"}
	}}
	s, snap := newAnalyzeServer(t, 3, f)
	res := runAnalysis(context.Background(), s.channelAnalysis(snap), snap.Channels, func(int, int) {})
	var httpErr *ai.HTTPError
	if res.failed != 3 || !errors.As(res.err(), &httpErr) || f.calls != 1 {
		t.Fatalf("want 3 failed after 1 call with the 401, got %+v after %d calls", res, f.calls)
	}
}

func TestPlaylistAnalysisSkipsDecidedAndRetriesSkipped(t *testing.T) {
	f := &fakeAI{playlists: func(_ int, batch []store.Playlist) (map[string]store.PlaylistAnalysis, error) {
		out := map[string]store.PlaylistAnalysis{}
		for i, p := range batch {
			if i == 0 && len(batch) > 1 {
				continue // skip one; the job should ask again
			}
			out[p.ID] = store.PlaylistAnalysis{Topic: "Programming", Verdict: store.PlaylistKeep, AnalyzedAt: time.Now()}
		}
		return out, nil
	}}
	s := newTestServer(t, f)
	ctx := context.Background()
	var pls []store.Playlist
	for i := range 25 {
		pls = append(pls, store.Playlist{ID: fmt.Sprintf("PL%02d", i), Title: fmt.Sprint(i)})
	}
	s.store.ReplacePlaylists(ctx, pls)
	s.store.SetPlaylistDecision(ctx, "PL00", store.PlaylistKeep)

	runJob(t, s, s.handlePlaylistAnalyze, "/playlists/analyze", "")

	ps, _ := s.store.Playlists(ctx)
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
