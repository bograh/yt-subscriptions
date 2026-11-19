package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"yt-subscriptions/internal/store"
	"yt-subscriptions/internal/youtube"
	"yt-subscriptions/views"
)

// maxHistoryUpload caps the Takeout watch-history upload.
const maxHistoryUpload = 512 << 20

func (s *Server) page(r *http.Request) (views.Page, error) {
	st, err := s.store.Snapshot(r.Context())
	if err != nil {
		return views.Page{}, err
	}
	q := r.URL.Query()
	return views.Page{
		Status: s.status(),
		State:  st,
		Stats:  views.ComputeStats(st, s.quotaLimit),
		List:   views.BuildList(st.Channels, q.Get("group"), q.Get("verdict"), q.Get("q")),
	}, nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	p, err := s.page(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, views.Dashboard(p))
}

// handleChannels re-renders the channel list when filters change.
func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	// A full page load of this URL (e.g. browser refresh after hx-push-url) gets the whole dashboard.
	if r.Header.Get("HX-Request") == "" {
		s.handleIndex(w, r)
		return
	}
	p, err := s.page(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, views.ChannelList(p.List))
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	yt, err := s.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, "Can't sync", err)
		return
	}
	s.startJob(w, r, "Syncing subscriptions", func(ctx context.Context, progress func(int, int)) error {
		fresh, uploadFailures, err := yt.FetchSubscriptions(ctx, progress)
		if err != nil {
			// Nothing is written: a quota or auth failure must not replace good data.
			return s.checkAuth(err)
		}
		if len(uploadFailures) > 0 {
			if err := s.keepPreviousUploads(ctx, fresh, uploadFailures); err != nil {
				return err
			}
			s.warn(fmt.Sprintf("Synced, but recent uploads couldn't be fetched for %d channel(s); their previous upload info was kept.", len(uploadFailures)))
		}
		// Analyses and watch stats live in their own tables, so they carry over automatically.
		return s.store.ReplaceChannels(ctx, fresh)
	})
}

// keepPreviousUploads copies last sync's upload info into fresh for the
// channels whose uploads couldn't be fetched, instead of making them look inactive.
func (s *Server) keepPreviousUploads(ctx context.Context, fresh []store.Channel, failedIDs []string) error {
	prev, err := s.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	old := make(map[string]store.Channel, len(prev.Channels))
	for _, c := range prev.Channels {
		old[c.ID] = c
	}
	failed := make(map[string]bool, len(failedIDs))
	for _, id := range failedIDs {
		failed[id] = true
	}
	for i := range fresh {
		if p, ok := old[fresh[i].ID]; ok && failed[fresh[i].ID] {
			fresh[i].LastUpload, fresh[i].RecentTitles = p.LastUpload, p.RecentTitles
		}
	}
	return nil
}

func (s *Server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	if s.ai == nil {
		fail(w, http.StatusBadRequest, "Can't analyze", errors.New("AI_MODEL is not configured"))
		return
	}
	all := r.URL.Query().Get("all") == "1"
	s.startJob(w, r, "Analyzing channels with AI", func(ctx context.Context, progress func(int, int)) error {
		st, err := s.store.Snapshot(ctx)
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
		return runAnalysis(ctx, s.channelAnalysis(st), todo, progress).err()
	})
}

func (s *Server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	const title = "Couldn't unsubscribe"
	subID, err := s.store.SubscriptionID(r.Context(), id)
	if err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	if subID == "" {
		fail(w, http.StatusNotFound, title, errors.New("that channel isn't in your synced subscriptions"))
		return
	}
	if err := s.ensureQuota(r.Context(), youtube.CostUnsubscribe); err != nil {
		fail(w, http.StatusTooManyRequests, title, err)
		return
	}
	yt, err := s.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, title, err)
		return
	}
	if err := yt.Unsubscribe(r.Context(), subID); err != nil {
		if err = s.checkAuth(err); errors.Is(err, ErrReconnect) {
			w.Header().Set("HX-Refresh", "true") // re-render with the Connect button
			fail(w, http.StatusUnauthorized, title, err)
			return
		}
		fail(w, http.StatusBadGateway, title, err)
		return
	}
	if err := s.store.DeleteChannel(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	st, err := s.store.Snapshot(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	// Empty body removes the card; stats are refreshed out-of-band.
	render(w, r, views.Summary(views.ComputeStats(st, s.quotaLimit), true))
}

// handleDecision pins (or, with "", clears) the user's own verdict for a channel
// and re-renders its row plus the summary.
func (s *Server) handleDecision(verdict string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const title = "Couldn't save your decision"
		id := r.PathValue("id")
		if err := s.store.SetDecision(r.Context(), id, verdict); err != nil {
			fail(w, http.StatusInternalServerError, title, err)
			return
		}
		st, err := s.store.Snapshot(r.Context())
		if err != nil {
			fail(w, http.StatusInternalServerError, title, err)
			return
		}
		for _, c := range st.Channels {
			if c.ID == id {
				render(w, r, views.ChannelRow(c))
				render(w, r, views.Summary(views.ComputeStats(st, s.quotaLimit), true))
				return
			}
		}
		fail(w, http.StatusNotFound, title, errors.New("that channel isn't in your synced subscriptions"))
	}
}

