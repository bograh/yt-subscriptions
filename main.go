package main

import (
	"bufio"
	"cmp"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"yt-subscriptions/internal/ai"
	"yt-subscriptions/internal/store"
	"yt-subscriptions/internal/youtube"
)

func main() {
	loadDotEnv(".env")

	port := cmp.Or(os.Getenv("PORT"), "8080")
	st, err := store.Open(cmp.Or(os.Getenv("DATA_DIR"), "data"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	app := &App{
		store: st,
		oauth: &oauth2.Config{
			ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
			ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			RedirectURL:  cmp.Or(os.Getenv("OAUTH_REDIRECT_URL"), "http://localhost:"+port+"/auth/callback"),
			Scopes:       []string{youtube.Scope},
			Endpoint:     google.Endpoint,
		},
		ai: &ai.Client{
			BaseURL: cmp.Or(os.Getenv("AI_BASE_URL"), os.Getenv("OPENAI_BASE_URL"), "https://api.openai.com/v1"),
			APIKey:  cmp.Or(os.Getenv("AI_API_KEY"), os.Getenv("OPENAI_API_KEY")),
			Model:   cmp.Or(os.Getenv("AI_MODEL"), os.Getenv("OPENAI_MODEL")),
			HTTP:    &http.Client{Timeout: 3 * time.Minute},
		},
		oauthStates: map[string]time.Time{},
	}

	host := cmp.Or(os.Getenv("HOST"), "127.0.0.1")
	password := os.Getenv("DASHBOARD_PASSWORD")
	if !isLoopback(host) && password == "" {
		log.Printf("WARNING: listening on %s without DASHBOARD_PASSWORD; anyone who can reach this port can manage your subscriptions", host)
	}

	srv := &http.Server{
		Addr:              host + ":" + port,
		Handler:           secure(app.routes(), password),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("dashboard on http://localhost:%s", port)
	log.Fatal(srv.ListenAndServe())
}

// loadDotEnv sets KEY=VALUE pairs from a .env file without overriding the real environment.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}
