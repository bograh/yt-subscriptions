package youtube

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"

	"yt-subscriptions/internal/store"
)

// ParseWatchHistory reads a Google Takeout YouTube watch history, either the
// JSON export (watch-history.json) or the HTML export (watch-history.html),
// and aggregates watches by channel ID. Both formats are processed as a
// stream, so large exports are never held in memory in full.
func ParseWatchHistory(r io.Reader) (map[string]store.WatchStat, error) {
	br := bufio.NewReader(r)
	first, err := sniff(br)
	if err != nil {
		return nil, err
	}
	stats := make(map[string]store.WatchStat)
	switch first {
	case '[':
		err = parseJSONHistory(br, stats)
	case '<':
		err = parseHTMLHistory(br, stats)
	default:
		return nil, fmt.Errorf("unrecognized watch history format: expected a Takeout JSON or HTML file, got %q", first)
	}
	if err != nil {
		return nil, err
	}
	return stats, nil
}

// sniff skips a UTF-8 BOM and leading whitespace and returns the first
// significant rune without consuming it.
func sniff(br *bufio.Reader) (rune, error) {
	for {
		c, _, err := br.ReadRune()
		if err == io.EOF {
			return 0, errors.New("watch history file is empty")
		}
		if err != nil {
			return 0, err
		}
		if c == '\uFEFF' || unicode.IsSpace(c) {
			continue
		}
		if err := br.UnreadRune(); err != nil {
			return 0, err
		}
		return c, nil
	}
}

// record adds one watch for the channel linked by channelURL. A zero t counts
// the watch without touching LastWatched.
func record(stats map[string]store.WatchStat, channelURL string, t time.Time) {
	id := channelID(channelURL)
	if id == "" {
		return
	}
	s := stats[id]
	s.Count++
	if t.After(s.LastWatched) {
		s.LastWatched = t
	}
	stats[id] = s
}

// channelID extracts the ID from a https://www.youtube.com/channel/<id> URL.
func channelID(u string) string {
	_, id, ok := strings.Cut(u, "/channel/")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(id, "/?#"); i >= 0 {
		id = id[:i]
	}
	return id
}

func parseJSONHistory(r io.Reader, stats map[string]store.WatchStat) error {
	dec := json.NewDecoder(r)
	if tok, err := dec.Token(); err != nil {
		return fmt.Errorf("parse watch history JSON: %w", err)
	} else if tok != json.Delim('[') {
		return errors.New("parse watch history JSON: expected an array")
	}
	for dec.More() {
		var e struct {
			Time      string `json:"time"`
			Subtitles []struct {
				URL string `json:"url"`
			} `json:"subtitles"`
		}
		if err := dec.Decode(&e); err != nil {
			return fmt.Errorf("parse watch history JSON: %w", err)
		}
		if len(e.Subtitles) == 0 {
			continue
		}
		t, _ := time.Parse(time.RFC3339Nano, e.Time) // zero on failure: still counted
		record(stats, e.Subtitles[0].URL, t)
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("parse watch history JSON: %w", err)
	}
	return nil
}

// htmlDateLayouts are the English date formats seen in Takeout HTML exports.
// Narrow and regular no-break spaces are normalized to plain spaces before
// parsing, which covers the U+202F variant newer exports put before AM/PM.
var htmlDateLayouts = []string{
	"Jan 2, 2006, 3:04:05 PM MST",
	"Jan 2, 2006, 3:04:05PM MST",
	"Jan 2, 2006, 15:04:05 MST",
	"2 Jan 2006, 15:04:05 MST",
	"2 Jan 2006, 3:04:05 PM MST",
	"January 2, 2006, 3:04:05 PM MST",
	"2 January 2006, 15:04:05 MST",
}

func parseHTMLDate(s string) (time.Time, bool) {
	s = strings.Map(func(r rune) rune {
		if r == '\u202f' || r == '\u00a0' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range htmlDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// htmlCell accumulates the parts of one content-cell div.
type htmlCell struct {
	depth      int // nesting depth of divs inside the cell; 0 = not in a cell
	inAnchor   int
	channelURL string
	lines      []string // text outside links, split on <br>
	cur        strings.Builder
}

func (c *htmlCell) breakLine() {
	c.lines = append(c.lines, c.cur.String())
	c.cur.Reset()
}

func (c *htmlCell) flush(stats map[string]store.WatchStat) {
	c.breakLine()
	if c.channelURL != "" {
		var t time.Time
		for _, l := range c.lines {
			if parsed, ok := parseHTMLDate(l); ok {
				t = parsed
				break
			}
		}
		record(stats, c.channelURL, t)
	}
	*c = htmlCell{}
}

func isContentCell(z *html.Tokenizer) bool {
	for {
		key, val, more := z.TagAttr()
		if string(key) == "class" {
			for _, f := range strings.Fields(string(val)) {
				if f == "content-cell" {
					return true
				}
			}
		}
		if !more {
			return false
		}
	}
}

func anchorHref(z *html.Tokenizer) string {
	for {
		key, val, more := z.TagAttr()
		if string(key) == "href" {
			return string(val)
		}
		if !more {
			return ""
		}
	}
}

func parseHTMLHistory(r io.Reader, stats map[string]store.WatchStat) error {
	z := html.NewTokenizer(r)
	var cell htmlCell
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if err := z.Err(); err != io.EOF {
				return fmt.Errorf("parse watch history HTML: %w", err)
			}
			if cell.depth > 0 {
				cell.flush(stats)
			}
			return nil
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "div":
				if tt == html.SelfClosingTagToken {
					continue
				}
				if cell.depth > 0 {
					cell.depth++
				} else if hasAttr && isContentCell(z) {
					cell.depth = 1
				}
			case "br":
				if cell.depth > 0 {
					cell.breakLine()
				}
			case "a":
				if cell.depth == 0 {
					continue
				}
				if hasAttr && cell.channelURL == "" {
					if href := anchorHref(z); strings.Contains(href, "/channel/") {
						cell.channelURL = href
					}
				}
				if tt == html.StartTagToken {
					cell.inAnchor++
				}
			}
		case html.EndTagToken:
			if cell.depth == 0 {
				continue
			}
			name, _ := z.TagName()
			switch string(name) {
			case "div":
				cell.depth--
				if cell.depth == 0 {
					cell.flush(stats)
				}
			case "a":
				if cell.inAnchor > 0 {
					cell.inAnchor--
				}
			}
		case html.TextToken:
			if cell.depth > 0 && cell.inAnchor == 0 {
				cell.cur.Write(z.Text())
			}
		}
	}
}
