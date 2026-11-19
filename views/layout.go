// Package views renders the dashboard: templ components (*.templ) and the
// view models and helpers they use (*.go). Files are split like
// internal/server: layout.* is shared, subscriptions.* and playlists.* are
// the two pages. Run `make generate` after editing a .templ file.
package views

import (
	"fmt"
	"strings"
	"time"
)

// Job is the background job shown in the progress banner.
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

// HeaderData drives the shared page header.
type HeaderData struct {
	Active     string // "subscriptions" | "playlists"
	SyncedAt   time.Time
	Connected  bool
	OAuthReady bool
	Job        Job
}

// Status is what every page shows around its content: connection state, the
// running job and one-time messages. Page and PlaylistPage embed it.
type Status struct {
	Connected    bool   // a YouTube token is stored
	OAuthReady   bool   // GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET are set
	AIConfigured bool   // AI_MODEL is set
	Job          Job    // the background job, if one is running
	Error        string // why the last job failed
	Warning      string // a non-fatal problem from the last job
	Flash        string // one-time notice
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

// SentencePart is one clause of the summary sentence; Accent marks the
// destructive suggestion, shown in the accent colour.
type SentencePart struct {
	Text   string
	Accent bool
}

// sentence builds "Unsubscribe from 3, review 2, keep 5." leaving out zero counts.
func sentence(parts ...SentencePart) []SentencePart {
	var out []SentencePart
	for _, p := range parts {
		if p.Text != "" {
			out = append(out, p)
		}
	}
	return out
}

func punct(i, n int) string {
	if i == n-1 {
		return "."
	}
	return ","
}

func clause(n int, format string, accent bool) SentencePart {
	if n == 0 {
		return SentencePart{}
	}
	return SentencePart{Text: fmt.Sprintf(format, n), Accent: accent}
}

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

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
