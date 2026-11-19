package views

import (
	"fmt"
	"sort"
	"strings"

	"yt-subscriptions/internal/store"
	"yt-subscriptions/internal/youtube"
)

// Page is the Subscriptions dashboard.
type Page struct {
	Status
	State store.State
	Stats Stats
	List  List
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
		if groupBy == "verdict" { // action order: Unsubscribe, Review, Keep, Not analyzed
			return verdictOrder[l.Groups[i].Channels[0].Verdict()] < verdictOrder[l.Groups[j].Channels[0].Verdict()]
		}
		if len(l.Groups[i].Channels) != len(l.Groups[j].Channels) {
			return len(l.Groups[i].Channels) > len(l.Groups[j].Channels)
		}
		return naturalLess(l.Groups[i].Name, l.Groups[j].Name)
	})
	return l
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
func (s Stats) BulkCost() int { return s.Unsubscribe * youtube.CostUnsubscribe }

// BulkNote explains the cost of "Unsubscribe from all".
func (s Stats) BulkNote() string {
	note := "Review the list first. Each unsubscribe costs " + fmt.Sprint(youtube.CostUnsubscribe) + " quota units"
	if s.BulkCost() > s.QuotaRemaining() {
		return fmt.Sprintf("%s, so today's quota covers %d of them.", note, s.QuotaRemaining()/youtube.CostUnsubscribe)
	}
	return note + "."
}

func (s Stats) Sentence() []SentencePart {
	return sentence(
		clause(s.Unsubscribe, "Unsubscribe from %d", true),
		clause(s.Review, "review %d", false),
		clause(s.Keep, "keep %d", false),
	)
}

// Pending is how many channels "Analyze new" will send: never analyzed or outdated.
func (s Stats) Pending() int { return s.Unanalyzed + s.Outdated }

func (s Stats) Analyzed() int { return s.Keep + s.Review + s.Unsubscribe }
