package store

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"time"
)

// Playlist verdicts. Merge means "move this playlist's videos into MergeWith".
const (
	PlaylistKeep   = "keep"
	PlaylistReview = "review"
	PlaylistDelete = "delete"
	PlaylistMerge  = "merge"
)

// Playlist is one of the user's own playlists with all of its items.
// Fields below the blank line are computed when reading from the store.
type Playlist struct {
	ID          string
	Title       string
	Description string
	Thumbnail   string
	Privacy     string // public | unlisted | private
	PublishedAt time.Time
	Items       []PlaylistItem // in playlist order

	LastAdded   time.Time         // newest item's AddedAt
	Unavailable int               // items whose video was deleted or made private
	Duplicates  []PlaylistItem    // later copies of a video already earlier in this playlist
	Overlaps    []PlaylistOverlap // other playlists sharing videos with this one, most shared first
	Analysis    *PlaylistAnalysis
	Decision    string // the user's own verdict ("" if none); wins over the AI's
}

// PlaylistItem is one entry in a playlist. ID is the playlistItem ID, which is
// what YouTube needs to remove the entry (not the video ID).
type PlaylistItem struct {
	ID           string
	VideoID      string
	Title        string
	ChannelID    string // the video owner's channel; empty when Unavailable
	ChannelTitle string
	Position     int
	AddedAt      time.Time
	Unavailable  bool // "Deleted video" / "Private video" entries
}

type PlaylistOverlap struct {
	PlaylistID string
	Title      string
	Shared     int // distinct videos in both playlists
}

type PlaylistAnalysis struct {
	Topic      string // one of ai.Categories
	Verdict    string // PlaylistKeep | PlaylistReview | PlaylistDelete | PlaylistMerge
	MergeWith  string // target playlist ID when Verdict is PlaylistMerge
	Reason     string
	AnalyzedAt time.Time
	Basis      string // fingerprint of the interests the AI saw
	Outdated   bool   // computed on read
}

// Verdict is the effective verdict: the user's decision, else the AI's, else "".
func (p Playlist) Verdict() string {
	if p.Decision != "" {
		return p.Decision
	}
	if p.Analysis != nil {
		return p.Analysis.Verdict
	}
	return ""
}

func (p Playlist) URL() string { return "https://www.youtube.com/playlist?list=" + p.ID }

// PlaylistState is everything the playlists page needs.
type PlaylistState struct {
	Playlists []Playlist
	SyncedAt  time.Time
	Interests string
	Basis     string // fingerprint of what personalises playlist suggestions
	QuotaUsed int
}

const keyPlaylistsSyncedAt = "playlists_synced_at"

// playlistBasis is independent of watch history, which doesn't feed playlist review.
func playlistBasis(interests string) string { return analysisBasis(interests, "playlists") }

// ReplacePlaylists swaps in a freshly synced set of playlists and their items.
// Analyses and decisions are kept by playlist ID.
func (s *Store) ReplacePlaylists(ctx context.Context, playlists []Playlist) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM playlists; DELETE FROM playlist_items;`); err != nil {
			return err
		}
		pStmt, err := tx.Prepare(`INSERT INTO playlists (id, title, description, thumbnail, privacy, published_at, sort)
			VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer pStmt.Close()
		iStmt, err := tx.Prepare(`INSERT OR REPLACE INTO playlist_items
			(id, playlist_id, video_id, title, channel_id, channel_title, position, added_at, unavailable)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer iStmt.Close()
		for i, p := range playlists {
			if _, err := pStmt.Exec(p.ID, p.Title, p.Description, p.Thumbnail, p.Privacy, toUnix(p.PublishedAt), i); err != nil {
				return err
			}
			for _, it := range p.Items {
				if _, err := iStmt.Exec(it.ID, p.ID, it.VideoID, it.Title, it.ChannelID, it.ChannelTitle,
					it.Position, toUnix(it.AddedAt), it.Unavailable); err != nil {
					return err
				}
			}
		}
		return setSetting(tx, keyPlaylistsSyncedAt, strconv.FormatInt(time.Now().Unix(), 10))
	})
}

// Playlists loads every playlist with its items, analysis and decision, and
// computes unavailable counts, duplicates and overlaps between playlists.
func (s *Store) Playlists(ctx context.Context) (PlaylistState, error) {
	var ps PlaylistState
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.title, p.description, p.thumbnail, p.privacy, p.published_at,
		       a.topic, a.verdict, a.merge_with, a.reason, a.analyzed_at, a.basis,
		       COALESCE(d.verdict, '')
		FROM playlists p
		LEFT JOIN playlist_analyses a ON a.playlist_id = p.id
		LEFT JOIN playlist_decisions d ON d.playlist_id = p.id
		ORDER BY p.sort`)
	if err != nil {
		return ps, err
	}
	idx := map[string]int{}
	for rows.Next() {
		var (
			p                                        Playlist
			published, analyzedAt                    sql.NullInt64
			topic, verdict, mergeWith, reason, basis sql.NullString
		)
		if err := rows.Scan(&p.ID, &p.Title, &p.Description, &p.Thumbnail, &p.Privacy, &published,
			&topic, &verdict, &mergeWith, &reason, &analyzedAt, &basis, &p.Decision); err != nil {
			rows.Close()
			return ps, err
		}
		p.PublishedAt = fromUnix(published)
		if verdict.Valid {
			p.Analysis = &PlaylistAnalysis{Topic: topic.String, Verdict: verdict.String, MergeWith: mergeWith.String,
				Reason: reason.String, AnalyzedAt: fromUnix(analyzedAt), Basis: basis.String}
		}
		idx[p.ID] = len(ps.Playlists)
		ps.Playlists = append(ps.Playlists, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ps, err
	}

	items, err := s.db.QueryContext(ctx, `
		SELECT id, playlist_id, video_id, title, channel_id, channel_title, position, added_at, unavailable
		FROM playlist_items ORDER BY playlist_id, position`)
	if err != nil {
		return ps, err
	}
	defer items.Close()
	for items.Next() {
		var (
			it      PlaylistItem
			pid     string
			addedAt sql.NullInt64
		)
		if err := items.Scan(&it.ID, &pid, &it.VideoID, &it.Title, &it.ChannelID, &it.ChannelTitle,
			&it.Position, &addedAt, &it.Unavailable); err != nil {
			return ps, err
		}
		it.AddedAt = fromUnix(addedAt)
		if i, ok := idx[pid]; ok {
			ps.Playlists[i].Items = append(ps.Playlists[i].Items, it)
		}
	}
	if err := items.Err(); err != nil {
		return ps, err
	}

	settings, err := s.settings(ctx)
	if err != nil {
		return ps, err
	}
	ps.Interests = settings[keyInterests]
	ps.Basis = playlistBasis(ps.Interests)
	if v, err := strconv.ParseInt(settings[keyPlaylistsSyncedAt], 10, 64); err == nil {
		ps.SyncedAt = time.Unix(v, 0)
	}
	if settings[keyQuotaDay] == QuotaDay(time.Now()) {
		ps.QuotaUsed, _ = strconv.Atoi(settings[keyQuotaUsed])
	}
	computePlaylistFacts(ps.Playlists, ps.Basis)
	return ps, nil
}

