package store

import "time"

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
