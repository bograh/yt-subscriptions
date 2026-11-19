package main

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

func TestCheckExposure(t *testing.T) {
	cases := []struct {
		host, password string
		allow, ok      bool
	}{
		{"127.0.0.1", "", false, true},
		{"localhost", "", false, true},
		{"::1", "", false, true},
		{"0.0.0.0", "", false, false},
		{"0.0.0.0", "secret", false, true},
		{"0.0.0.0", "", true, true},
	}
	for _, c := range cases {
		if err := checkExposure(c.host, c.password, c.allow); (err == nil) != c.ok {
			t.Errorf("checkExposure(%q, %q, %v) = %v", c.host, c.password, c.allow, err)
		}
	}
}