// computePlaylistFacts fills the derived fields of each playlist.
func computePlaylistFacts(playlists []Playlist, basis string) {
	inPlaylists := map[string][]int{} // video ID -> playlists containing it (each once)
	for i := range playlists {
		p := &playlists[i]
		seen := map[string]bool{}
		for _, it := range p.Items {
			if it.AddedAt.After(p.LastAdded) {
				p.LastAdded = it.AddedAt
			}
			if it.Unavailable {
				p.Unavailable++
				continue
			}
			if seen[it.VideoID] {
				p.Duplicates = append(p.Duplicates, it)
				continue
			}
			seen[it.VideoID] = true
			inPlaylists[it.VideoID] = append(inPlaylists[it.VideoID], i)
		}
		if a := p.Analysis; a != nil && a.Basis != "" && a.Basis != basis {
			a.Outdated = true
		}
	}
	shared := map[[2]int]int{}
	for _, ps := range inPlaylists {
		for a := 0; a < len(ps); a++ {
			for b := a + 1; b < len(ps); b++ {
				shared[[2]int{ps[a], ps[b]}]++
			}
		}
	}
	for pair, n := range shared {
		a, b := pair[0], pair[1]
		playlists[a].Overlaps = append(playlists[a].Overlaps, PlaylistOverlap{PlaylistID: playlists[b].ID, Title: playlists[b].Title, Shared: n})
		playlists[b].Overlaps = append(playlists[b].Overlaps, PlaylistOverlap{PlaylistID: playlists[a].ID, Title: playlists[a].Title, Shared: n})
	}
	for i := range playlists {
		sort.Slice(playlists[i].Overlaps, func(x, y int) bool {
			ox, oy := playlists[i].Overlaps[x], playlists[i].Overlaps[y]
			if ox.Shared != oy.Shared {
				return ox.Shared > oy.Shared
			}
			return ox.Title < oy.Title
		})
	}
}

func (s *Store) SavePlaylistAnalyses(ctx context.Context, analyses map[string]PlaylistAnalysis) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT OR REPLACE INTO playlist_analyses
			(playlist_id, topic, verdict, merge_with, reason, analyzed_at, basis) VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for id, a := range analyses {
			if _, err := stmt.Exec(id, a.Topic, a.Verdict, a.MergeWith, a.Reason, a.AnalyzedAt.Unix(), a.Basis); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetPlaylistDecision records the user's own verdict; "" clears it.
func (s *Store) SetPlaylistDecision(ctx context.Context, playlistID, verdict string) error {
	if verdict == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM playlist_decisions WHERE playlist_id = ?`, playlistID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO playlist_decisions (playlist_id, verdict, decided_at) VALUES (?, ?, ?)
		ON CONFLICT(playlist_id) DO UPDATE SET verdict = excluded.verdict, decided_at = excluded.decided_at`,
		playlistID, verdict, time.Now().Unix())
	return err
}

// DeletePlaylist forgets a playlist deleted on YouTube, with its items, analysis and decision.
func (s *Store) DeletePlaylist(ctx context.Context, playlistID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM playlists WHERE id = ?`,
			`DELETE FROM playlist_items WHERE playlist_id = ?`,
			`DELETE FROM playlist_analyses WHERE playlist_id = ?`,
			`DELETE FROM playlist_decisions WHERE playlist_id = ?`,
		} {
			if _, err := tx.Exec(q, playlistID); err != nil {
				return err
			}
		}
		return nil
	})
}

// RemovePlaylistItems forgets items removed on YouTube.
func (s *Store) RemovePlaylistItems(ctx context.Context, itemIDs []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`DELETE FROM playlist_items WHERE id = ?`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, id := range itemIDs {
			if _, err := stmt.Exec(id); err != nil {
				return err
			}
		}
		return nil
	})
}
