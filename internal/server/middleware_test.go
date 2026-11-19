package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestHealthzSkipsAuthAndReportsDB(t *testing.T) {
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("app")) })
	h := withHealth(securityHeaders(secure(app, "pw")), fakePinger{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz without password: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("dashboard without password: %d, want 401", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("security headers missing on the 401 response")
	}

	down := withHealth(app, fakePinger{err: errors.New("closed")})
	rec = httptest.NewRecorder()
	down.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz with a dead database: %d, want 503", rec.Code)
	}
}

// The full handler serves the dashboard, both pages and the embedded assets.
func TestHandlerServesPagesAndAssets(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	for _, path := range []string{"/", "/playlists", "/static/app.css", "/static/htmx.min.js", "/static/fonts/Geist-Variable.woff2"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("GET %s: %d, %d bytes", path, rec.Code, rec.Body.Len())
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/static.go", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("Go source must not be served: %d", rec.Code)
	}
}
