// Command yt-subscriptions runs the YouTube Cleanup dashboard.
//
// It only wires things together: settings come from internal/config, the web
// app lives in internal/server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // quota day is Pacific time; distroless images have no zoneinfo

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"yt-subscriptions/internal/ai"
	"yt-subscriptions/internal/config"
	"yt-subscriptions/internal/server"
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

	config.LoadDotEnv(".env")
	cfg := config.FromEnv()
	if *healthcheck {
		os.Exit(runHealthcheck(cfg.Port))
	}
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}

func run(cfg config.Config) error {
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("close store: %v", err)
		}
	}()

	var analyzer server.Analyzer // stays nil (analysis disabled) without a model
	if cfg.AI.Model != "" {
		analyzer = &ai.Client{
			BaseURL:  cfg.AI.BaseURL,
			APIKey:   cfg.AI.APIKey,
			Model:    cfg.AI.Model,
			JSONMode: cfg.AI.JSONMode,
			HTTP:     &http.Client{Timeout: 3 * time.Minute},
		}
	}

	app := server.New(server.Options{
		Store: st,
		OAuth: &oauth2.Config{
			ClientID:     cfg.Google.ClientID,
			ClientSecret: cfg.Google.ClientSecret,
			RedirectURL:  cfg.Google.RedirectURL,
			Scopes:       []string{youtube.Scope},
			Endpoint:     google.Endpoint,
		},
		AI:         analyzer,
		QuotaLimit: cfg.YouTubeDailyQuota,
		Password:   cfg.Password,
	})
	defer app.Close() // cancels running jobs and waits for them, before the store closes

	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute, // watch-history uploads can be large
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Print("shutting down…")
		app.Close() // stop background jobs at their next step
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("yt-subscriptions %s listening on %s (public URL %s)", version, cfg.Addr(), cfg.PublicURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
