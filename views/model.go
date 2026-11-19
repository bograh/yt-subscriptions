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
	Warning      string
	Flash        string
	Stats        Stats
	List         List
}

type Stats struct {
	Total, Keep, Review, Unsubscribe, Unanalyzed, Inactive, NeverWatched int
	Outdated                                                             int
	QuotaUsed, QuotaLimit                                                int
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

func ComputeStats(st store.State, quotaLimit int) Stats {
	s := Stats{Total: len(st.Channels), HistoryImported: st.HistoryImported, QuotaUsed: st.QuotaUsed, QuotaLimit: quotaLimit}
	for _, c := range st.Channels {
		switch c.Verdict() {
		case "":
			s.Unanalyzed++
		case store.VerdictKeep:
			s.Keep++
		case store.VerdictUnsubscribe:
			s.Unsubscribe++
		default:
			s.Review++
		}
		if c.Decision == "" && c.Analysis != nil && c.Analysis.Outdated {
			s.Outdated++
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
		if verdict != "" && v != verdict && !(verdict == "unanalyzed" && v == "") {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(c.Title+" "+c.Description), q) {
			continue
		}
		key := "Not analyzed yet"
		if groupBy == "verdict" && v != "" {
			key = VerdictLabel(v)
		} else if c.Analysis != nil {
			switch groupBy {
			case "category":
				key = c.Analysis.Category
			case "type":
				key = c.Analysis.ContentType
			case "verdict":
				key = VerdictLabel(v)
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
			return naturalLess(chs[i].Title, chs[j].Title)
		})
		l.Groups = append(l.Groups, Group{Name: name, Channels: chs})
	}
	sort.Slice(l.Groups, func(i, j int) bool {
		if len(l.Groups[i].Channels) != len(l.Groups[j].Channels) {
			return len(l.Groups[i].Channels) > len(l.Groups[j].Channels)
		}
		return naturalLess(l.Groups[i].Name, l.Groups[j].Name)
	})
	return l
}

// naturalLess compares case-insensitively with digit runs as numbers,
// so "Channel 3" sorts before "Channel 15".
func naturalLess(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	for a != "" && b != "" {
		if isDigit(a[0]) && isDigit(b[0]) {
			na, ra := digitRun(a)
			nb, rb := digitRun(b)
			ta, tb := strings.TrimLeft(na, "0"), strings.TrimLeft(nb, "0")
			if len(ta) != len(tb) {
				return len(ta) < len(tb)
			}
			if ta != tb {
				return ta < tb
			}
			a, b = ra, rb
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func digitRun(s string) (run, rest string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return s[:i], s[i:]
}

func verdictOf(c store.Channel) string { return c.Verdict() }

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

func (s Stats) QuotaRemaining() int { return max(0, s.QuotaLimit-s.QuotaUsed) }

// BulkCost is the quota a bulk unsubscribe of every suggested channel would use.
func (s Stats) BulkCost() int { return s.Unsubscribe * 50 }

// BulkNote explains the cost of "Unsubscribe from all".
func (s Stats) BulkNote() string {
	note := "Review the list first. Each unsubscribe costs 50 quota units"
	if s.BulkCost() > s.QuotaRemaining() {
		return fmt.Sprintf("%s, so today's quota covers %d of them.", note, s.QuotaRemaining()/50)
	}
	return note + "."
}

// Pending is how many channels "Analyze new" will send: never analyzed or outdated.
func (s Stats) Pending() int { return s.Unanalyzed + s.Outdated }

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

// Thousands formats n with comma separators: 10000 -> "10,000".
func Thousands(n int) string {
	str := fmt.Sprint(n)
	if n < 0 {
		return "-" + Thousands(-n)
	}
	for i := len(str) - 3; i > 0; i -= 3 {
		str = str[:i] + "," + str[i:]
	}
	return str
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
