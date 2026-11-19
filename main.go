package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // quota day is Pacific time; distroless images have no zoneinfo

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"yt-subscriptions/internal/ai"
	"yt-subscriptions/internal/store"
	"yt-subscriptions/internal/youtube"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	healthcheck := flag.Bool("healthcheck", false, "check that the server on $PORT is healthy, then exit 0 (healthy) or 1 (for container health checks)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	loadDotEnv(".env")

	port := cmp.Or(os.Getenv("PORT"), "8080")
	if *healthcheck {
		os.Exit(runHealthcheck(port))
	}

	host := cmp.Or(os.Getenv("HOST"), "127.0.0.1")
	password := os.Getenv("DASHBOARD_PASSWORD")
	if err := checkExposure(host, password, envBool("ALLOW_NO_PASSWORD")); err != nil {
		log.Fatal(err)
	}
	// PUBLIC_URL is where users reach the dashboard (e.g. https://subs.example.com);
	// it sets the OAuth redirect unless OAUTH_REDIRECT_URL overrides it.
	publicURL := strings.TrimRight(cmp.Or(os.Getenv("PUBLIC_URL"), "http://localhost:"+port), "/")
	redirectURL := cmp.Or(os.Getenv("OAUTH_REDIRECT_URL"), publicURL+"/auth/callback")
	st, err := store.Open(cmp.Or(os.Getenv("DATA_DIR"), "data"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app := &App{
		ctx:   ctx,
		store: st,
		oauth: &oauth2.Config{
			ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
			ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			RedirectURL:  redirectURL,
			Scopes:       []string{youtube.Scope},
			Endpoint:     google.Endpoint,
		},
		ai: &ai.Client{
			BaseURL:  cmp.Or(os.Getenv("AI_BASE_URL"), os.Getenv("OPENAI_BASE_URL"), "https://api.openai.com/v1"),
			APIKey:   cmp.Or(os.Getenv("AI_API_KEY"), os.Getenv("OPENAI_API_KEY")),
			Model:    cmp.Or(os.Getenv("AI_MODEL"), os.Getenv("OPENAI_MODEL")),
			HTTP:     &http.Client{Timeout: 3 * time.Minute},
			JSONMode: envBool("AI_JSON_MODE"),
		},
		oauthStates: map[string]time.Time{},
		quotaLimit:  envInt("YOUTUBE_DAILY_QUOTA", 10_000),
	}

	srv := &http.Server{
		Addr:              host + ":" + port,
		Handler:           withHealth(securityHeaders(secure(app.routes(), password)), st),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute, // watch-history uploads can be large
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		log.Print("shutting down…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("yt-subscriptions %s listening on %s:%s (public URL %s)", version, host, port, publicURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	app.jobs.Wait() // background jobs see ctx cancelled and stop at their next step
	if err := st.Close(); err != nil {
		log.Printf("close store: %v", err)
	}
}

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes"
}

// checkExposure refuses to serve on a non-loopback address without a password,
// unless explicitly allowed (e.g. a container whose port is published on
// 127.0.0.1 only, or access restricted by a proxy).
func checkExposure(host, password string, allowNoPassword bool) error {
	if isLoopback(host) || password != "" || allowNoPassword {
		return nil
	}
	return fmt.Errorf("refusing to listen on %s without DASHBOARD_PASSWORD: anyone who can reach this port could manage your subscriptions. "+
		"Set DASHBOARD_PASSWORD, or set ALLOW_NO_PASSWORD=1 if access is already restricted (e.g. the port is only published on 127.0.0.1)", host)
}

// runHealthcheck probes /healthz on the local server; used by the image's HEALTHCHECK
// since distroless images have no curl or wget.
func runHealthcheck(port string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.Status)
		return 1
	}
	return 0
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
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
