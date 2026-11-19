package store

import (
	"database/sql"
	"fmt"
)

// migrations are applied in order; the database's PRAGMA user_version records how
// many have run. Never edit a released migration: append a new one instead.
var migrations = []string{
	// 1: initial schema. IF NOT EXISTS keeps it safe for databases created before
	// migrations were versioned.
	`CREATE TABLE IF NOT EXISTS channels (
		id               TEXT PRIMARY KEY,
		subscription_id  TEXT NOT NULL,
		title            TEXT NOT NULL,
		description      TEXT NOT NULL DEFAULT '',
		thumbnail        TEXT NOT NULL DEFAULT '',
		subscribed_at    INTEGER,
		subscriber_count INTEGER NOT NULL DEFAULT 0,
		video_count      INTEGER NOT NULL DEFAULT 0,
		topics           TEXT NOT NULL DEFAULT '[]',
		uploads_playlist TEXT NOT NULL DEFAULT '',
		last_upload      INTEGER,
		recent_titles    TEXT NOT NULL DEFAULT '[]'
	);
	CREATE TABLE IF NOT EXISTS analyses (
		channel_id   TEXT PRIMARY KEY,
		category     TEXT NOT NULL,
		content_type TEXT NOT NULL,
		verdict      TEXT NOT NULL,
		reason       TEXT NOT NULL,
		analyzed_at  INTEGER NOT NULL
	);
	CREATE TABLE IF NOT EXISTS watch_stats (
		channel_id   TEXT PRIMARY KEY,
		count        INTEGER NOT NULL,
		last_watched INTEGER
	);
	CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`,

	// 2: channels that hide their subscriber count.
	`ALTER TABLE channels ADD COLUMN subscribers_hidden INTEGER NOT NULL DEFAULT 0;`,

	// 3: fingerprint of the interests + watch history each analysis was based on.
	`ALTER TABLE analyses ADD COLUMN basis TEXT NOT NULL DEFAULT '';`,

	// 4: the user's own decisions, which override the AI's verdict.
	`CREATE TABLE decisions (
		channel_id TEXT PRIMARY KEY,
		verdict    TEXT NOT NULL,
		decided_at INTEGER NOT NULL
	);`,

	// 5: the user's own playlists, their items, and AI analyses / user decisions for them.
	`CREATE TABLE playlists (
		id           TEXT PRIMARY KEY,
		title        TEXT NOT NULL,
		description  TEXT NOT NULL DEFAULT '',
		thumbnail    TEXT NOT NULL DEFAULT '',
		privacy      TEXT NOT NULL DEFAULT '',
		published_at INTEGER,
		sort         INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE playlist_items (
		id            TEXT PRIMARY KEY,
		playlist_id   TEXT NOT NULL,
		video_id      TEXT NOT NULL,
		title         TEXT NOT NULL DEFAULT '',
		channel_id    TEXT NOT NULL DEFAULT '',
		channel_title TEXT NOT NULL DEFAULT '',
		position      INTEGER NOT NULL DEFAULT 0,
		added_at      INTEGER,
		unavailable   INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX playlist_items_by_playlist ON playlist_items (playlist_id, position);
	CREATE TABLE playlist_analyses (
		playlist_id TEXT PRIMARY KEY,
		topic       TEXT NOT NULL,
		verdict     TEXT NOT NULL,
		merge_with  TEXT NOT NULL DEFAULT '',
		reason      TEXT NOT NULL,
		analyzed_at INTEGER NOT NULL,
		basis       TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE playlist_decisions (
		playlist_id TEXT PRIMARY KEY,
		verdict     TEXT NOT NULL,
		decided_at  INTEGER NOT NULL
	);`,
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		// PRAGMA can't take bound parameters; i is an int we control.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
