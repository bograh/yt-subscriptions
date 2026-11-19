package views

import (
	"fmt"
	"sort"
	"strings"

	"yt-subscriptions/internal/store"
	"yt-subscriptions/internal/youtube"
)

// PlaylistPage is the Playlists dashboard.
type PlaylistPage struct {
	Status
	State store.PlaylistState
	Stats PlaylistStats
	List  PlaylistList
}

type PlaylistStats struct {
	Total, Keep, Review, Delete, Merge, Unanalyzed, Outdated int
	Unavailable, Duplicates, Empty                           int
	QuotaUsed, QuotaLimit                                    int
}

func ComputePlaylistStats(ps store.PlaylistState, quotaLimit int) PlaylistStats {
	s := PlaylistStats{Total: len(ps.Playlists), QuotaUsed: ps.QuotaUsed, QuotaLimit: quotaLimit}
	for _, p := range ps.Playlists {
		switch p.Verdict() {
		case "":
			s.Unanalyzed++
		case store.PlaylistKeep:
			s.Keep++
		case store.PlaylistDelete:
			s.Delete++
		case store.PlaylistMerge:
			s.Merge++
		default:
			s.Review++
		}
		if p.Decision == "" && p.Analysis != nil && p.Analysis.Outdated {
			s.Outdated++
		}
		s.Unavailable += p.Unavailable
		s.Duplicates += len(p.Duplicates)
		if len(p.Items) == 0 {
			s.Empty++
		}
	}
	return s
}

func (s PlaylistStats) Sentence() []SentencePart {
	return sentence(
		clause(s.Delete, "Delete %d", true),
		clause(s.Merge, "merge %d", false),
		clause(s.Review, "review %d", false),
		clause(s.Keep, "keep %d", false),
	)
}

func (s PlaylistStats) Analyzed() int  { return s.Keep + s.Review + s.Delete + s.Merge }
func (s PlaylistStats) Pending() int   { return s.Unanalyzed + s.Outdated }
func (s PlaylistStats) Cleanable() int { return s.Unavailable + s.Duplicates }
func (s PlaylistStats) QuotaRemaining() int {
	return max(0, s.QuotaLimit-s.QuotaUsed)
}

func (s PlaylistStats) Pct(n int) string {
	if s.Total == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.3f%%", float64(n)*100/float64(s.Total))
}

// CleanAllNote explains what "Clean up all" removes and costs.
func (s PlaylistStats) CleanAllNote() string {
	cost := s.Cleanable() * youtube.CostWrite
	note := fmt.Sprintf("Removes %s and %s across your playlists for about %s quota units",
		plural(s.Unavailable, "unavailable video", "unavailable videos"),
		plural(s.Duplicates, "duplicate", "duplicates"), Thousands(cost))
	if cost > s.QuotaRemaining() {
		return fmt.Sprintf("%s; today's quota covers %d of them.", note, s.QuotaRemaining()/youtube.CostWrite)
	}
	return note + "."
}

type PlaylistGroup struct {
	Name      string
	Playlists []store.Playlist
}

type PlaylistList struct {
	GroupBy string // topic | verdict
	Verdict string // "" = all, or a verdict, "unanalyzed", "problems"
	Query   string
	Groups  []PlaylistGroup
	Shown   int
	ByID    map[string]store.Playlist // every playlist, for merge targets
}

var playlistVerdictOrder = map[string]int{
	store.PlaylistDelete: 0, store.PlaylistMerge: 1, store.PlaylistReview: 2, store.PlaylistKeep: 3, "": 4,
}

