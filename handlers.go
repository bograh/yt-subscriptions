package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"golang.org/x/oauth2"

	"yt-subscriptions/internal/ai"
	"yt-subscriptions/internal/store"
	"yt-subscriptions/internal/youtube"
	"yt-subscriptions/views"
)

type App struct {
	ctx   context.Context // cancelled on shutdown; parent of background jobs
	jobs  sync.WaitGroup
	store *store.Store
	oauth *oauth2.Config
	ai    *ai.Client

	mu          sync.Mutex
	job         views.Job
	lastErr     string
	lastWarn    string
	flash       string // one-time notice, cleared once shown
	oauthStates map[string]time.Time
	quotaLimit  int    // YouTube API units per day (YOUTUBE_DAILY_QUOTA)
	ytBase      string // YouTube API base URL override (tests)
	// Test hooks for the analysis job.
	testAnalyzer analyzer
	retryBackoff time.Duration
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", staticHandler())
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /channels", a.handleChannels)
	mux.HandleFunc("DELETE /channels/{id}", a.handleUnsubscribe)
	mux.HandleFunc("POST /unsubscribe-suggested", a.handleBulkUnsubscribe)
	mux.HandleFunc("POST /channels/{id}/keep", a.handleDecision(store.VerdictKeep))
	mux.HandleFunc("DELETE /channels/{id}/keep", a.handleDecision(""))
	mux.HandleFunc("GET /job", a.handleJob)
	mux.HandleFunc("POST /sync", a.handleSync)
	mux.HandleFunc("POST /analyze", a.handleAnalyze)
	mux.HandleFunc("POST /interests", a.handleInterests)
	mux.HandleFunc("POST /history", a.handleHistory)
	mux.HandleFunc("GET /auth/login", a.handleLogin)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	mux.HandleFunc("POST /auth/logout", a.handleLogout)
	return mux
}

// ---- pages ----

func (a *App) page(r *http.Request) (views.Page, error) {
	st, err := a.store.Snapshot(r.Context())
	if err != nil {
		return views.Page{}, err
	}
	a.mu.Lock()
	job, lastErr, lastWarn, flash := a.job, a.lastErr, a.lastWarn, a.flash
	a.flash = ""
	a.mu.Unlock()
	q := r.URL.Query()
	return views.Page{
		State:        st,
		Connected:    a.store.Token() != nil,
		OAuthReady:   a.oauth.ClientID != "" && a.oauth.ClientSecret != "",
		AIConfigured: a.ai.Model != "",
		Job:          job,
		Error:        lastErr,
		Warning:      lastWarn,
		Flash:        flash,
		Stats:        views.ComputeStats(st, a.quotaLimit),
		List:         views.BuildList(st.Channels, q.Get("group"), q.Get("verdict"), q.Get("q")),
	}, nil
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	p, err := a.page(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, views.Dashboard(p))
}

func (a *App) handleChannels(w http.ResponseWriter, r *http.Request) {
	// A full page load of this URL (e.g. browser refresh after hx-push-url) gets the whole dashboard.
	if r.Header.Get("HX-Request") == "" {
		a.handleIndex(w, r)
		return
	}
	p, err := a.page(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, views.ChannelList(p.List))
}

func (a *App) handleJob(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	job := a.job
	a.mu.Unlock()
	if !job.Running {
		w.Header().Set("HX-Refresh", "true")
		return
	}
	render(w, r, views.JobBanner(job))
}

// ---- background jobs ----

// startJob runs fn in the background; only one job runs at a time.
func (a *App) startJob(w http.ResponseWriter, r *http.Request, label string, fn func(ctx context.Context, progress func(done, total int)) error) {
	a.mu.Lock()
	if a.job.Running {
		job := a.job
		a.mu.Unlock()
		render(w, r, views.JobBanner(job))
		return
	}
	a.job = views.Job{Running: true, Label: label}
	a.lastErr, a.lastWarn = "", ""
	job := a.job
	a.mu.Unlock()

	a.jobs.Add(1)
	go func() {
		defer a.jobs.Done()
		ctx, cancel := context.WithTimeout(a.ctx, 30*time.Minute)
		defer cancel()
		err := fn(ctx, func(done, total int) {
			a.mu.Lock()
			a.job.Done, a.job.Total = done, total
			a.mu.Unlock()
		})
		a.mu.Lock()
		a.job = views.Job{}
		if err != nil {
			a.lastErr = fmt.Sprintf("%s failed: %v", label, err)
			log.Print(a.lastErr)
		}
		a.mu.Unlock()
	}()
	render(w, r, views.JobBanner(job))
}

