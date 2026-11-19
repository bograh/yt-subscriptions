package youtube

import (
	"strings"
	"testing"
	"time"

	"yt-subscriptions/internal/store"
)

func mustTime(t *testing.T, layout, v string) time.Time {
	t.Helper()
	tm, err := time.Parse(layout, v)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

const jsonHistory = `[
  {"header":"YouTube","title":"Watched A","titleUrl":"https://www.youtube.com/watch?v=a","subtitles":[{"name":"One","url":"https://www.youtube.com/channel/UC1"}],"time":"2026-10-01T12:00:00.123Z"},
  {"header":"YouTube","title":"Watched B","titleUrl":"https://www.youtube.com/watch?v=b","subtitles":[{"name":"One","url":"https://www.youtube.com/channel/UC1"}],"time":"2026-10-05T08:30:00Z"},
  {"header":"YouTube","title":"Watched C","titleUrl":"https://www.youtube.com/watch?v=c","subtitles":[{"name":"One","url":"https://www.youtube.com/channel/UC1"}],"time":"2026-09-01T00:00:00Z"},
  {"header":"YouTube","title":"Watched D","titleUrl":"https://www.youtube.com/watch?v=d","subtitles":[{"name":"Two","url":"https://www.youtube.com/channel/UC2"}],"time":"2026-08-15T10:00:00Z"},
  {"header":"YouTube","title":"Watched a video that has been removed","time":"2026-10-02T00:00:00Z"},
  {"header":"YouTube","title":"Visited YouTube Music","titleUrl":"https://music.youtube.com/","time":"2026-10-03T00:00:00Z"}
]`

const htmlCellFmt = `<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid">` +
	`<div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">YouTube<br></p></div>` +
	`<div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">%s</div>` +
	`<div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1 mdl-typography--text-right"></div>` +
	`<div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;YouTube<br></div>` +
	`</div></div>`

func htmlEntry(video, channelID, date string) string {
	body := `Watched&nbsp;<a href="https://www.youtube.com/watch?v=` + video + `">Video ` + video + `</a><br>`
	if channelID != "" {
		body += `<a href="https://www.youtube.com/channel/` + channelID + `">Channel ` + channelID + `</a><br>`
	}
	body += date + `<br>`
	return strings.Replace(htmlCellFmt, "%s", body, 1)
}

func htmlDoc(entries ...string) string {
	return `<html><head><meta charset="utf-8"><title>Watch history</title></head><body>` +
		`<div class="mdl-grid">` + strings.Join(entries, "\n") + `</div></body></html>`
}

func TestParseWatchHistory(t *testing.T) {
	jsonWant := map[string]store.WatchStat{
		"UC1": {Count: 3, LastWatched: mustTime(t, time.RFC3339, "2026-10-05T08:30:00Z")},
		"UC2": {Count: 1, LastWatched: mustTime(t, time.RFC3339, "2026-08-15T10:00:00Z")},
	}

	tests := []struct {
		name  string
		input string
		want  map[string]store.WatchStat
	}{
		{name: "json", input: jsonHistory, want: jsonWant},
		{name: "json with BOM and leading whitespace", input: "\uFEFF\n  " + jsonHistory, want: jsonWant},
		{name: "json empty array", input: "[]", want: map[string]store.WatchStat{}},
		{name: "json only skipped entries", input: `[{"title":"Visited YouTube","time":"2026-10-03T00:00:00Z"},{"title":"Watched x","subtitles":[]}]`, want: map[string]store.WatchStat{}},
		{
			name: "html",
			input: htmlDoc(
				htmlEntry("a", "UC1", "Nov 18, 2026, 9:01:02 PM GMT"),
				htmlEntry("b", "UC1", "Nov 10, 2026, 7:00:00\u202fAM GMT"),
				htmlEntry("c", "UC2", "3 Nov 2026, 14:05:06 GMT"),
				htmlEntry("removed", "", "Nov 19, 2026, 1:00:00 AM GMT"),
			),
			want: map[string]store.WatchStat{
				"UC1": {Count: 2, LastWatched: time.Date(2026, 11, 18, 21, 1, 2, 0, time.UTC)},
				"UC2": {Count: 1, LastWatched: time.Date(2026, 11, 3, 14, 5, 6, 0, time.UTC)},
			},
		},
		{
			name: "html with BOM, doctype and unparseable date",
			input: "\uFEFF<!DOCTYPE html>" + htmlDoc(
				htmlEntry("a", "UC1", "18 de nov. de 2026, 21:01:02 GMT"),
				htmlEntry("b", "UC1", "18 de nov. de 2026, 22:01:02 GMT"),
			),
			want: map[string]store.WatchStat{"UC1": {Count: 2}},
		},
		{name: "html without entries", input: htmlDoc(), want: map[string]store.WatchStat{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseWatchHistory(strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d channels %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for id, w := range tc.want {
				g, ok := got[id]
				if !ok {
					t.Fatalf("missing channel %s in %v", id, got)
				}
				if g.Count != w.Count || !g.LastWatched.Equal(w.LastWatched) {
					t.Errorf("%s: got %+v, want %+v", id, g, w)
				}
			}
		})
	}
}

func TestParseWatchHistoryErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"only whitespace and BOM", "\uFEFF  \n"},
		{"plain text", "hello"},
		{"json object instead of array", `{"title":"x"}`},
		{"truncated json", `[{"title":"x","subtitles":[{"url":"https://www.youtube.com/channel/UC1"}]`},
		{"malformed json entry", `[{"title":}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseWatchHistory(strings.NewReader(tc.input)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseHTMLDate(t *testing.T) {
	want := time.Date(2026, 11, 18, 21, 1, 2, 0, time.UTC)
	for _, s := range []string{
		"Nov 18, 2026, 9:01:02 PM GMT",
		"Nov 18, 2026, 9:01:02\u202fPM GMT",
		"Nov 18, 2026, 9:01:02\u00a0PM GMT",
		"  Nov 18, 2026, 9:01:02 PM GMT\n",
		"18 Nov 2026, 21:01:02 GMT",
		"November 18, 2026, 9:01:02 PM GMT",
	} {
		got, ok := parseHTMLDate(s)
		if !ok || !got.Equal(want) {
			t.Errorf("parseHTMLDate(%q) = %v, %v; want %v", s, got, ok, want)
		}
	}
	if _, ok := parseHTMLDate("not a date"); ok {
		t.Error("expected failure for garbage input")
	}
}
