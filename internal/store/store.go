// Package store holds the app's data model and persists it as JSON on disk.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	VerdictKeep        = "keep"
	VerdictReview      = "review"
	VerdictUnsubscribe = "unsubscribe"
)

type Analysis struct {
	Category    string    `json:"category"`
	ContentType string    `json:"content_type"`
	Verdict     string    `json:"verdict"`
	Reason      string    `json:"reason"`
	AnalyzedAt  time.Time `json:"analyzed_at"`
}

type Channel struct {
	ID              string    `json:"id"`
	SubscriptionID  string    `json:"subscription_id"`
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	Thumbnail       string    `json:"thumbnail"`
	SubscribedAt    time.Time `json:"subscribed_at"`
	SubscriberCount int64     `json:"subscriber_count"`
	VideoCount      int64     `json:"video_count"`
	Topics          []string  `json:"topics,omitempty"`
	UploadsPlaylist string    `json:"uploads_playlist"`
	LastUpload      time.Time `json:"last_upload"`
	RecentTitles    []string  `json:"recent_titles,omitempty"`

	// From an optional Google Takeout watch-history import.
	WatchCount  int       `json:"watch_count"`
	LastWatched time.Time `json:"last_watched"`

	Analysis *Analysis `json:"analysis,omitempty"`
}

func (c Channel) URL() string { return "https://www.youtube.com/channel/" + c.ID }

// Inactive reports whether the channel hasn't uploaded in over a year.
func (c Channel) Inactive() bool {
	return !c.LastUpload.IsZero() && time.Since(c.LastUpload) > 365*24*time.Hour
}

type State struct {
	Channels        []Channel `json:"channels"`
	Interests       string    `json:"interests"`
	SyncedAt        time.Time `json:"synced_at"`
	HistoryImported bool      `json:"history_imported"`
	// Watch stats per channel ID from the Takeout import, kept so re-syncs can reapply them.
	Watch map[string]WatchStat `json:"watch,omitempty"`
}

type WatchStat struct {
	Count       int       `json:"count"`
	LastWatched time.Time `json:"last_watched"`
}

// Store is a mutex-guarded JSON file store.
type Store struct {
	mu    sync.RWMutex
	dir   string
	state State
	token *oauth2.Token
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	if err := readJSON(filepath.Join(dir, "state.json"), &s.state); err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := readJSON(filepath.Join(dir, "token.json"), &tok); err != nil {
		return nil, err
	}
	if tok.AccessToken != "" || tok.RefreshToken != "" {
		s.token = &tok
	}
	return s, nil
}

// Snapshot returns a deep-enough copy of the state for read-only use.
func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.state
	st.Channels = append([]Channel(nil), s.state.Channels...)
	return st
}

// Update mutates state under lock and persists it.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
	return writeJSON(filepath.Join(s.dir, "state.json"), s.state)
}

func (s *Store) Token() *oauth2.Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.token
}

func (s *Store) SaveToken(t *oauth2.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = t
	return writeJSON(filepath.Join(s.dir, "token.json"), t)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