// handleBulkUnsubscribe unsubscribes from every channel whose effective verdict is
// "unsubscribe", as a background job, stopping before the daily quota runs out.
func (s *Server) handleBulkUnsubscribe(w http.ResponseWriter, r *http.Request) {
	s.startUnsubscribeJob(w, r, "Unsubscribing from suggested channels", func(c store.Channel) bool {
		return c.Verdict() == store.VerdictUnsubscribe
	})
}

// handleChannelBulk applies one action to the channels selected on the page
// (form values: action, ids).
func (s *Server) handleChannelBulk(w http.ResponseWriter, r *http.Request) {
	ids, ok := selectedIDs(w, r, "channels")
	if !ok {
		return
	}
	switch action := r.FormValue("action"); action {
	case "keep", "undo":
		verdict, done := store.VerdictKeep, "Kept"
		if action == "undo" {
			verdict, done = "", "Cleared your decision for"
		}
		if err := s.store.SetDecisions(r.Context(), ids, verdict); err != nil {
			fail(w, http.StatusInternalServerError, "Couldn't save your decisions", err)
			return
		}
		s.setFlash(fmt.Sprintf("%s %s.", done, views.Plural(len(ids), "channel", "channels")))
		w.Header().Set("HX-Refresh", "true")
	case "unsubscribe":
		selected := set(ids)
		s.startUnsubscribeJob(w, r, "Unsubscribing from selected channels", func(c store.Channel) bool {
			return selected[c.ID]
		})
	default:
		fail(w, http.StatusBadRequest, "Unknown action", fmt.Errorf("%q isn't a bulk action for channels", action))
	}
}

// startUnsubscribeJob unsubscribes, in the background, from every synced
// channel that match accepts, stopping before the daily quota runs out.
func (s *Server) startUnsubscribeJob(w http.ResponseWriter, r *http.Request, label string, match func(store.Channel) bool) {
	yt, err := s.youtube()
	if err != nil {
		fail(w, http.StatusUnauthorized, "Can't unsubscribe", err)
		return
	}
	s.startJob(w, r, label, func(ctx context.Context, progress func(int, int)) error {
		st, err := s.store.Snapshot(ctx)
		if err != nil {
			return err
		}
		var todo []store.Channel
		for _, c := range st.Channels {
			if match(c) {
				todo = append(todo, c)
			}
		}
		affordable := max(0, s.quotaLimit-st.QuotaUsed) / youtube.CostUnsubscribe
		skipped := 0
		if len(todo) > affordable {
			skipped = len(todo) - affordable
			todo = todo[:affordable]
		}
		progress(0, len(todo))
		var failed []string
		for i, c := range todo {
			if err := yt.Unsubscribe(ctx, c.SubscriptionID); err != nil {
				if err = s.checkAuth(err); errors.Is(err, ErrReconnect) {
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
			} else if err := s.store.DeleteChannel(ctx, c.ID); err != nil {
				return err
			}
			progress(i+1, len(todo))
		}
		switch {
		case len(failed) > 0:
			return fmt.Errorf("couldn't unsubscribe from %d channel(s): %s", len(failed), strings.Join(failed, ", "))
		case skipped > 0:
			s.warn(fmt.Sprintf("Stopped before YouTube's daily quota ran out; %d channel(s) are still subscribed. Run it again after midnight Pacific time.", skipped))
		}
		return nil
	})
}

func (s *Server) handleInterests(w http.ResponseWriter, r *http.Request) {
	interests := r.FormValue("interests")
	if err := s.store.SetInterests(r.Context(), interests); err != nil {
		fail(w, http.StatusInternalServerError, "Couldn't save interests", err)
		return
	}
	w.Header().Set("HX-Refresh", "true") // suggestions may now be outdated
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxHistoryUpload)
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
	matched, err := s.store.ImportWatchStats(r.Context(), stats)
	if err != nil {
		fail(w, http.StatusInternalServerError, title, err)
		return
	}
	s.setFlash(fmt.Sprintf("Watch history imported: you've watched %d of your subscribed channels. Analyze new to update the suggestions.", matched))
	w.Header().Set("HX-Refresh", "true")
}
