package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/restic"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
		Host  string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing token"})
		return
	}
	private := s.cfg.Load().PrivateRepos
	// Check the name before spending the single-use token.
	if private && !config.ValidClientName(body.Host) {
		msg := "this server gives each client its own repository and needs the client's host name; use hoard-agent 0.29 or newer"
		if body.Host != "" {
			msg = "host name must be letters, digits, dot, dash or underscore"
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	if !s.tokens.redeem(body.Token) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid or expired token"})
		return
	}
	nas := r.Host
	if h, _, err := net.SplitHostPort(nas); err == nil {
		nas = h
	}
	if !private {
		writeJSON(w, http.StatusOK, map[string]string{
			"repository": "rest:http://" + nas + ":8000/hot",
			"password":   s.cfg.Load().Hot.Password,
			"server_url": "http://" + r.Host,
		})
		return
	}
	cl, err := s.enrollClient(r.Context(), body.Host)
	if err != nil {
		s.log.Error("enroll client", "host", body.Host, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"repository": "rest:http://" + body.Host + ":" + cl.HTTPPassword + "@" + nas + ":8000/" + body.Host + "/",
		"password":   cl.Password,
		"server_url": "http://" + r.Host,
	})
}

// enrollClient returns host's credentials, creating its login and its own
// repository on first enrollment. Enrolling an existing client again hands
// back the same credentials (the caller already proved it holds a token).
func (s *Server) enrollClient(ctx context.Context, host string) (config.Client, error) {
	if cl, ok := s.cfg.Load().Clients[host]; ok {
		return cl, nil
	}
	pw, err := randomSecret()
	if err != nil {
		return config.Client{}, err
	}
	httpPW, err := randomSecret()
	if err != nil {
		return config.Client{}, err
	}
	cl := config.Client{Password: pw, HTTPPassword: httpPW}
	err = s.cfg.Update(func(c *config.Config) {
		clients := maps.Clone(c.Clients) // the old config snapshot is still in use
		if clients == nil {
			clients = map[string]config.Client{}
		}
		clients[host] = cl
		c.Clients = clients
	})
	if err != nil {
		return config.Client{}, fmt.Errorf("save client: %w", err)
	}
	cfg := s.cfg.Load()
	if err := os.MkdirAll(cfg.ReposDir(), 0o700); err != nil {
		return config.Client{}, err
	}
	// Share the shared repo's chunker settings when it exists, so the same
	// files on two machines still deduplicate in the offsite copy.
	var like *config.Repo
	if _, err := os.Stat(filepath.Join(cfg.Hot.Repository, "config")); err == nil {
		like = &cfg.Hot
	}
	ictx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := restic.New(s.resticBin, cfg.ClientRepo(host)).InitLike(ictx, like); err != nil {
		return config.Client{}, err
	}
	if err := cfg.WriteHtpasswd(); err != nil {
		return config.Client{}, fmt.Errorf("write login file: %w", err)
	}
	s.waitForLogin(ctx, host, cl.HTTPPassword)
	return cl, nil
}

// waitForLogin holds the enrollment reply until rest-server accepts the new
// login. rest-server re-reads its login file on its own schedule (up to about
// half a minute), and a backup started before that would be refused. Gives up
// quietly after 45 s, or at once if rest-server is not reachable from here.
func (s *Server) waitForLogin(ctx context.Context, host, password string) {
	port := "8000"
	if _, p, err := net.SplitHostPort(os.Getenv("HOARD_REST_ADDR")); err == nil && p != "" {
		port = p
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://127.0.0.1:"+port+"/"+host+"/config", nil)
		if err != nil {
			return
		}
		req.SetBasicAuth(host, password)
		resp, err := s.client.Do(req)
		if err != nil {
			return // no rest-server here (tests, split deployments)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			return
		}
		time.Sleep(2 * time.Second)
	}
}
