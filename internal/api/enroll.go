package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"
)

type tokenStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time
	ttl    time.Duration
	now    func() time.Time
}

func newTokenStore() *tokenStore {
	return &tokenStore{tokens: map[string]time.Time{}, ttl: 15 * time.Minute, now: time.Now}
}

func (t *tokenStore) mint() (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	exp := t.now().Add(t.ttl)
	t.mu.Lock()
	t.tokens[tok] = exp
	// opportunistic GC of expired tokens
	for k, e := range t.tokens {
		if t.now().After(e) {
			delete(t.tokens, k)
		}
	}
	t.mu.Unlock()
	return tok, exp, nil
}

// redeem reports whether the token is valid, consuming it (single use).
func (t *tokenStore) redeem(tok string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.tokens[tok]
	if !ok || t.now().After(exp) {
		delete(t.tokens, tok)
		return false
	}
	delete(t.tokens, tok)
	return true
}

func (s *Server) handleEnrollMint(w http.ResponseWriter, r *http.Request) {
	tok, exp, err := s.tokens.mint()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not mint token"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "expires_at": exp})
}

func (s *Server) handleEnrollRedeem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing token"})
		return
	}
	if !s.tokens.redeem(body.Token) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid or expired token"})
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"repository": "rest:http://" + host + ":8000/hot",
		"password":   s.cfg.Load().Hot.Password,
		"server_url": "http://" + r.Host,
	})
}
