package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"yt-subscriptions/internal/ai"
	"yt-subscriptions/internal/store"
)

// Tuning for AI analysis. Smaller batches mean more requests but less lost
// when the model skips items or returns unparseable output.
const (
	channelBatchSize   = 25
	playlistBatchSize  = 20
	analyzeConcurrency = 3 // batches in flight at once
	analyzeAttempts    = 3 // tries per request, with exponential backoff
	defaultBackoff     = 2 * time.Second
)

// analysis describes one kind of AI analysis: items of type T get results of
// type R. channelAnalysis and playlistAnalysis are the two instances; a new
// kind of item only needs a new constructor like those.
type analysis[T, R any] struct {
	noun      string // plural, for messages: "channels"
	batchSize int
	backoff   time.Duration
	id        func(T) string
	analyze   func(ctx context.Context, batch []T) (map[string]R, error)
	save      func(ctx context.Context, results map[string]R) error
}

func (s *Server) channelAnalysis(st store.State) analysis[store.Channel, store.Analysis] {
	return analysis[store.Channel, store.Analysis]{
		noun:      "channels",
		batchSize: channelBatchSize,
		backoff:   s.retryBackoff,
		id:        func(c store.Channel) string { return c.ID },
		analyze: func(ctx context.Context, batch []store.Channel) (map[string]store.Analysis, error) {
			return s.ai.Analyze(ctx, st.Interests, st.HistoryImported, batch)
		},
		save: func(ctx context.Context, res map[string]store.Analysis) error {
			for id, an := range res {
				an.Basis = st.Basis // marks the suggestion outdated once interests or history change
				res[id] = an
			}
			return s.store.SaveAnalyses(ctx, res)
		},
	}
}

func (s *Server) playlistAnalysis(ps store.PlaylistState) analysis[store.Playlist, store.PlaylistAnalysis] {
	return analysis[store.Playlist, store.PlaylistAnalysis]{
		noun:      "playlists",
		batchSize: playlistBatchSize,
		backoff:   s.retryBackoff,
		id:        func(p store.Playlist) string { return p.ID },
		analyze: func(ctx context.Context, batch []store.Playlist) (map[string]store.PlaylistAnalysis, error) {
			// All playlists go along so the model can suggest merge targets.
			return s.ai.AnalyzePlaylists(ctx, ps.Interests, ps.Playlists, batch)
		},
		save: func(ctx context.Context, res map[string]store.PlaylistAnalysis) error {
			for id, an := range res {
				an.Basis = ps.Basis
				res[id] = an
			}
			return s.store.SavePlaylistAnalyses(ctx, res)
		},
	}
}

type analysisResult struct {
	noun     string
	total    int
	failed   int   // items left without an analysis
	firstErr error // first error that caused a failure
	saveErr  error // a database write failed, which stopped the run
}

// err is the error a job should report, or nil if every item was analyzed.
func (r analysisResult) err() error {
	if r.saveErr != nil {
		return r.saveErr
	}
	if r.failed > 0 {
		return fmt.Errorf("%d of %d %s couldn't be analyzed; run Analyze new to retry them. First error: %w", r.failed, r.total, r.noun, r.firstErr)
	}
	return nil
}

// runAnalysis sends todo to the model in batches, a few at a time. Each batch
// is retried with backoff on rate limits, server errors and unparseable
// output, and items the model skipped get one follow-up request. Results are
// saved per batch, so progress survives a later failure.
func runAnalysis[T, R any](ctx context.Context, job analysis[T, R], todo []T, progress func(done, total int)) analysisResult {
	var (
		mu   sync.Mutex
		res  = analysisResult{noun: job.noun, total: len(todo)}
		done int
		wg   sync.WaitGroup
		sem  = make(chan struct{}, analyzeConcurrency)
	)
	progress(0, len(todo))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for start := 0; start < len(todo); start += job.batchSize {
		batch := todo[start:min(start+job.batchSize, len(todo))]
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				res.failed += len(batch)
				res.firstErr = firstError(res.firstErr, ctx.Err())
				mu.Unlock()
				return
			}

			got, err := analyzeBatch(ctx, job, batch)
			if len(got) > 0 {
				if serr := job.save(ctx, got); serr != nil {
					mu.Lock()
					res.saveErr = firstError(res.saveErr, serr)
					mu.Unlock()
					cancel()
					return
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if missing := len(batch) - len(got); missing > 0 {
				res.failed += missing
				if err == nil {
					err = fmt.Errorf("the model skipped some %s twice", job.noun)
				}
				res.firstErr = firstError(res.firstErr, err)
			}
			done += len(batch)
			progress(done, len(todo))
		}()
	}
	wg.Wait()
	return res
}

// analyzeBatch returns whatever results it could get for batch: a retried
// request, then one follow-up request for any items the model left out.
func analyzeBatch[T, R any](ctx context.Context, job analysis[T, R], batch []T) (map[string]R, error) {
	call := func(b []T) (map[string]R, error) {
		return retry(ctx, job.backoff, func() (map[string]R, error) { return job.analyze(ctx, b) })
	}
	got, err := call(batch)
	if err != nil {
		return nil, err
	}
	var missing []T
	for _, item := range batch {
		if _, ok := got[job.id(item)]; !ok {
			missing = append(missing, item)
		}
	}
	if len(missing) == 0 {
		return got, nil
	}
	more, err := call(missing)
	for id, r := range more {
		got[id] = r
	}
	return got, err
}

// retry calls fn up to analyzeAttempts times with exponential backoff, except
// for non-retryable HTTP errors (bad request, auth, unknown model).
func retry[T any](ctx context.Context, backoff time.Duration, fn func() (T, error)) (T, error) {
	if backoff == 0 {
		backoff = defaultBackoff
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

// firstError keeps the first non-nil error.
func firstError(current, next error) error {
	if current != nil {
		return current
	}
	return next
}
