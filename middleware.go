package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
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

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
