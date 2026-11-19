package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"yt-subscriptions/views"
)

// jobTimeout bounds any single background job.
const jobTimeout = 30 * time.Minute

// jobFunc is the work of a background job. It reports progress as it goes;
// a returned error is shown on the next page load.
type jobFunc func(ctx context.Context, progress func(done, total int)) error

// startJob runs fn in the background and renders the progress banner, which
// polls GET /job. Only one job runs at a time: if one is already running, its
// banner is rendered instead.
func (s *Server) startJob(w http.ResponseWriter, r *http.Request, label string, fn jobFunc) {
	s.mu.Lock()
	if s.job.Running {
		job := s.job
		s.mu.Unlock()
		render(w, r, views.JobBanner(job))
		return
	}
	s.job = views.Job{Running: true, Label: label}
	s.lastErr, s.lastWarn = "", ""
	job := s.job
	s.mu.Unlock()

	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		ctx, cancel := context.WithTimeout(s.ctx, jobTimeout)
		defer cancel()
		err := fn(ctx, func(done, total int) {
			s.mu.Lock()
			s.job.Done, s.job.Total = done, total
			s.mu.Unlock()
		})
		s.mu.Lock()
		s.job = views.Job{}
		if err != nil {
			s.lastErr = fmt.Sprintf("%s failed: %v", label, err)
			log.Print(s.lastErr)
		}
		s.mu.Unlock()
	}()
	render(w, r, views.JobBanner(job))
}

// handleJob renders the progress banner while a job runs, then tells HTMX to
// reload the page so it shows the results.
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	job := s.job
	s.mu.Unlock()
	if !job.Running {
		w.Header().Set("HX-Refresh", "true")
		return
	}
	render(w, r, views.JobBanner(job))
}

// warn records a non-fatal problem from a background job, shown on the next page load.
func (s *Server) warn(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastWarn = msg
	log.Print(msg)
}

// setFlash records a one-time notice, shown on the next page load.
func (s *Server) setFlash(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flash = msg
}