func (a *App) handleSync(w http.ResponseWriter, r *http.Request) {
	yt, err := a.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, "Can't sync", err)
		return
	}
	a.startJob(w, r, "Syncing subscriptions", func(ctx context.Context, progress func(int, int)) error {
		fresh, uploadFailures, err := yt.FetchSubscriptions(ctx, progress)
		if err != nil {
			// Nothing is written: a quota or auth failure must not replace good data.
			return a.checkAuth(err)
		}
		if len(uploadFailures) > 0 {
			// Keep last sync's upload info for these, instead of making them look inactive.
			prev, err := a.store.Snapshot(ctx)
			if err != nil {
				return err
			}
			old := make(map[string]store.Channel, len(prev.Channels))
			for _, c := range prev.Channels {
				old[c.ID] = c
			}
			failed := make(map[string]bool, len(uploadFailures))
			for _, id := range uploadFailures {
				failed[id] = true
			}
			for i := range fresh {
				if p, ok := old[fresh[i].ID]; ok && failed[fresh[i].ID] {
					fresh[i].LastUpload, fresh[i].RecentTitles = p.LastUpload, p.RecentTitles
				}
			}
			a.warn(fmt.Sprintf("Synced, but recent uploads couldn't be fetched for %d channel(s); their previous upload info was kept.", len(uploadFailures)))
		}
		// Analyses and watch stats live in their own tables, so they carry over automatically.
		return a.store.ReplaceChannels(ctx, fresh)
	})
}

func (a *App) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	if a.ai.Model == "" {
		fail(w, http.StatusBadRequest, "Can't analyze", errors.New("AI_MODEL is not configured"))
		return
	}
	all := r.URL.Query().Get("all") == "1"
	a.startJob(w, r, "Analyzing channels with AI", func(ctx context.Context, progress func(int, int)) error {
		st, err := a.store.Snapshot(ctx)
		if err != nil {
			return err
		}
		var todo []store.Channel
		for _, c := range st.Channels {
			if c.Decision != "" {
				continue // the user already decided; don't spend AI calls on it
			}
			if all || c.Analysis == nil || c.Analysis.Outdated {
				todo = append(todo, c)
			}
		}
		res := a.runAnalysis(ctx, st, todo, progress)
		if res.saveErr != nil {
			return res.saveErr
		}
		if res.failed > 0 {
			return fmt.Errorf("%d of %d channels couldn't be analyzed; run Analyze new to retry them. First error: %w", res.failed, len(todo), res.firstErr)
		}
		return nil
	})
}

func withBasis(res map[string]store.Analysis, basis string) map[string]store.Analysis {
	for id, an := range res {
		an.Basis = basis
		res[id] = an
	}
	return res
}

// warn records a non-fatal problem from a background job, shown on the next page load.
func (a *App) warn(msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastWarn = msg
	log.Print(msg)
}

// ---- actions ----

