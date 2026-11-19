package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"yt-subscriptions/internal/store"
	"yt-subscriptions/internal/youtube"
)

// An expired refresh token (Google answers invalid_grant) must drop the stored
// token and surface ErrReconnect instead of failing forever.
func TestInvalidGrantForgetsToken(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
	}))
	defer tokenSrv.Close()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expired := &oauth2.Token{AccessToken: "old", RefreshToken: "r", Expiry: time.Now().Add(-time.Hour)}
	if err := st.SaveToken(expired); err != nil {
		t.Fatal(err)
	}
	a := New(Options{Store: st, OAuth: &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: tokenSrv.URL}}})

	yt, err := a.youtube()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = yt.FetchSubscriptions(t.Context(), func(int, int) {})
	if !errors.Is(err, ErrReconnect) {
		t.Fatalf("want ErrReconnect, got %v", err)
	}
	if st.Token() != nil {
		t.Fatal("token should have been deleted")
	}
}

func TestCheckAuthDropsTokenOn401(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.SaveToken(&oauth2.Token{AccessToken: "a"})
	a := New(Options{Store: st})

	if err := a.checkAuth(&youtube.APIError{Status: 403, Reason: "quotaExceeded"}); errors.Is(err, ErrReconnect) {
		t.Fatal("quota errors must not disconnect")
	}
	if st.Token() == nil {
		t.Fatal("token deleted on a non-auth error")
	}
	if err := a.checkAuth(&youtube.APIError{Status: 401}); !errors.Is(err, ErrReconnect) {
		t.Fatalf("want ErrReconnect, got %v", err)
	}
	if st.Token() != nil {
		t.Fatal("token should have been deleted")
	}
}
