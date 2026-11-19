package main

import (
	"embed"
	"net/http"
)

// Built assets (Tailwind output, htmx) are embedded so the binary is self-contained.
//
//go:embed static
var staticFS embed.FS

func staticHandler() http.Handler {
	fs := http.FileServerFS(staticFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fs.ServeHTTP(w, r)
	})
}
