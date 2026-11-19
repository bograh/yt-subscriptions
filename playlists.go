package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"yt-subscriptions/internal/ai"
	"yt-subscriptions/internal/store"
	"yt-subscriptions/internal/youtube"
	"yt-subscriptions/views"
)

const playlistBatchSize = 20

func (a *App) playlistRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /playlists", a.handlePlaylists)
	mux.HandleFunc("GET /playlists/list", a.handlePlaylistList)
	mux.HandleFunc("POST /playlists/sync", a.handlePlaylistSync)
	mux.HandleFunc("POST /playlists/analyze", a.handlePlaylistAnalyze)
	mux.HandleFunc("POST /playlists/clean-all", a.handlePlaylistClean(""))
	mux.HandleFunc("POST /playlists/{id}/clean", func(w http.ResponseWriter, r *http.Request) {
		a.handlePlaylistClean(r.PathValue("id"))(w, r)
	})
	mux.HandleFunc("POST /playlists/{id}/merge", a.handlePlaylistMerge)
	mux.HandleFunc("POST /playlists/{id}/keep", a.handlePlaylistDecision(store.PlaylistKeep))
	mux.HandleFunc("DELETE /playlists/{id}/keep", a.handlePlaylistDecision(""))
	mux.HandleFunc("DELETE /playlists/{id}", a.handlePlaylistDelete)
}

func (a *App) playlistPage(r *http.Request) (views.PlaylistPage, error) {
	ps, err := a.store.Playlists(r.Context())
	if err != nil {
		return views.PlaylistPage{}, err
	}
	a.mu.Lock()
	job, lastErr, lastWarn, flash := a.job, a.lastErr, a.lastWarn, a.flash
	a.flash = ""
	a.mu.Unlock()
	q := r.URL.Query()
	return views.PlaylistPage{
		State:        ps,
		Connected:    a.store.Token() != nil,
		OAuthReady:   a.oauth.ClientID != "" && a.oauth.ClientSecret != "",
		AIConfigured: a.ai.Model != "",
		Job:          job,
		Error:        lastErr,
		Warning:      lastWarn,
		Flash:        flash,
		Stats:        views.ComputePlaylistStats(ps, a.quotaLimit),
		List:         views.BuildPlaylistList(ps.Playlists, q.Get("group"), q.Get("verdict"), q.Get("q")),
	}, nil
}

func (a *App) handlePlaylists(w http.ResponseWriter, r *http.Request) {
	p, err := a.playlistPage(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, views.PlaylistsPage(p))
}

func (a *App) handlePlaylistList(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "" {
		a.handlePlaylists(w, r)
		return
	}
	p, err := a.playlistPage(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, views.PlaylistRows(p.List))
}

func (a *App) handlePlaylistSync(w http.ResponseWriter, r *http.Request) {
	yt, err := a.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, "Can't sync playlists", err)
		return
	}
	a.startJob(w, r, "Syncing playlists", func(ctx context.Context, progress func(int, int)) error {
		return a.syncPlaylists(ctx, yt, progress)
	})
}

func (a *App) syncPlaylists(ctx context.Context, yt *youtube.Client, progress func(int, int)) error {
	playlists, err := yt.FetchPlaylists(ctx, progress)
	if err != nil {
		return a.checkAuth(err) // nothing is written on failure
	}
	return a.store.ReplacePlaylists(ctx, playlists)
}

func (a *App) handlePlaylistAnalyze(w http.ResponseWriter, r *http.Request) {
	if a.ai.Model == "" {
		fail(w, http.StatusBadRequest, "Can't analyze", errors.New("AI_MODEL is not configured"))
		return
	}
	all := r.URL.Query().Get("all") == "1"
	a.startJob(w, r, "Analyzing playlists with AI", func(ctx context.Context, progress func(int, int)) error {
		ps, err := a.store.Playlists(ctx)
		if err != nil {
			return err
		}
		var todo []store.Playlist
		for _, p := range ps.Playlists {
			if p.Decision != "" {
				continue
			}
			if all || p.Analysis == nil || p.Analysis.Outdated {
				todo = append(todo, p)
			}
		}
		return a.runPlaylistAnalysis(ctx, ps, todo, progress)
	})
}

