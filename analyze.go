package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"yt-subscriptions/internal/ai"
	"yt-subscriptions/internal/store"
)

const (
	analyzeBatchSize   = 25
	analyzeConcurrency = 3
	analyzeAttempts    = 3
)

// analyzer is the slice of ai.Client the job needs (swapped out in tests).
type analyzer interface {
	Analyze(ctx context.Context, interests string, historyImported bool, channels []store.Channel) (map[string]store.Analysis, error)
}

type analysisResult struct {
	failed   int   // channels left without an analysis
	firstErr error // first error that caused a failure
	saveErr  error // a database write failed: the job should stop
}

// runAnalysis sends channels to the model in batches, a few at a time. Each batch
// is retried with backoff on rate limits / server errors / unparseable output,
// and channels the model skipped get one follow-up request. Results are saved
// per batch, so progress survives a later failure.
func (a *App) runAnalysis(ctx context.Context, st store.State, todo []store.Channel, progress func(done, total int)) analysisResult {
	var (
		mu   sync.Mutex
		res  analysisResult
		done int
		wg   sync.WaitGroup
		sem  = make(chan struct{}, analyzeConcurrency)
	)
	progress(0, len(todo))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for start := 0; start < len(todo); start += analyzeBatchSize {
		batch := todo[start:min(start+analyzeBatchSize, len(todo))]
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				res.failed += len(batch)
				res.firstErr = cmpErr(res.firstErr, ctx.Err())
				mu.Unlock()
				return
			}

			got, err := a.analyzeBatch(ctx, st, batch)
			if len(got) > 0 {
				if serr := a.store.SaveAnalyses(ctx, withBasis(got, st.Basis)); serr != nil {
					mu.Lock()
					res.saveErr = cmpErr(res.saveErr, serr)
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
					err = errors.New("the model skipped some channels twice")
				}
				res.firstErr = cmpErr(res.firstErr, err)
			}
			done += len(batch)
			progress(done, len(todo))
		}()
	}
	wg.Wait()
	return res
}

// analyzeBatch returns whatever analyses it could get for batch: a retried
// request, then one follow-up request for any channels the model left out.
func (a *App) analyzeBatch(ctx context.Context, st store.State, batch []store.Channel) (map[string]store.Analysis, error) {
	got, err := a.analyzeWithRetry(ctx, st, batch)
	if err != nil {
		return nil, err
	}
	var missing []store.Channel
	for _, c := range batch {
		if _, ok := got[c.ID]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return got, nil
	}
	more, err := a.analyzeWithRetry(ctx, st, missing)
	for id, an := range more {
		got[id] = an
	}
	return got, err
}

func (a *App) analyzeWithRetry(ctx context.Context, st store.State, batch []store.Channel) (map[string]store.Analysis, error) {
	backoff := a.retryBackoff
	if backoff == 0 {
		backoff = 2 * time.Second
	}
	var err error
	for attempt := 1; attempt <= analyzeAttempts; attempt++ {
		var got map[string]store.Analysis
		got, err = a.analyzer().Analyze(ctx, st.Interests, st.HistoryImported, batch)
		if err == nil {
			return got, nil
		}
		var httpErr *ai.HTTPError
		if errors.As(err, &httpErr) && !httpErr.Retryable() {
			return nil, err // bad request, auth, unknown model: retrying won't help
		}
		if attempt == analyzeAttempts {
			break
		}
		select {
		case <-time.After(backoff):
			backoff *= 2
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, err
}

func (a *App) analyzer() analyzer {
	if a.testAnalyzer != nil {
		return a.testAnalyzer
	}
	return a.ai
}

func cmpErr(current, next error) error {
	if current != nil {
		return current
	}
	return next
}
