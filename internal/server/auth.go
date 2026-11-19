package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"yt-subscriptions/internal/store"
	"yt-subscriptions/internal/youtube"
)

// oauthStateTTL is how long a sign-in may take before its state is rejected.
const oauthStateTTL = 10 * time.Minute

// ErrReconnect means Google no longer accepts our stored credentials. Apps in
// Google's "Testing" publishing status get refresh tokens that expire after 7 days.
var ErrReconnect = errors.New("your Google sign-in has expired or was revoked; connect YouTube again")

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.oauth.ClientID == "" {
		http.Error(w, "GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET are not set", http.StatusBadRequest)
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	state := hex.EncodeToString(b)
	s.mu.Lock()
	for st, created := range s.oauthStates { // forget abandoned sign-ins
		if time.Since(created) > oauthStateTTL {
			delete(s.oauthStates, st)
		}
	}
	s.oauthStates[state] = time.Now()
	s.mu.Unlock()
	http.Redirect(w, r, s.oauth.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	s.mu.Lock()
	created, ok := s.oauthStates[state]
	delete(s.oauthStates, state)
	s.mu.Unlock()
	if !ok || time.Since(created) > oauthStateTTL {
		http.Error(w, "invalid or expired login state, try again", http.StatusBadRequest)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "Google sign-in failed: "+e, http.StatusBadRequest)
		return
	}
	tok, err := s.oauth.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := s.store.SaveToken(tok); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleLogout revokes the token at Google (best effort) and forgets it locally.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := s.store.Token(); tok != nil {
		t := cmp.Or(tok.RefreshToken, tok.AccessToken)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/revoke",
			strings.NewReader(url.Values{"token": {t}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if resp, err := http.DefaultClient.Do(req); err != nil {
			log.Printf("revoke token: %v", err)
		} else {
			resp.Body.Close()
		}
	}
	if err := s.store.DeleteToken(); err != nil {
		fail(w, http.StatusInternalServerError, "Couldn't disconnect", err)
		return
	}
	w.Header().Set("HX-Redirect", "/")
}

// youtube returns an API client that persists refreshed tokens and records
// the quota each call costs.
func (s *Server) youtube() (*youtube.Client, error) {
	tok := s.store.Token()
	if tok == nil {
		return nil, errors.New("not connected to YouTube")
	}
	ts := &savingTokenSource{
		src:   s.oauth.TokenSource(context.Background(), tok),
		store: s.store,
		last:  tok.AccessToken,
	}
	return &youtube.Client{
		HTTP: oauth2.NewClient(context.Background(), ts),
		Base: s.ytBase,
		OnCost: func(units int) {
			if err := s.store.AddQuotaUsage(context.Background(), units); err != nil {
				log.Printf("record quota usage: %v", err)
			}
		},
	}, nil
}

// checkAuth forgets the stored token when YouTube rejects it, and returns the
// error to report (ErrReconnect in that case).
func (s *Server) checkAuth(err error) error {
	if errors.Is(err, ErrReconnect) {
		return ErrReconnect
	}
	var apiErr *youtube.APIError
	if errors.As(err, &apiErr) && apiErr.Unauthorized() {
		if derr := s.store.DeleteToken(); derr != nil {
			log.Printf("delete token: %v", derr)
		}
		return ErrReconnect
	}
	return err
}

// savingTokenSource stores each newly refreshed token, and turns Google's
// invalid_grant into ErrReconnect after forgetting the dead token.
type savingTokenSource struct {
	mu    sync.Mutex
	src   oauth2.TokenSource
	store *store.Store
	last  string
}

func (ts *savingTokenSource) Token() (*oauth2.Token, error) {
	t, err := ts.src.Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
			if derr := ts.store.DeleteToken(); derr != nil {
				log.Printf("delete token: %v", derr)
			}
			return nil, ErrReconnect
		}
		return nil, err
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if t.AccessToken != ts.last {
		ts.last = t.AccessToken
		if err := ts.store.SaveToken(t); err != nil {
			log.Printf("save token: %v", err)
		}
	}
	return t, nil
}