// runPlaylistAnalysis mirrors runAnalysis for channels: a few batches at a time,
// retries with backoff, one follow-up for playlists the model skipped.
func (a *App) runPlaylistAnalysis(ctx context.Context, ps store.PlaylistState, todo []store.Playlist, progress func(int, int)) error {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		done     int
		failed   int
		firstErr error
		saveErr  error
		sem      = make(chan struct{}, analyzeConcurrency)
	)
	progress(0, len(todo))
	for start := 0; start < len(todo); start += playlistBatchSize {
		batch := todo[start:min(start+playlistBatchSize, len(todo))]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			got, err := a.analyzePlaylistBatch(ctx, ps, batch)
			if len(got) > 0 {
				for id, an := range got {
					an.Basis = ps.Basis
					got[id] = an
				}
				if serr := a.store.SavePlaylistAnalyses(ctx, got); serr != nil {
					mu.Lock()
					saveErr = cmpErr(saveErr, serr)
					mu.Unlock()
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if missing := len(batch) - len(got); missing > 0 {
				failed += missing
				if err == nil {
					err = errors.New("the model skipped some playlists twice")
				}
				firstErr = cmpErr(firstErr, err)
			}
			done += len(batch)
			progress(done, len(todo))
		}()
	}
	wg.Wait()
	if saveErr != nil {
		return saveErr
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d playlists couldn't be analyzed; run Analyze new to retry them. First error: %w", failed, len(todo), firstErr)
	}
	return nil
}

func (a *App) analyzePlaylistBatch(ctx context.Context, ps store.PlaylistState, batch []store.Playlist) (map[string]store.PlaylistAnalysis, error) {
	call := func(b []store.Playlist) (map[string]store.PlaylistAnalysis, error) {
		return retry(ctx, a.retryBackoff, func() (map[string]store.PlaylistAnalysis, error) {
			return a.playlistAnalyzer().AnalyzePlaylists(ctx, ps.Interests, ps.Playlists, b)
		})
	}
	got, err := call(batch)
	if err != nil {
		return nil, err
	}
	var missing []store.Playlist
	for _, p := range batch {
		if _, ok := got[p.ID]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return got, nil
	}
	more, err := call(missing)
	for id, an := range more {
		got[id] = an
	}
	return got, err
}

type playlistAnalyzer interface {
	AnalyzePlaylists(ctx context.Context, interests string, all, batch []store.Playlist) (map[string]store.PlaylistAnalysis, error)
}

func (a *App) playlistAnalyzer() playlistAnalyzer {
	if a.testPlaylistAnalyzer != nil {
		return a.testPlaylistAnalyzer
	}
	return a.ai
}

// handlePlaylistClean removes unavailable and duplicate videos, from one
// playlist (id) or from all of them (""), within today's quota.
func (a *App) handlePlaylistClean(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		yt, err := a.youtube()
		if err != nil {
			fail(w, http.StatusUnauthorized, "Can't clean up", err)
			return
		}
		a.startJob(w, r, "Removing unavailable and duplicate videos", func(ctx context.Context, progress func(int, int)) error {
			ps, err := a.store.Playlists(ctx)
			if err != nil {
				return err
			}
			var items []string
			for _, p := range ps.Playlists {
				if id != "" && p.ID != id {
					continue
				}
				for _, it := range p.Items {
					if it.Unavailable {
						items = append(items, it.ID)
					}
				}
				for _, it := range p.Duplicates {
					items = append(items, it.ID)
				}
			}
			affordable := max(0, a.quotaLimit-ps.QuotaUsed) / youtube.CostWrite
			skipped := 0
			if len(items) > affordable {
				skipped = len(items) - affordable
				items = items[:affordable]
			}
			removed, err := a.removeItems(ctx, yt, items, progress)
			if err != nil {
				var apiErr *youtube.APIError
				if !errors.As(err, &apiErr) || !apiErr.QuotaExceeded() {
					return err
				}
				skipped += len(items) - len(removed)
			}
			if skipped > 0 {
				a.warn(fmt.Sprintf("Removed %d videos, then stopped before YouTube's daily quota ran out; %d are left. Run it again after midnight Pacific time.", len(removed), skipped))
			}
			return nil
		})
	}
}

// removeItems deletes playlist items one by one, recording each removal locally.
// It returns the IDs removed and stops at the first error.
func (a *App) removeItems(ctx context.Context, yt *youtube.Client, items []string, progress func(int, int)) ([]string, error) {
	progress(0, len(items))
	var removed []string
	defer func() {
		if len(removed) > 0 {
			a.store.RemovePlaylistItems(context.Background(), removed)
		}
	}()
	for i, id := range items {
		if err := yt.RemovePlaylistItem(ctx, id); err != nil {
			return removed, a.checkAuth(err)
		}
		removed = append(removed, id)
		progress(i+1, len(items))
	}
	return removed, nil
}

