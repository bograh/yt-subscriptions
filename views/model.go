package views

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"yt-subscriptions/internal/store"
)

type Job struct {
	Running bool
	Label   string
	Done    int
	Total   int
}

func (j Job) Percent() int {
	if j.Total == 0 {
		return 0
	}
	return j.Done * 100 / j.Total
}

type Page struct {
	State        store.State
	Connected    bool
	OAuthReady   bool
	AIConfigured bool
	Job          Job
	Error        string
	Stats        Stats
	List         List
}

type Stats struct {
	Total, Keep, Review, Unsubscribe, Unanalyzed, Inactive, NeverWatched int
	HistoryImported                                                      bool
}

type Group struct {
	Name     string
	Channels []store.Channel
}

type List struct {
	GroupBy string // category | type | verdict
	Verdict string // "" = all
	Query   string
	Groups  []Group
	Shown   int
}

func ComputeStats(st store.State) Stats {
	s := Stats{Total: len(st.Channels), HistoryImported: st.HistoryImported}
	for _, c := range st.Channels {
		switch {
		case c.Analysis == nil:
			s.Unanalyzed++
		case c.Analysis.Verdict == store.VerdictKeep:
			s.Keep++
		case c.Analysis.Verdict == store.VerdictUnsubscribe:
			s.Unsubscribe++
		default:
			s.Review++
		}
		if c.Inactive() {
			s.Inactive++
		}
		if st.HistoryImported && c.WatchCount == 0 {
			s.NeverWatched++
		}
	}
	return s
}

var verdictOrder = map[string]int{store.VerdictUnsubscribe: 0, store.VerdictReview: 1, store.VerdictKeep: 2, "": 3}

func BuildList(channels []store.Channel, groupBy, verdict, query string) List {
	if groupBy != "type" && groupBy != "verdict" {
		groupBy = "category"
	}
	l := List{GroupBy: groupBy, Verdict: verdict, Query: query}
	q := strings.ToLower(strings.TrimSpace(query))
	groups := map[string][]store.Channel{}
	for _, c := range channels {
		v := verdictOf(c)
		if verdict != "" && v != verdict && !(verdict == "unanalyzed" && c.Analysis == nil) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(c.Title+" "+c.Description), q) {
			continue
		}
		key := "Not analyzed yet"
		if c.Analysis != nil {
			switch groupBy {
			case "category":
				key = c.Analysis.Category
			case "type":
				key = c.Analysis.ContentType
			case "verdict":
				key = VerdictLabel(c.Analysis.Verdict)
			}
		}
		groups[key] = append(groups[key], c)
		l.Shown++
	}
	for name, chs := range groups {
		sort.Slice(chs, func(i, j int) bool {
			vi, vj := verdictOrder[verdictOf(chs[i])], verdictOrder[verdictOf(chs[j])]
			if vi != vj {
				return vi < vj
			}
			return strings.ToLower(chs[i].Title) < strings.ToLower(chs[j].Title)
		})
		l.Groups = append(l.Groups, Group{Name: name, Channels: chs})
	}
	sort.Slice(l.Groups, func(i, j int) bool {
		if len(l.Groups[i].Channels) != len(l.Groups[j].Channels) {
			return len(l.Groups[i].Channels) > len(l.Groups[j].Channels)
		}
		return l.Groups[i].Name < l.Groups[j].Name
	})
	return l
}

func verdictOf(c store.Channel) string {
	if c.Analysis == nil {
		return ""
	}
	return c.Analysis.Verdict
}

func VerdictLabel(v string) string {
	switch v {
	case store.VerdictKeep:
		return "Keep"
	case store.VerdictUnsubscribe:
		return "Unsubscribe"
	case store.VerdictReview:
		return "Review"
	}
	return "Not analyzed"
}

// VerdictMark is the Tailwind background class for a verdict's square marker.
func VerdictMark(v string) string {
	switch v {
	case store.VerdictKeep:
		return "bg-keep"
	case store.VerdictUnsubscribe:
		return "bg-drop"
	case store.VerdictReview:
		return "bg-review"
	}
	return "bg-rule-strong"
}

// Pct is n as a percentage of all subscriptions, for the summary bar widths.
func (s Stats) Pct(n int) string {
	if s.Total == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.3f%%", float64(n)*100/float64(s.Total))
}

func (s Stats) Analyzed() int { return s.Keep + s.Review + s.Unsubscribe }

func Compact(n int64) string {
	switch {
	case n >= 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000_000)) + "M"
	case n >= 1_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000)) + "K"
	}
	return fmt.Sprint(n)
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

func Ago(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < 48*time.Hour:
		return "today"
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%.1fy ago", d.Hours()/24/365)
}
