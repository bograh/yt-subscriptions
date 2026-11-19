// Package store holds the app's data model and persists it in SQLite.
//
// AI analyses live in their own table keyed by channel ID, independent of the
// current subscription list, so they survive re-syncs, unsubscribes and re-subscribes.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	_ "modernc.org/sqlite"
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
	// Basis fingerprints the interests + watch history the AI saw (see State.Basis).
	Basis string `json:"basis,omitempty"`
	// Outdated is computed on read: the basis no longer matches the current one.
	Outdated bool `json:"-"`
}

type Channel struct {
	ID              string    `json:"id"`
	SubscriptionID  string    `json:"subscription_id"`
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	Thumbnail       string    `json:"thumbnail"`
	SubscribedAt    time.Time `json:"subscribed_at"`
	SubscriberCount int64     `json:"subscriber_count"`
	// SubscribersHidden is set when the channel hides its count (SubscriberCount is then 0).
	SubscribersHidden bool      `json:"subscribers_hidden"`
	VideoCount        int64     `json:"video_count"`
	Topics            []string  `json:"topics,omitempty"`
	UploadsPlaylist   string    `json:"uploads_playlist"`
	LastUpload        time.Time `json:"last_upload"`
	RecentTitles      []string  `json:"recent_titles,omitempty"`

	// From an optional Google Takeout watch-history import.
	WatchCount  int       `json:"watch_count"`
	LastWatched time.Time `json:"last_watched"`

	Analysis *Analysis `json:"analysis,omitempty"`
	// Decision is the user's own verdict ("" if none). It wins over the AI's.
	Decision string `json:"decision,omitempty"`
}

// Verdict is the effective verdict: the user's decision if any, else the AI's, else "".
func (c Channel) Verdict() string {
	if c.Decision != "" {
		return c.Decision
	}
	if c.Analysis != nil {
		return c.Analysis.Verdict
	}
	return ""
}

func (c Channel) URL() string { return "https://www.youtube.com/channel/" + c.ID }

// Inactive reports whether the channel hasn't uploaded in over a year.
func (c Channel) Inactive() bool {
	return !c.LastUpload.IsZero() && time.Since(c.LastUpload) > 365*24*time.Hour
}

type State struct {
	Channels        []Channel
	Interests       string
	SyncedAt        time.Time
	HistoryImported bool
	// Basis fingerprints what personalises suggestions (interests + watch-history
	// import). Analyses made under a different basis are marked Outdated.
	Basis string
}

func analysisBasis(interests, historyImportedAt string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(interests) + "\x00" + historyImportedAt))
	return hex.EncodeToString(sum[:6])
}

type WatchStat struct {
	Count       int       `json:"count"`
	LastWatched time.Time `json:"last_watched"`
}

const (
	keyInterests         = "interests"
	keySyncedAt          = "synced_at"
	keyHistoryImported   = "history_imported"
	keyHistoryImportedAt = "history_imported_at"
	keyOAuthToken        = "oauth_token"
)