// handlePlaylistMerge copies the playlist's videos that the target lacks into the
// suggested target, deletes the source, then re-syncs playlists.
func (a *App) handlePlaylistMerge(w http.ResponseWriter, r *http.Request) {
	const title = "Can't merge"
	ps, err := a.store.Playlists(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	byID := map[string]store.Playlist{}
	for _, p := range ps.Playlists {
		byID[p.ID] = p
	}
	src, ok := byID[r.PathValue("id")]
	if !ok || src.Analysis == nil || src.Analysis.MergeWith == "" {
		fail(w, http.StatusNotFound, title, errors.New("there's no merge suggestion for that playlist; sync and analyze again"))
		return
	}
	target, ok := byID[src.Analysis.MergeWith]
	if !ok {
		fail(w, http.StatusNotFound, title, errors.New("the suggested target playlist no longer exists"))
		return
	}
	moves := views.MergeMoves(src, target)
	if err := a.ensureQuota(r.Context(), (moves+1)*youtube.CostWrite); err != nil {
		fail(w, http.StatusTooManyRequests, title, err)
		return
	}
	yt, err := a.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, title, err)
		return
	}
	a.startJob(w, r, fmt.Sprintf("Merging %s into %s", src.Title, target.Title), func(ctx context.Context, progress func(int, int)) error {
		have := map[string]bool{}
		for _, it := range target.Items {
			have[it.VideoID] = true
		}
		progress(0, moves+1)
		done := 0
		for _, it := range src.Items {
			if it.Unavailable || have[it.VideoID] {
				continue
			}
			if err := yt.AddPlaylistItem(ctx, target.ID, it.VideoID); err != nil {
				a.syncPlaylists(context.Background(), yt, func(int, int) {}) // show what did get copied
				return fmt.Errorf("copied %d of %d videos, then: %w (%s was not deleted)", done, moves, a.checkAuth(err), src.Title)
			}
			have[it.VideoID] = true
			done++
			progress(done, moves+1)
		}
		if err := yt.DeletePlaylist(ctx, src.ID); err != nil {
			return fmt.Errorf("copied all videos but couldn't delete %s: %w", src.Title, a.checkAuth(err))
		}
		progress(moves+1, moves+1)
		a.mu.Lock()
		a.flash = fmt.Sprintf("Merged %s into %s (%d videos copied).", src.Title, target.Title, done)
		a.mu.Unlock()
		return a.syncPlaylists(ctx, yt, func(int, int) {})
	})
}

func (a *App) handlePlaylistDelete(w http.ResponseWriter, r *http.Request) {
	const title = "Couldn't delete the playlist"
	id := r.PathValue("id")
	if err := a.ensureQuota(r.Context(), youtube.CostWrite); err != nil {
		fail(w, http.StatusTooManyRequests, title, err)
		return
	}
	yt, err := a.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, title, err)
		return
	}
	if err := yt.DeletePlaylist(r.Context(), id); err != nil {
		if err = a.checkAuth(err); errors.Is(err, ErrReconnect) {
			w.Header().Set("HX-Refresh", "true")
		}
		fail(w, http.StatusBadGateway, title, err)
		return
	}
	if err := a.store.DeletePlaylist(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	ps, err := a.store.Playlists(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	// Empty body removes the row; the summary updates out of band.
	render(w, r, views.PlaylistSummary(views.ComputePlaylistStats(ps, a.quotaLimit), true))
}

func (a *App) handlePlaylistDecision(verdict string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const title = "Couldn't save your decision"
		id := r.PathValue("id")
		if err := a.store.SetPlaylistDecision(r.Context(), id, verdict); err != nil {
			fail(w, http.StatusInternalServerError, title, err)
			return
		}
		ps, err := a.store.Playlists(r.Context())
		if err != nil {
			fail(w, http.StatusInternalServerError, title, err)
			return
		}
		list := views.BuildPlaylistList(ps.Playlists, "", "", "")
		p, ok := list.ByID[id]
		if !ok {
			fail(w, http.StatusNotFound, title, errors.New("that playlist isn't in your synced playlists"))
			return
		}
		render(w, r, views.PlaylistRow(p, list))
		render(w, r, views.PlaylistSummary(views.ComputePlaylistStats(ps, a.quotaLimit), true))
	}
}

// retry calls fn up to analyzeAttempts times with exponential backoff, except for
// non-retryable HTTP errors (bad request, auth, unknown model).
func retry[T any](ctx context.Context, backoff time.Duration, fn func() (T, error)) (T, error) {
	if backoff == 0 {
		backoff = 2 * time.Second
	}
	var (
		v   T
		err error
	)
	for attempt := 1; attempt <= analyzeAttempts; attempt++ {
		if v, err = fn(); err == nil {
			return v, nil
		}
		var httpErr *ai.HTTPError
		if errors.As(err, &httpErr) && !httpErr.Retryable() {
			return v, err
		}
		if attempt == analyzeAttempts {
			break
		}
		select {
		case <-time.After(backoff):
			backoff *= 2
		case <-ctx.Done():
			return v, ctx.Err()
		}
	}
	return v, err
}
