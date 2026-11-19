// Package config reads every setting the app understands from the environment
// (and an optional .env file). It is the one place to look when adding or
// renaming a setting; .env.example documents the same variables for users.
package config

import (
	"bufio"
	"cmp"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Config is the app's whole configuration.
type Config struct {
	Host      string // HOST: interface to listen on (default 127.0.0.1)
	Port      string // PORT (default 8080)
	PublicURL string // PUBLIC_URL: where users open the dashboard (default http://localhost:PORT)
	DataDir   string // DATA_DIR: where the SQLite database lives (default data)

	Password        string // DASHBOARD_PASSWORD: HTTP basic auth; required off loopback
	AllowNoPassword bool   // ALLOW_NO_PASSWORD: skip that requirement (access restricted elsewhere)

	Google Google
	AI     AI

	YouTubeDailyQuota int // YOUTUBE_DAILY_QUOTA: API units per day (default 10000)
}

// Google is the OAuth client used to sign in to YouTube.
type Google struct {
	ClientID     string // GOOGLE_CLIENT_ID
	ClientSecret string // GOOGLE_CLIENT_SECRET
	RedirectURL  string // OAUTH_REDIRECT_URL (default PUBLIC_URL/auth/callback)
}

// Ready reports whether sign-in can work at all.
func (g Google) Ready() bool { return g.ClientID != "" && g.ClientSecret != "" }

// AI is any OpenAI-compatible chat completions endpoint.
type AI struct {
	BaseURL  string // AI_BASE_URL (or OPENAI_BASE_URL; default https://api.openai.com/v1)
	APIKey   string // AI_API_KEY (or OPENAI_API_KEY)
	Model    string // AI_MODEL (or OPENAI_MODEL); empty disables analysis
	JSONMode bool   // AI_JSON_MODE: send response_format=json_object
}

// FromEnv builds a Config from environment variables, applying defaults.
func FromEnv() Config {
	port := cmp.Or(os.Getenv("PORT"), "8080")
	publicURL := strings.TrimRight(cmp.Or(os.Getenv("PUBLIC_URL"), "http://localhost:"+port), "/")
	return Config{
		Host:            cmp.Or(os.Getenv("HOST"), "127.0.0.1"),
		Port:            port,
		PublicURL:       publicURL,
		DataDir:         cmp.Or(os.Getenv("DATA_DIR"), "data"),
		Password:        os.Getenv("DASHBOARD_PASSWORD"),
		AllowNoPassword: envBool("ALLOW_NO_PASSWORD"),
		Google: Google{
			ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
			ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			RedirectURL:  cmp.Or(os.Getenv("OAUTH_REDIRECT_URL"), publicURL+"/auth/callback"),
		},
		AI: AI{
			BaseURL:  cmp.Or(os.Getenv("AI_BASE_URL"), os.Getenv("OPENAI_BASE_URL"), "https://api.openai.com/v1"),
			APIKey:   cmp.Or(os.Getenv("AI_API_KEY"), os.Getenv("OPENAI_API_KEY")),
			Model:    cmp.Or(os.Getenv("AI_MODEL"), os.Getenv("OPENAI_MODEL")),
			JSONMode: envBool("AI_JSON_MODE"),
		},
		YouTubeDailyQuota: envInt("YOUTUBE_DAILY_QUOTA", 10_000),
	}
}

// Addr is the host:port to listen on.
func (c Config) Addr() string { return net.JoinHostPort(c.Host, c.Port) }

// Validate refuses to serve on a non-loopback address without a password,
// unless explicitly allowed (e.g. a container whose port is published on
// 127.0.0.1 only, or access restricted by a proxy).
func (c Config) Validate() error {
	if isLoopback(c.Host) || c.Password != "" || c.AllowNoPassword {
		return nil
	}
	return fmt.Errorf("refusing to listen on %s without DASHBOARD_PASSWORD: anyone who can reach this port could manage your subscriptions. "+
		"Set DASHBOARD_PASSWORD, or set ALLOW_NO_PASSWORD=1 if access is already restricted (e.g. the port is only published on 127.0.0.1)", c.Host)
}

// LoadDotEnv sets KEY=VALUE pairs from a .env file without overriding the real
// environment. A missing file is not an error.
func LoadDotEnv(path string) {
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

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes"
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}