func BuildPlaylistList(playlists []store.Playlist, groupBy, verdict, query string) PlaylistList {
	if groupBy != "verdict" {
		groupBy = "topic"
	}
	l := PlaylistList{GroupBy: groupBy, Verdict: verdict, Query: query, ByID: map[string]store.Playlist{}}
	q := strings.ToLower(strings.TrimSpace(query))
	groups := map[string][]store.Playlist{}
	for _, p := range playlists {
		l.ByID[p.ID] = p
		v := p.Verdict()
		switch {
		case verdict == "problems":
			if p.Unavailable == 0 && len(p.Duplicates) == 0 && len(p.Items) > 0 {
				continue
			}
		case verdict == "unanalyzed":
			if v != "" {
				continue
			}
		case verdict != "" && v != verdict:
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(p.Title+" "+p.Description), q) {
			continue
		}
		key := "Not analyzed yet"
		switch {
		case groupBy == "verdict" && v != "":
			key = PlaylistVerdictLabel(v)
		case groupBy == "topic" && p.Analysis != nil:
			key = p.Analysis.Topic
		}
		groups[key] = append(groups[key], p)
		l.Shown++
	}
	for name, ps := range groups {
		sort.Slice(ps, func(i, j int) bool {
			vi, vj := playlistVerdictOrder[ps[i].Verdict()], playlistVerdictOrder[ps[j].Verdict()]
			if vi != vj {
				return vi < vj
			}
			return naturalLess(ps[i].Title, ps[j].Title)
		})
		l.Groups = append(l.Groups, PlaylistGroup{Name: name, Playlists: ps})
	}
	sort.Slice(l.Groups, func(i, j int) bool {
		if groupBy == "verdict" { // action order: Delete, Merge, Review, Keep, Not analyzed
			return playlistVerdictOrder[l.Groups[i].Playlists[0].Verdict()] < playlistVerdictOrder[l.Groups[j].Playlists[0].Verdict()]
		}
		if len(l.Groups[i].Playlists) != len(l.Groups[j].Playlists) {
			return len(l.Groups[i].Playlists) > len(l.Groups[j].Playlists)
		}
		return naturalLess(l.Groups[i].Name, l.Groups[j].Name)
	})
	return l
}

// MergeTarget returns the playlist p's analysis suggests merging into, if it still exists.
func (l PlaylistList) MergeTarget(p store.Playlist) (store.Playlist, bool) {
	if p.Verdict() != store.PlaylistMerge || p.Analysis == nil || p.Analysis.MergeWith == "" {
		return store.Playlist{}, false
	}
	t, ok := l.ByID[p.Analysis.MergeWith]
	return t, ok
}

// MergeMoves counts the videos a merge would copy: available, distinct, not already in target.
func MergeMoves(src, target store.Playlist) int {
	have := map[string]bool{}
	for _, it := range target.Items {
		have[it.VideoID] = true
	}
	n := 0
	for _, it := range src.Items {
		if !it.Unavailable && !have[it.VideoID] {
			have[it.VideoID] = true
			n++
		}
	}
	return n
}

func PlaylistVerdictLabel(v string) string {
	switch v {
	case store.PlaylistKeep:
		return "Keep"
	case store.PlaylistReview:
		return "Review"
	case store.PlaylistDelete:
		return "Delete"
	case store.PlaylistMerge:
		return "Merge"
	}
	return "Not analyzed"
}

func PlaylistVerdictMark(v string) string {
	switch v {
	case store.PlaylistKeep:
		return "bg-keep"
	case store.PlaylistReview:
		return "bg-review"
	case store.PlaylistDelete:
		return "bg-drop"
	case store.PlaylistMerge:
		return "bg-merge"
	}
	return "bg-rule-strong"
}

// PlaylistProblems is the one-line health summary for a row, e.g.
// "4 unavailable · 2 duplicates · shares 12 with Golang".
func PlaylistProblems(p store.Playlist) string {
	var parts []string
	if len(p.Items) == 0 {
		parts = append(parts, "empty")
	}
	if p.Unavailable > 0 {
		parts = append(parts, fmt.Sprintf("%d unavailable", p.Unavailable))
	}
	if n := len(p.Duplicates); n > 0 {
		parts = append(parts, plural(n, "duplicate", "duplicates"))
	}
	return strings.Join(parts, " · ")
}

// PlaylistOverlapNote is context, not a problem: "shares 11 videos with Golang".
func PlaylistOverlapNote(p store.Playlist) string {
	if len(p.Overlaps) == 0 {
		return ""
	}
	o := p.Overlaps[0]
	return fmt.Sprintf("shares %s with %s", plural(o.Shared, "video", "videos"), o.Title)
}

func Privacy(p store.Playlist) string {
	switch p.Privacy {
	case "private", "unlisted":
		return p.Privacy
	}
	return ""
}
