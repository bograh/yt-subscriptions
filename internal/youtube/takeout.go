package youtube

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"yt-subscriptions/internal/store"
)

// ParseWatchHistory reads a Google Takeout watch-history.json and aggregates by channel ID.
func ParseWatchHistory(r io.Reader) (map[string]store.WatchStat, error) {
	var entries []struct {
		Time      time.Time `json:"time"`
		Subtitles []struct {
			URL string `json:"url"`
		} `json:"subtitles"`
	}
	if err := json.NewDecoder(r).Decode(&entries); err != nil {
		return nil, err
	}
	stats := make(map[string]store.WatchStat)
	for _, e := range entries {
		if len(e.Subtitles) == 0 {
			continue
		}
		_, id, ok := strings.Cut(e.Subtitles[0].URL, "/channel/")
		if !ok || id == "" {
			continue
		}
		s := stats[id]
		s.Count++
		if e.Time.After(s.LastWatched) {
			s.LastWatched = e.Time
		}
		stats[id] = s
	}
	return stats, nil
}
