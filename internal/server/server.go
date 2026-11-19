// Package server is the web dashboard: HTTP routes, handlers, background jobs
// and the glue between the store, the YouTube API and the AI client.
//
// Files are split by feature:
//
//	server.go         Server, Options, the route table, shared helpers
//	middleware.go     auth, CSRF, security headers, /healthz, static files
//	jobs.go           the single background job slot and its progress banner
//	auth.go           Google sign-in, token refresh and storage
//	subscriptions.go  the Subscriptions page and its actions
//	playlists.go      the Playlists page and its actions
//	analyze.go        batched, retried AI analysis shared by both pages
package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/a-h/templ"
	"golang.org/x/oauth2"

	"yt-subscriptions/internal/store"
	"yt-subscriptions/views"
)

// Analyzer is what the server needs from the AI client. *ai.Client implements
// it; tests use a fake.
type Analyzer interface {
	Analyze(ctx context.Context, interests string, historyImported bool, channels []store.Channel) (map[string]store.Analysis, error)
	AnalyzePlaylists(ctx context.Context, interests string, all, batch []store.Playlist) (map[string]store.PlaylistAnalysis, error)
}

// Options configures a Server.
type Options struct {
	Store      *store.Store
	OAuth      *oauth2.Config
	AI         Analyzer // nil when no model is configured; analysis is then disabled
	QuotaLimit int      // YouTube API units per day
	Password   string   // dashboard password; empty disables basic auth
}

// Server serves the dashboard. Create it with New and stop it with Close.
type Server struct {
	store      *store.Store
	oauth      *oauth2.Config
	ai         Analyzer
	quotaLimit int
	password   string

	ctx    context.Context // cancelled by Close; parent of background jobs
	cancel context.CancelFunc
	jobs   sync.WaitGroup

	mu          sync.Mutex
	job         views.Job
	lastErr     string
	lastWarn    string
	flash       string // one-time notice, cleared once shown
	oauthStates map[string]time.Time

	// Test hooks.
	ytBase       string        // YouTube API base URL override
	retryBackoff time.Duration // first AI retry delay (default 2s)
}

// New returns a Server. Call Close when done to stop background jobs.
func New(opts Options) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		store:       opts.Store,
		oauth:       opts.OAuth,
		ai:          opts.AI,
		quotaLimit:  opts.QuotaLimit,
		password:    opts.Password,
		ctx:         ctx,
		cancel:      cancel,
		oauthStates: map[string]time.Time{},
	}
}

// Close cancels any running background job and waits for it to stop.
func (s *Server) Close() {
	s.cancel()
	s.jobs.Wait()
}

// Handler returns the dashboard with all middleware applied.
func (s *Server) Handler() http.Handler {
	return withHealth(securityHeaders(secure(s.routes(), s.password)), s.store)
}

// routes is the full route table. HTMX requests from views/*.templ hit these.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", staticHandler())

	// Subscriptions (subscriptions.go)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /channels", s.handleChannels)
	mux.HandleFunc("POST /sync", s.handleSync)
	mux.HandleFunc("POST /analyze", s.handleAnalyze)
	mux.HandleFunc("DELETE /channels/{id}", s.handleUnsubscribe)
	mux.HandleFunc("POST /unsubscribe-suggested", s.handleBulkUnsubscribe)
	mux.HandleFunc("POST /channels/{id}/keep", s.handleDecision(store.VerdictKeep))
	mux.HandleFunc("DELETE /channels/{id}/keep", s.handleDecision(""))
	mux.HandleFunc("POST /interests", s.handleInterests)
	mux.HandleFunc("POST /history", s.handleHistory)

	// Playlists (playlists.go)
	mux.HandleFunc("GET /playlists", s.handlePlaylists)
	mux.HandleFunc("GET /playlists/list", s.handlePlaylistList)
	mux.HandleFunc("POST /playlists/sync", s.handlePlaylistSync)
	mux.HandleFunc("POST /playlists/analyze", s.handlePlaylistAnalyze)
	mux.HandleFunc("POST /playlists/clean-all", s.handlePlaylistClean)
	mux.HandleFunc("POST /playlists/{id}/clean", s.handlePlaylistClean)
	mux.HandleFunc("POST /playlists/{id}/merge", s.handlePlaylistMerge)
	mux.HandleFunc("POST /playlists/{id}/keep", s.handlePlaylistDecision(store.PlaylistKeep))
	mux.HandleFunc("DELETE /playlists/{id}/keep", s.handlePlaylistDecision(""))
	mux.HandleFunc("DELETE /playlists/{id}", s.handlePlaylistDelete)

	// Background jobs (jobs.go) and sign-in (auth.go)
	mux.HandleFunc("GET /job", s.handleJob)
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	return mux
}

// status collects what every page shows around its content, and clears the
// one-time flash message.
func (s *Server) status() views.Status {
	connected := s.store.Token() != nil
	s.mu.Lock()
	defer s.mu.Unlock()
	st := views.Status{
		Connected:    connected,
		OAuthReady:   s.oauth.ClientID != "" && s.oauth.ClientSecret != "",
		AIConfigured: s.ai != nil,
		Job:          s.job,
		Error:        s.lastErr,
		Warning:      s.lastWarn,
		Flash:        s.flash,
	}
	s.flash = ""
	return st
}

// ensureQuota refuses work that would exceed today's estimated YouTube quota.
func (s *Server) ensureQuota(ctx context.Context, units int) error {
	st, err := s.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	if left := s.quotaLimit - st.QuotaUsed; units > left {
		return fmt.Errorf("that needs %d YouTube API units but only about %d are left today; quota resets at midnight Pacific time", units, max(0, left))
	}
	return nil
}

// fail writes a plain-text error; static/app.js shows it as a toast with the given title.
func fail(w http.ResponseWriter, status int, title string, err error) {
	w.Header().Set("X-Error-Title", title)
	http.Error(w, err.Error(), status)
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		log.Printf("render: %v", err)
	}
}