type Store struct {
	db *sql.DB
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// The database holds the OAuth token: keep the folder and files private to this user.
	// SQLite creates the -wal/-shm files with the main file's permissions.
	dbPath := filepath.Join(dir, "app.db")
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dbPath, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	dsn := "file:" + dbPath +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has a single writer; this avoids SQLITE_BUSY entirely.
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrateJSON(dir); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate json data: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Snapshot loads the current subscriptions with their analyses and watch stats.
func (s *Store) Snapshot(ctx context.Context) (State, error) {
	var st State
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.subscription_id, c.title, c.description, c.thumbnail, c.subscribed_at,
		       c.subscriber_count, c.subscribers_hidden, c.video_count, c.topics, c.uploads_playlist, c.last_upload, c.recent_titles,
		       COALESCE(w.count, 0), w.last_watched,
		       a.category, a.content_type, a.verdict, a.reason, a.analyzed_at, a.basis,
		       COALESCE(d.verdict, '')
		FROM channels c
		LEFT JOIN watch_stats w ON w.channel_id = c.id
		LEFT JOIN analyses a    ON a.channel_id = c.id
		LEFT JOIN decisions d   ON d.channel_id = c.id
		ORDER BY c.title COLLATE NOCASE`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			c                                 Channel
			subscribedAt, lastUpload, watched sql.NullInt64
			topics, titles                    string
			category, ctype, verdict, reason  sql.NullString
			basis                             sql.NullString
			analyzedAt                        sql.NullInt64
		)
		if err := rows.Scan(&c.ID, &c.SubscriptionID, &c.Title, &c.Description, &c.Thumbnail, &subscribedAt,
			&c.SubscriberCount, &c.SubscribersHidden, &c.VideoCount, &topics, &c.UploadsPlaylist, &lastUpload, &titles,
			&c.WatchCount, &watched,
			&category, &ctype, &verdict, &reason, &analyzedAt, &basis,
			&c.Decision); err != nil {
			return st, err
		}
		c.SubscribedAt, c.LastUpload, c.LastWatched = fromUnix(subscribedAt), fromUnix(lastUpload), fromUnix(watched)
		_ = json.Unmarshal([]byte(topics), &c.Topics)
		_ = json.Unmarshal([]byte(titles), &c.RecentTitles)
		if verdict.Valid {
			c.Analysis = &Analysis{
				Category: category.String, ContentType: ctype.String, Verdict: verdict.String,
				Reason: reason.String, AnalyzedAt: fromUnix(analyzedAt), Basis: basis.String,
			}
		}
		st.Channels = append(st.Channels, c)
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	settings, err := s.settings(ctx)
	if err != nil {
		return st, err
	}
	st.Interests = settings[keyInterests]
	st.HistoryImported = settings[keyHistoryImported] == "1"
	st.Basis = analysisBasis(st.Interests, settings[keyHistoryImportedAt])
	for i := range st.Channels {
		// An empty basis predates tracking; don't flag those.
		if a := st.Channels[i].Analysis; a != nil && a.Basis != "" && a.Basis != st.Basis {
			a.Outdated = true
		}
	}
	if v, err := strconv.ParseInt(settings[keySyncedAt], 10, 64); err == nil {
		st.SyncedAt = time.Unix(v, 0)
	}
	return st, nil
}

// SubscriptionID returns the YouTube subscription ID for a channel, or "" if not subscribed.
func (s *Store) SubscriptionID(ctx context.Context, channelID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT subscription_id FROM channels WHERE id = ?`, channelID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// ReplaceChannels swaps in a freshly synced subscription list. Analyses are untouched.
func (s *Store) ReplaceChannels(ctx context.Context, channels []Channel) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM channels`); err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT OR REPLACE INTO channels
			(id, subscription_id, title, description, thumbnail, subscribed_at, subscriber_count,
			 subscribers_hidden, video_count, topics, uploads_playlist, last_upload, recent_titles)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, c := range channels {
			if _, err := stmt.Exec(c.ID, c.SubscriptionID, c.Title, c.Description, c.Thumbnail, toUnix(c.SubscribedAt),
				c.SubscriberCount, c.SubscribersHidden, c.VideoCount, jsonList(c.Topics), c.UploadsPlaylist, toUnix(c.LastUpload),
				jsonList(c.RecentTitles)); err != nil {
				return err
			}
		}
		return setSetting(tx, keySyncedAt, strconv.FormatInt(time.Now().Unix(), 10))
	})
}

func (s *Store) SaveAnalyses(ctx context.Context, analyses map[string]Analysis) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT OR REPLACE INTO analyses
			(channel_id, category, content_type, verdict, reason, analyzed_at, basis) VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for id, a := range analyses {
			if _, err := stmt.Exec(id, a.Category, a.ContentType, a.Verdict, a.Reason, a.AnalyzedAt.Unix(), a.Basis); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetDecision records the user's own verdict for a channel; "" clears it.
// Decisions are kept by channel ID, so they survive re-syncs and re-subscribing.
func (s *Store) SetDecision(ctx context.Context, channelID, verdict string) error {
	if verdict == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM decisions WHERE channel_id = ?`, channelID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO decisions (channel_id, verdict, decided_at) VALUES (?, ?, ?)
		ON CONFLICT(channel_id) DO UPDATE SET verdict = excluded.verdict, decided_at = excluded.decided_at`,
		channelID, verdict, time.Now().Unix())
	return err
}

// DeleteChannel removes a channel from the subscription list after unsubscribing.
// Its analysis is kept in case you subscribe again.
func (s *Store) DeleteChannel(ctx context.Context, channelID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, channelID)
	return err
}

func (s *Store) SetInterests(ctx context.Context, interests string) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return setSetting(tx, keyInterests, interests) })
}

// ImportWatchStats replaces all watch stats and returns how many current subscriptions were watched.
func (s *Store) ImportWatchStats(ctx context.Context, stats map[string]WatchStat) (matched int, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM watch_stats`); err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT INTO watch_stats (channel_id, count, last_watched) VALUES (?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for id, w := range stats {
			if _, err := stmt.Exec(id, w.Count, toUnix(w.LastWatched)); err != nil {
				return err
			}
		}
		if err := setSetting(tx, keyHistoryImported, "1"); err != nil {
			return err
		}
		if err := setSetting(tx, keyHistoryImportedAt, strconv.FormatInt(time.Now().UnixNano(), 10)); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT COUNT(*) FROM channels c JOIN watch_stats w ON w.channel_id = c.id`).Scan(&matched)
	})
	return matched, err
}

func (s *Store) Token() *oauth2.Token {
	var raw string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, keyOAuthToken).Scan(&raw); err != nil {
		return nil
	}
	var t oauth2.Token
	if json.Unmarshal([]byte(raw), &t) != nil {
		return nil
	}
	return &t
}

func (s *Store) SaveToken(t *oauth2.Token) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return s.tx(context.Background(), func(tx *sql.Tx) error { return setSetting(tx, keyOAuthToken, string(b)) })
}

// DeleteToken forgets the OAuth token (disconnect, or Google revoked it).
func (s *Store) DeleteToken() error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, keyOAuthToken)
	return err
}

// ---- helpers ----

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

func setSetting(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func toUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func fromUnix(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0)
}

func jsonList(xs []string) string {
	if xs == nil {
		return "[]"
	}
	b, _ := json.Marshal(xs)
	return string(b)
}

// migrateJSON imports data from the earlier JSON-file storage (state.json / token.json)
// once, then renames those files to *.migrated.
func (s *Store) migrateJSON(dir string) error {
	statePath, tokenPath := filepath.Join(dir, "state.json"), filepath.Join(dir, "token.json")
	ctx := context.Background()

	if b, err := os.ReadFile(statePath); err == nil {
		var old struct {
			Channels        []Channel            `json:"channels"`
			Interests       string               `json:"interests"`
			SyncedAt        time.Time            `json:"synced_at"`
			HistoryImported bool                 `json:"history_imported"`
			Watch           map[string]WatchStat `json:"watch"`
		}
		if err := json.Unmarshal(b, &old); err != nil {
			return err
		}
		if err := s.ReplaceChannels(ctx, old.Channels); err != nil {
			return err
		}
		analyses := map[string]Analysis{}
		for _, c := range old.Channels {
			if c.Analysis != nil {
				analyses[c.ID] = *c.Analysis
			}
		}
		if err := s.SaveAnalyses(ctx, analyses); err != nil {
			return err
		}
		if old.HistoryImported {
			if _, err := s.ImportWatchStats(ctx, old.Watch); err != nil {
				return err
			}
		}
		if err := s.tx(ctx, func(tx *sql.Tx) error {
			if err := setSetting(tx, keyInterests, old.Interests); err != nil {
				return err
			}
			if old.SyncedAt.IsZero() {
				_, err := tx.Exec(`DELETE FROM settings WHERE key = ?`, keySyncedAt)
				return err
			}
			return setSetting(tx, keySyncedAt, strconv.FormatInt(old.SyncedAt.Unix(), 10))
		}); err != nil {
			return err
		}
		if err := os.Rename(statePath, statePath+".migrated"); err != nil {
			return err
		}
	}

	if b, err := os.ReadFile(tokenPath); err == nil {
		var t oauth2.Token
		if err := json.Unmarshal(b, &t); err != nil {
			return err
		}
		if err := s.SaveToken(&t); err != nil {
			return err
		}
		if err := os.Rename(tokenPath, tokenPath+".migrated"); err != nil {
			return err
		}
	}
	return nil
}
