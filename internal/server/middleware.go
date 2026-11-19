package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"time"

	"yt-subscriptions/static"
)

// secure wraps the app with cross-origin (CSRF) protection and, when a password
// is configured, HTTP basic auth.
func secure(h http.Handler, password string) http.Handler {
	// Rejects state-changing requests (POST/DELETE/...) sent by other websites,
	// using Sec-Fetch-Site / Origin headers.
	h = http.NewCrossOriginProtection().Handler(h)
	if password == "" {
		return h
	}
	want := sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		got := sha256.Sum256([]byte(pass))
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Subscription Cleanup", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// securityHeaders sets browser hardening headers on every response.
func securityHeaders(h http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"img-src 'self' https: data:; " + // channel avatars come from YouTube's CDN
		"style-src 'self' 'unsafe-inline'; " + // inline style attributes (progress bar widths)
		"script-src 'self'; connect-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", csp)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.ServeHTTP(w, r)
	})
}

type pinger interface {
	Ping(ctx context.Context) error
}

// withHealth serves GET /healthz before auth, so orchestrators and the image's
// HEALTHCHECK can probe it without the dashboard password. It reveals nothing
// beyond whether the database answers.
func withHealth(h http.Handler, db pinger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			h.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Cache-Control", "no-store")
		if err := db.Ping(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
}

// staticHandler serves the embedded assets under /static/. They aren't
// fingerprinted, so browsers revalidate them on each load.
func staticHandler() http.Handler {
	fs := http.StripPrefix("/static/", http.FileServerFS(static.FS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fs.ServeHTTP(w, r)
	})
}
