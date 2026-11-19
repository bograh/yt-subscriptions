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

	"golang.org/x/oauth2"

	"yt-subscriptions/internal/store"
)

// newTestServer returns a Server on a fresh database with a generous quota,
// fast AI retries and the given AI fake (nil disables analysis). Point ytBase
// at an httptest server to fake YouTube.
func newTestServer(t *testing.T, analyzer Analyzer) *Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(Options{Store: st, OAuth: &oauth2.Config{}, AI: analyzer, QuotaLimit: 10_000})
	s.retryBackoff = time.Millisecond
	t.Cleanup(s.Close)
	return s
}

// connect stores a valid-looking token so s.youtube() works.
func connect(t *testing.T, s *Server) {
	t.Helper()
	if err := s.store.SaveToken(&oauth2.Token{AccessToken: "a", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

// fakeAI answers each call with behaviour chosen by the test. call counts
// calls of that kind from 1.
type fakeAI struct {
	mu        sync.Mutex
	calls     int
	channels  func(call int, batch []store.Channel) (map[string]store.Analysis, error)
	playlists func(call int, batch []store.Playlist) (map[string]store.PlaylistAnalysis, error)
}

func (f *fakeAI) next() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.calls
}

func (f *fakeAI) Analyze(_ context.Context, _ string, _ bool, batch []store.Channel) (map[string]store.Analysis, error) {
	return f.channels(f.next(), batch)
}

func (f *fakeAI) AnalyzePlaylists(_ context.Context, _ string, _, batch []store.Playlist) (map[string]store.PlaylistAnalysis, error) {
	return f.playlists(f.next(), batch)
}

// runJob calls a handler that starts a background job, waits for the job and
// fails the test if it reported an error.
func runJob(t *testing.T, s *Server, h http.HandlerFunc, path, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if id != "" {
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	s.jobs.Wait()
	if s.lastErr != "" {
		t.Fatalf("%s: job failed: %s", path, s.lastErr)
	}
	return rec
}

// postForm sends a form POST to h, waits for any job it started and fails
// the test if that job reported an error.
func postForm(t *testing.T, s *Server, h http.HandlerFunc, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h(rec, req)
	s.jobs.Wait()
	if s.lastErr != "" {
		t.Fatalf("job failed: %s", s.lastErr)
	}
	return rec
}
