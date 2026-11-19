package youtube

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeAPI serves subscriptions/channels/playlistItems for channels UC0..UC{n-1}.
// playlist(id) decides each uploads playlist's response.
func fakeAPI(t *testing.T, n int, playlist func(w http.ResponseWriter, id string)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/subscriptions") && r.Method == http.MethodGet:
			var items []map[string]any
			for i := range n {
				items = append(items, map[string]any{
					"id": fmt.Sprintf("sub%d", i),
					"snippet": map[string]any{
						"title":      fmt.Sprintf("Channel %d", i),
						"resourceId": map[string]string{"channelId": fmt.Sprintf("UC%d", i)},
					},
				})
			}
			json.NewEncoder(w).Encode(map[string]any{"items": items})
		case strings.HasSuffix(r.URL.Path, "/channels"):
			var items []map[string]any
			for _, id := range strings.Split(r.URL.Query().Get("id"), ",") {
				items = append(items, map[string]any{
					"id": id,
					"statistics": map[string]any{
						"subscriberCount": "1200", "videoCount": "10",
						"hiddenSubscriberCount": id == "UC1",
					},
					"contentDetails": map[string]any{"relatedPlaylists": map[string]string{"uploads": "UU" + id}},
				})
			}
			json.NewEncoder(w).Encode(map[string]any{"items": items})
		case strings.HasSuffix(r.URL.Path, "/playlistItems"):
			playlist(w, strings.TrimPrefix(r.URL.Query().Get("playlistId"), "UU"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Base: srv.URL + "/"}
}

func apiError(w http.ResponseWriter, status int, reason string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"code":%d,"message":"%s","errors":[{"reason":"%s"}]}}`, status, reason, reason)
}

func uploads(w http.ResponseWriter) {
	fmt.Fprint(w, `{"items":[{"snippet":{"title":"New video","publishedAt":"2026-11-01T00:00:00Z"}}]}`)
}

func TestFetchSubscriptionsPartialFailures(t *testing.T) {
	c := fakeAPI(t, 4, func(w http.ResponseWriter, id string) {
		switch id {
		case "UC2":
			apiError(w, http.StatusNotFound, "playlistNotFound") // no uploads: not a failure
		case "UC3":
			apiError(w, http.StatusInternalServerError, "backendError")
		default:
			uploads(w)
		}
	})
	chs, failed, err := c.FetchSubscriptions(t.Context(), func(int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 4 {
		t.Fatalf("got %d channels", len(chs))
	}
	if !slices.Equal(failed, []string{"UC3"}) {
		t.Fatalf("failed = %v, want [UC3]", failed)
	}
	if chs[0].LastUpload.IsZero() || len(chs[0].RecentTitles) != 1 {
		t.Fatalf("uploads not applied: %+v", chs[0])
	}
	if !chs[1].SubscribersHidden || chs[0].SubscribersHidden {
		t.Fatal("hiddenSubscriberCount not mapped")
	}
}

func TestFetchSubscriptionsAbortsOnQuota(t *testing.T) {
	var calls atomic.Int32
	c := fakeAPI(t, 60, func(w http.ResponseWriter, id string) {
		calls.Add(1)
		apiError(w, http.StatusForbidden, "quotaExceeded")
	})
	chs, _, err := c.FetchSubscriptions(t.Context(), func(int, int) {})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.QuotaExceeded() {
		t.Fatalf("want quotaExceeded error, got %v", err)
	}
	if chs != nil {
		t.Fatal("no channels should be returned when the sync aborts")
	}
	if n := calls.Load(); n >= 60 {
		t.Fatalf("expected the sync to stop early, made %d playlist calls", n)
	}
}

func TestUnsubscribeAlreadyGoneIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") == "gone" {
			apiError(w, http.StatusNotFound, "subscriptionNotFound")
			return
		}
		apiError(w, http.StatusForbidden, "quotaExceeded")
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Base: srv.URL + "/"}
	if err := c.Unsubscribe(t.Context(), "gone"); err != nil {
		t.Fatalf("404 should be success, got %v", err)
	}
	if err := c.Unsubscribe(t.Context(), "other"); err == nil {
		t.Fatal("quota error should be returned")
	}
}
