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