func (a *App) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	const title = "Couldn't unsubscribe"
	subID, err := a.store.SubscriptionID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	if subID == "" {
		fail(w, http.StatusNotFound, title, errors.New("that channel isn't in your synced subscriptions"))
		return
	}
	if err := a.ensureQuota(r.Context(), youtube.CostUnsubscribe); err != nil {
		fail(w, http.StatusTooManyRequests, title, err)
		return
	}
	yt, err := a.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, title, err)
		return
	}
	if err := yt.Unsubscribe(r.Context(), subID); err != nil {
		if err = a.checkAuth(err); errors.Is(err, ErrReconnect) {
			w.Header().Set("HX-Refresh", "true") // re-render with the Connect button
			fail(w, http.StatusUnauthorized, title, err)
			return
		}
		fail(w, http.StatusBadGateway, title, err)
		return
	}
	if err := a.store.DeleteChannel(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	st, err := a.store.Snapshot(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	// Empty body removes the card; stats are refreshed out-of-band.
	render(w, r, views.Summary(views.ComputeStats(st, a.quotaLimit), true))
}

// handleDecision pins (or, with "", clears) the user's own verdict for a channel
// and re-renders its row plus the summary.
func (a *App) handleDecision(verdict string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const title = "Couldn't save your decision"
		id := r.PathValue("id")
		if err := a.store.SetDecision(r.Context(), id, verdict); err != nil {
			fail(w, http.StatusInternalServerError, title, err)
			return
		}
		st, err := a.store.Snapshot(r.Context())
		if err != nil {
			fail(w, http.StatusInternalServerError, title, err)
			return
		}
		for _, c := range st.Channels {
			if c.ID == id {
				render(w, r, views.ChannelRow(c))
				render(w, r, views.Summary(views.ComputeStats(st, a.quotaLimit), true))
				return
			}
		}
		fail(w, http.StatusNotFound, title, errors.New("that channel isn't in your synced subscriptions"))
	}
}

// ensureQuota refuses work that would exceed today's estimated YouTube quota.
func (a *App) ensureQuota(ctx context.Context, units int) error {
	st, err := a.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	if left := a.quotaLimit - st.QuotaUsed; units > left {
		return fmt.Errorf("that needs %d YouTube API units but only about %d are left today; quota resets at midnight Pacific time", units, max(0, left))
	}
	return nil
}

// handleBulkUnsubscribe unsubscribes from every channel whose effective verdict is
// "unsubscribe", as a background job, stopping before the daily quota runs out.
func (a *App) handleBulkUnsubscribe(w http.ResponseWriter, r *http.Request) {
	yt, err := a.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, "Can't unsubscribe", err)
		return
	}
	a.startJob(w, r, "Unsubscribing from suggested channels", func(ctx context.Context, progress func(int, int)) error {
		st, err := a.store.Snapshot(ctx)
		if err != nil {
			return err
		}
		var todo []store.Channel
		for _, c := range st.Channels {
			if c.Verdict() == store.VerdictUnsubscribe {
				todo = append(todo, c)
			}
		}
		affordable := max(0, a.quotaLimit-st.QuotaUsed) / youtube.CostUnsubscribe
		skipped := 0
		if len(todo) > affordable {
			skipped = len(todo) - affordable
			todo = todo[:affordable]
		}
		progress(0, len(todo))
		var failed []string
		for i, c := range todo {
			if err := yt.Unsubscribe(ctx, c.SubscriptionID); err != nil {
				if err = a.checkAuth(err); errors.Is(err, ErrReconnect) {
					return err
				}
				var apiErr *youtube.APIError
				if errors.As(err, &apiErr) && apiErr.QuotaExceeded() {
					skipped += len(todo) - i
					break
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				failed = append(failed, c.Title)
			} else if err := a.store.DeleteChannel(ctx, c.ID); err != nil {
				return err
			}
			progress(i+1, len(todo))
		}
		switch {
		case len(failed) > 0:
			return fmt.Errorf("couldn't unsubscribe from %d channel(s): %s", len(failed), strings.Join(failed, ", "))
		case skipped > 0:
			a.warn(fmt.Sprintf("Stopped before YouTube's daily quota ran out; %d channel(s) are still marked Unsubscribe. Run it again after midnight Pacific time.", skipped))
		}
		return nil
	})
}

func (a *App) handleInterests(w http.ResponseWriter, r *http.Request) {
	interests := r.FormValue("interests")
	if err := a.store.SetInterests(r.Context(), interests); err != nil {
		fail(w, http.StatusInternalServerError, "Couldn't save interests", err)
		return
	}
	w.Header().Set("HX-Refresh", "true") // suggestions may now be outdated
}

