package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/a-h/templ"
	"golang.org/x/oauth2"

	"yt-subscriptions/internal/ai"
	"yt-subscriptions/internal/store"
	"yt-subscriptions/internal/youtube"
	"yt-subscriptions/views"
)

const analyzeBatchSize = 25

type App struct {
	ctx   context.Context // cancelled on shutdown; parent of background jobs
	jobs  sync.WaitGroup
	store *store.Store
	oauth *oauth2.Config
	ai    *ai.Client

	mu          sync.Mutex
	job         views.Job
	lastErr     string
	oauthStates map[string]time.Time
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", staticHandler())
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /channels", a.handleChannels)
	mux.HandleFunc("DELETE /channels/{id}", a.handleUnsubscribe)
	mux.HandleFunc("GET /job", a.handleJob)
	mux.HandleFunc("POST /sync", a.handleSync)
	mux.HandleFunc("POST /analyze", a.handleAnalyze)
	mux.HandleFunc("POST /interests", a.handleInterests)
	mux.HandleFunc("POST /history", a.handleHistory)
	mux.HandleFunc("GET /auth/login", a.handleLogin)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	return mux
}

// ---- pages ----

func (a *App) page(r *http.Request) (views.Page, error) {
	st, err := a.store.Snapshot(r.Context())
	if err != nil {
		return views.Page{}, err
	}
	a.mu.Lock()
	job, lastErr := a.job, a.lastErr
	a.mu.Unlock()
	q := r.URL.Query()
	return views.Page{
		State:        st,
		Connected:    a.store.Token() != nil,
		OAuthReady:   a.oauth.ClientID != "" && a.oauth.ClientSecret != "",
		AIConfigured: a.ai.Model != "",
		Job:          job,
		Error:        lastErr,
		Stats:        views.ComputeStats(st),
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
	a.lastErr = ""
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
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	a.startJob(w, r, "Syncing subscriptions", func(ctx context.Context, progress func(int, int)) error {
		fresh, err := yt.FetchSubscriptions(ctx, progress)
		if err != nil {
			return err
		}
		// Analyses and watch stats live in their own tables, so they carry over automatically.
		return a.store.ReplaceChannels(ctx, fresh)
	})
}

func (a *App) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	if a.ai.Model == "" {
		http.Error(w, "AI_MODEL is not configured", http.StatusBadRequest)
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
			if all || c.Analysis == nil {
				todo = append(todo, c)
			}
		}
		progress(0, len(todo))
		var failed []error
		for start := 0; start < len(todo); start += analyzeBatchSize {
			batch := todo[start:min(start+analyzeBatchSize, len(todo))]
			res, err := a.ai.Analyze(ctx, st.Interests, st.HistoryImported, batch)
			if err != nil && ctx.Err() == nil { // one retry for flaky model output
				res, err = a.ai.Analyze(ctx, st.Interests, st.HistoryImported, batch)
			}
			if err != nil {
				failed = append(failed, err)
				if ctx.Err() != nil {
					break
				}
			} else if err := a.store.SaveAnalyses(ctx, res); err != nil {
				return err
			}
			progress(min(start+analyzeBatchSize, len(todo)), len(todo))
		}
		if len(failed) > 0 {
			return fmt.Errorf("%d batch(es) failed, run \"Analyze new\" to retry them; first error: %w", len(failed), failed[0])
		}
		return nil
	})
}

// ---- actions ----

func (a *App) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	subID, err := a.store.SubscriptionID(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if subID == "" {
		http.Error(w, "unknown channel", http.StatusNotFound)
		return
	}
	yt, err := a.youtube()
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if err := yt.Unsubscribe(r.Context(), subID); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := a.store.DeleteChannel(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	st, err := a.store.Snapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Empty body removes the card; stats are refreshed out-of-band.
	render(w, r, views.Summary(views.ComputeStats(st), true))
}

func (a *App) handleInterests(w http.ResponseWriter, r *http.Request) {
	interests := r.FormValue("interests")
	if err := a.store.SetInterests(r.Context(), interests); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<20)
	f, _, err := r.FormFile("file")
	if err != nil {
		fmt.Fprint(w, "Choose your watch-history.json file first.")
		return
	}
	defer f.Close()
	stats, err := youtube.ParseWatchHistory(f)
	if err != nil {
		fmt.Fprintf(w, "Could not read that file (is it the JSON export?): %v", err)
		return
	}
	matched, err := a.store.ImportWatchStats(r.Context(), stats)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "Imported. You've watched %d of your subscribed channels. Run \"Re-analyze all\" to use it.", matched)
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
	return &youtube.Client{HTTP: oauth2.NewClient(context.Background(), ts)}, nil
}

type savingTokenSource struct {
	mu    sync.Mutex
	src   oauth2.TokenSource
	store *store.Store
	last  string
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err != nil {
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

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		log.Printf("render: %v", err)
	}
}
