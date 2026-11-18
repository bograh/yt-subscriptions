// Package ai classifies channels using any OpenAI-compatible chat completions API.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"yt-subscriptions/internal/store"
)

var Categories = []string{
	"Technology", "Programming", "Science", "Education", "Gaming", "Music",
	"Entertainment", "Comedy", "Film & Animation", "News & Politics",
	"Business & Finance", "Sports", "Fitness & Health", "Food & Cooking",
	"Travel", "Lifestyle", "Auto & Vehicles", "DIY & Crafts", "Art & Design",
	"Religion & Spirituality", "Other",
}

var ContentTypes = []string{
	"Tutorials & How-tos", "Courses & Lectures", "Video Essays", "Documentaries",
	"News & Commentary", "Reviews & Unboxings", "Podcasts & Interviews", "Vlogs",
	"Let's Plays & Streams", "Music & Performances", "Reactions", "Shorts & Clips",
	"Sketches & Skits", "Other",
}

type Client struct {
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	Model   string
	HTTP    *http.Client
}

type result struct {
	ID          string `json:"id"`
	Category    string `json:"category"`
	ContentType string `json:"content_type"`
	Verdict     string `json:"verdict"`
	Reason      string `json:"reason"`
}

// Analyze classifies a batch of channels (keep batches to ~20-30).
func (c *Client) Analyze(ctx context.Context, interests string, historyImported bool, channels []store.Channel) (map[string]store.Analysis, error) {
	type in struct {
		ID           string   `json:"id"`
		Title        string   `json:"title"`
		Description  string   `json:"description,omitempty"`
		Topics       []string `json:"topics,omitempty"`
		Subscribers  int64    `json:"subscribers"`
		LastUpload   string   `json:"last_upload,omitempty"`
		RecentVideos []string `json:"recent_videos,omitempty"`
		SubscribedOn string   `json:"subscribed_on,omitempty"`
		TimesWatched *int     `json:"times_watched,omitempty"`
		LastWatched  string   `json:"last_watched,omitempty"`
	}
	items := make([]in, 0, len(channels))
	for _, ch := range channels {
		it := in{
			ID:           ch.ID,
			Title:        ch.Title,
			Description:  truncate(ch.Description, 300),
			Topics:       ch.Topics,
			Subscribers:  ch.SubscriberCount,
			RecentVideos: ch.RecentTitles,
			LastUpload:   date(ch.LastUpload),
			SubscribedOn: date(ch.SubscribedAt),
		}
		if historyImported {
			n := ch.WatchCount
			it.TimesWatched = &n
			it.LastWatched = date(ch.LastWatched)
		}
		items = append(items, it)
	}
	payload, _ := json.Marshal(items)

	if strings.TrimSpace(interests) == "" {
		interests = "(not provided — judge mainly on activity and watch behaviour)"
	}
	historyNote := "Watch history is NOT available."
	if historyImported {
		historyNote = "times_watched / last_watched come from the user's real watch history. 0 means they never watched this channel."
	}

	system := fmt.Sprintf(`You help a user clean up their YouTube subscriptions.
For each channel, return:
- category: exactly one of %s
- content_type: the main kind of videos, exactly one of %s
- verdict: "keep", "review", or "unsubscribe"
- reason: one short sentence (max 20 words) explaining the verdict

Guidance:
- Recommend "keep" when the channel clearly matches the user's interests and is still active, or the user watches it regularly.
- Recommend "unsubscribe" when it does not match the interests, has not uploaded in over a year, or the user never/rarely watches it.
- Use "review" when signals conflict or are unclear.
- %s
Today is %s.

User's interests: %s

Respond with ONLY a JSON object: {"results":[{"id":"...","category":"...","content_type":"...","verdict":"...","reason":"..."}]} with one entry per input channel.`,
		quoteList(Categories), quoteList(ContentTypes), historyNote, time.Now().Format("2006-01-02"), interests)

	body, _ := json.Marshal(map[string]any{
		"model":       c.Model,
		"temperature": 0.2,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": string(payload)},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ai: %s: %s", resp.Status, truncate(string(raw), 300))
	}
	var cr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil || len(cr.Choices) == 0 {
		return nil, fmt.Errorf("ai: unexpected response: %s", truncate(string(raw), 300))
	}

	content := extractJSON(cr.Choices[0].Message.Content)
	var parsed struct {
		Results []result `json:"results"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil, fmt.Errorf("ai: could not parse model output: %w", err)
	}

	out := make(map[string]store.Analysis, len(parsed.Results))
	now := time.Now()
	for _, r := range parsed.Results {
		out[r.ID] = store.Analysis{
			Category:    oneOf(r.Category, Categories),
			ContentType: oneOf(r.ContentType, ContentTypes),
			Verdict:     normalizeVerdict(r.Verdict),
			Reason:      r.Reason,
			AnalyzedAt:  now,
		}
	}
	return out, nil
}

// extractJSON strips code fences / chatter around the JSON object some models add.
func extractJSON(s string) string {
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

func oneOf(v string, allowed []string) string {
	for _, a := range allowed {
		if strings.EqualFold(strings.TrimSpace(v), a) {
			return a
		}
	}
	return "Other"
}

func normalizeVerdict(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case store.VerdictKeep:
		return store.VerdictKeep
	case store.VerdictUnsubscribe:
		return store.VerdictUnsubscribe
	default:
		return store.VerdictReview
	}
}

func quoteList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = `"` + x + `"`
	}
	return strings.Join(q, ", ")
}

func date(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