func (a *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<20)
	const title = "Couldn't import watch history"
	f, _, err := r.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, title, errors.New("choose your Takeout watch-history file first"))
		return
	}
	defer f.Close()
	stats, err := youtube.ParseWatchHistory(f)
	if err != nil {
		fail(w, http.StatusBadRequest, title, fmt.Errorf("that doesn't look like a Takeout watch history: %w", err))
		return
	}
	if len(stats) == 0 {
		// Don't wipe existing stats with an empty import (wrong file, or a non-English export).
		fail(w, http.StatusBadRequest, title, errors.New("no watched channels found in that file; make sure it's watch-history from YouTube's Takeout export"))
		return
	}
	matched, err := a.store.ImportWatchStats(r.Context(), stats)
	if err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	a.mu.Lock()
	a.flash = fmt.Sprintf("Watch history imported: you've watched %d of your subscribed channels. Analyze new to update the suggestions.", matched)
	a.mu.Unlock()
	w.Header().Set("HX-Refresh", "true")
}

// ---- OAuth ----

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if a.oauth.ClientID == "" {
		http.Error(w, "GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET are not set", http.StatusBadRequest)
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	state := hex.EncodeToString(b)
	a.mu.Lock()
	for s, created := range a.oauthStates { // forget abandoned sign-ins
		if time.Since(created) > 10*time.Minute {
			delete(a.oauthStates, s)
		}
	}
	a.oauthStates[state] = time.Now()
	a.mu.Unlock()
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce), http.StatusFound)
}

func (a *App) handleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	a.mu.Lock()
	created, ok := a.oauthStates[state]
	delete(a.oauthStates, state)
	a.mu.Unlock()
	if !ok || time.Since(created) > 10*time.Minute {
		http.Error(w, "invalid or expired login state, try again", http.StatusBadRequest)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "Google sign-in failed: "+e, http.StatusBadRequest)
		return
	}
	tok, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := a.store.SaveToken(tok); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleLogout revokes the token at Google (best effort) and forgets it locally.
func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := a.store.Token(); tok != nil {
		t := cmp.Or(tok.RefreshToken, tok.AccessToken)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/revoke",
			strings.NewReader(url.Values{"token": {t}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if resp, err := http.DefaultClient.Do(req); err != nil {
			log.Printf("revoke token: %v", err)
		} else {
			resp.Body.Close()
		}
	}
	if err := a.store.DeleteToken(); err != nil {
		fail(w, http.StatusInternalServerError, "Couldn't disconnect", err)
		return
	}
	w.Header().Set("HX-Redirect", "/")
}

// youtube returns an API client whose refreshed tokens are persisted.
func (a *App) youtube() (*youtube.Client, error) {
	tok := a.store.Token()
	if tok == nil {
		return nil, errors.New("not connected to YouTube")
	}
	ts := &savingTokenSource{
		src:   a.oauth.TokenSource(context.Background(), tok),
		store: a.store,
		last:  tok.AccessToken,
	}
	return &youtube.Client{
		HTTP: oauth2.NewClient(context.Background(), ts),
		Base: a.ytBase,
		OnCost: func(units int) {
			if err := a.store.AddQuotaUsage(context.Background(), units); err != nil {
				log.Printf("record quota usage: %v", err)
			}
		},
	}, nil
}

type savingTokenSource struct {
	mu    sync.Mutex
	src   oauth2.TokenSource
	store *store.Store
	last  string
}

// ErrReconnect means Google no longer accepts our stored credentials. Apps in
// Google's "Testing" publishing status get refresh tokens that expire after 7 days.
var ErrReconnect = errors.New("your Google sign-in has expired or was revoked; connect YouTube again")

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
			if derr := s.store.DeleteToken(); derr != nil {
				log.Printf("delete token: %v", derr)
			}
			return nil, ErrReconnect
		}
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.AccessToken != s.last {
		s.last = t.AccessToken
		if err := s.store.SaveToken(t); err != nil {
			log.Printf("save token: %v", err)
		}
	}
	return t, nil
}

// checkAuth forgets the stored token when YouTube rejects it, and returns the
// error to report (ErrReconnect in that case).
func (a *App) checkAuth(err error) error {
	if errors.Is(err, ErrReconnect) {
		return ErrReconnect
	}
	var apiErr *youtube.APIError
	if errors.As(err, &apiErr) && apiErr.Unauthorized() {
		if derr := a.store.DeleteToken(); derr != nil {
			log.Printf("delete token: %v", derr)
		}
		return ErrReconnect
	}
	return err
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
