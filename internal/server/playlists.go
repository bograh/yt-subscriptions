package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"yt-subscriptions/internal/store"
	"yt-subscriptions/internal/youtube"
	"yt-subscriptions/views"
)

func (s *Server) playlistPage(r *http.Request) (views.PlaylistPage, error) {
	ps, err := s.store.Playlists(r.Context())
	if err != nil {
		return views.PlaylistPage{}, err
	}
	q := r.URL.Query()
	return views.PlaylistPage{
		Status: s.status(),
		State:  ps,
		Stats:  views.ComputePlaylistStats(ps, s.quotaLimit),
		List:   views.BuildPlaylistList(ps.Playlists, q.Get("group"), q.Get("verdict"), q.Get("q")),
	}, nil
}

func (s *Server) handlePlaylists(w http.ResponseWriter, r *http.Request) {
	p, err := s.playlistPage(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, views.PlaylistsPage(p))
}

func (s *Server) handlePlaylistList(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "" {
		s.handlePlaylists(w, r)
		return
	}
	p, err := s.playlistPage(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, views.PlaylistRows(p.List))
}

func (s *Server) handlePlaylistSync(w http.ResponseWriter, r *http.Request) {
	yt, err := s.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, "Can't sync playlists", err)
		return
	}
	s.startJob(w, r, "Syncing playlists", func(ctx context.Context, progress func(int, int)) error {
		return s.syncPlaylists(ctx, yt, progress)
	})
}

func (s *Server) syncPlaylists(ctx context.Context, yt *youtube.Client, progress func(int, int)) error {
	playlists, err := yt.FetchPlaylists(ctx, progress)
	if err != nil {
		return s.checkAuth(err) // nothing is written on failure
	}
	return s.store.ReplacePlaylists(ctx, playlists)
}

func (s *Server) handlePlaylistAnalyze(w http.ResponseWriter, r *http.Request) {
	if s.ai == nil {
		fail(w, http.StatusBadRequest, "Can't analyze", errors.New("AI_MODEL is not configured"))
		return
	}
	all := r.URL.Query().Get("all") == "1"
	s.startJob(w, r, "Analyzing playlists with AI", func(ctx context.Context, progress func(int, int)) error {
		ps, err := s.store.Playlists(ctx)
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
		return runAnalysis(ctx, s.playlistAnalysis(ps), todo, progress).err()
	})
}

// handlePlaylistClean removes unavailable and duplicate videos, from one
// playlist ({id}) or from all of them (no {id}), within today's quota.
func (s *Server) handlePlaylistClean(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	yt, err := s.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, "Can't clean up", err)
		return
	}
	s.startJob(w, r, "Removing unavailable and duplicate videos", func(ctx context.Context, progress func(int, int)) error {
		ps, err := s.store.Playlists(ctx)
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
		affordable := max(0, s.quotaLimit-ps.QuotaUsed) / youtube.CostWrite
		skipped := 0
		if len(items) > affordable {
			skipped = len(items) - affordable
			items = items[:affordable]
		}
		removed, err := s.removeItems(ctx, yt, items, progress)
		if err != nil {
			var apiErr *youtube.APIError
			if !errors.As(err, &apiErr) || !apiErr.QuotaExceeded() {
				return err
			}
			skipped += len(items) - len(removed)
		}
		if skipped > 0 {
			s.warn(fmt.Sprintf("Removed %d videos, then stopped before YouTube's daily quota ran out; %d are left. Run it again after midnight Pacific time.", len(removed), skipped))
		}
		return nil
	})
}

// removeItems deletes playlist items one by one, recording each removal locally.
// It returns the IDs removed and stops at the first error.
func (s *Server) removeItems(ctx context.Context, yt *youtube.Client, items []string, progress func(int, int)) ([]string, error) {
	progress(0, len(items))
	var removed []string
	defer func() {
		if len(removed) > 0 {
			s.store.RemovePlaylistItems(context.Background(), removed)
		}
	}()
	for i, id := range items {
		if err := yt.RemovePlaylistItem(ctx, id); err != nil {
			return removed, s.checkAuth(err)
		}
		removed = append(removed, id)
		progress(i+1, len(items))
	}
	return removed, nil
}

// handlePlaylistMerge copies the playlist's videos that the target lacks into the
// suggested target, deletes the source, then re-syncs playlists.
func (s *Server) handlePlaylistMerge(w http.ResponseWriter, r *http.Request) {
	const title = "Can't merge"
	ps, err := s.store.Playlists(r.Context())
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
	if err := s.ensureQuota(r.Context(), (moves+1)*youtube.CostWrite); err != nil {
		fail(w, http.StatusTooManyRequests, title, err)
		return
	}
	yt, err := s.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, title, err)
		return
	}
	s.startJob(w, r, fmt.Sprintf("Merging %s into %s", src.Title, target.Title), func(ctx context.Context, progress func(int, int)) error {
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
				s.syncPlaylists(context.Background(), yt, func(int, int) {}) // show what did get copied
				return fmt.Errorf("copied %d of %d videos, then: %w (%s was not deleted)", done, moves, s.checkAuth(err), src.Title)
			}
			have[it.VideoID] = true
			done++
			progress(done, moves+1)
		}
		if err := yt.DeletePlaylist(ctx, src.ID); err != nil {
			return fmt.Errorf("copied all videos but couldn't delete %s: %w", src.Title, s.checkAuth(err))
		}
		progress(moves+1, moves+1)
		s.setFlash(fmt.Sprintf("Merged %s into %s (%d videos copied).", src.Title, target.Title, done))
		return s.syncPlaylists(ctx, yt, func(int, int) {})
	})
}

func (s *Server) handlePlaylistDelete(w http.ResponseWriter, r *http.Request) {
	const title = "Couldn't delete the playlist"
	id := r.PathValue("id")
	if err := s.ensureQuota(r.Context(), youtube.CostWrite); err != nil {
		fail(w, http.StatusTooManyRequests, title, err)
		return
	}
	yt, err := s.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, title, err)
		return
	}
	if err := yt.DeletePlaylist(r.Context(), id); err != nil {
		if err = s.checkAuth(err); errors.Is(err, ErrReconnect) {
			w.Header().Set("HX-Refresh", "true")
		}
		fail(w, http.StatusBadGateway, title, err)
		return
	}
	if err := s.store.DeletePlaylist(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	ps, err := s.store.Playlists(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	// Empty body removes the row; the summary updates out of band.
	render(w, r, views.PlaylistSummary(views.ComputePlaylistStats(ps, s.quotaLimit), true))
}

func (s *Server) handlePlaylistDecision(verdict string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const title = "Couldn't save your decision"
		id := r.PathValue("id")
		if err := s.store.SetPlaylistDecision(r.Context(), id, verdict); err != nil {
			fail(w, http.StatusInternalServerError, title, err)
			return
		}
		ps, err := s.store.Playlists(r.Context())
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
		render(w, r, views.PlaylistSummary(views.ComputePlaylistStats(ps, s.quotaLimit), true))
	}
}
