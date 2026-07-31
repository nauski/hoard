package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

// --- unit tests: tokenStore with an injected clock ---

func TestTokenStoreMintRedeem(t *testing.T) {
	cur := time.Unix(1000, 0)
	ts := newTokenStore()
	ts.now = func() time.Time { return cur }

	tok, _, err := ts.mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok == "" {
		t.Fatal("expected non-empty token")
	}
	if !ts.redeem(tok) {
		t.Fatal("expected first redeem to succeed")
	}
	if ts.redeem(tok) {
		t.Fatal("expected second redeem of same token to fail (single-use)")
	}
}

func TestTokenStoreExpiry(t *testing.T) {
	cur := time.Unix(1000, 0)
	ts := newTokenStore()
	ts.now = func() time.Time { return cur }

	tok, _, err := ts.mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Advance the injected clock past the ttl.
	cur = cur.Add(ts.ttl + time.Second)
	if ts.redeem(tok) {
		t.Fatal("expected redeem of expired token to fail")
	}
}

func TestTokenStoreUnknownToken(t *testing.T) {
	ts := newTokenStore()
	if ts.redeem("does-not-exist") {
		t.Fatal("expected redeem of unknown token to fail")
	}
}

// --- HTTP tests ---

func newEnrollServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.NewStore(&config.Config{Hot: config.Repo{Repository: "rest:http://placeholder:8000/hot", Password: "hotpw-secret"}}, "")
	st, _ := state.Load("")
	sched := scheduler.New(cfg, "restic", st, testLogger())
	return New(cfg, "restic", sched, st, testLogger(), nil)
}

func TestHandleEnrollMint(t *testing.T) {
	srv := newEnrollServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/enroll/mint", nil)
	w := httptest.NewRecorder()
	srv.handleEnrollMint(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("expected non-empty token in mint response")
	}
}

func TestHandleEnrollRedeem(t *testing.T) {
	srv := newEnrollServer(t)

	// Mint via the handler to get a real token.
	mintReq := httptest.NewRequest(http.MethodPost, "/api/enroll/mint", nil)
	mintW := httptest.NewRecorder()
	srv.handleEnrollMint(mintW, mintReq)
	var mintResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(mintW.Body.Bytes(), &mintResp); err != nil {
		t.Fatalf("decode mint: %v", err)
	}

	redeemBody := func(tok string) *bytes.Reader {
		b, _ := json.Marshal(map[string]string{"token": tok})
		return bytes.NewReader(b)
	}

	// Good token.
	req := httptest.NewRequest(http.MethodPost, "/api/enroll/redeem", redeemBody(mintResp.Token))
	req.Host = "truenas:8080"
	w := httptest.NewRecorder()
	srv.handleEnrollRedeem(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("redeem: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Repository string `json:"repository"`
		Password   string `json:"password"`
		ServerURL  string `json:"server_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode redeem: %v", err)
	}
	if resp.Password != "hotpw-secret" {
		t.Fatalf("expected password %q, got %q", "hotpw-secret", resp.Password)
	}
	if resp.Repository != "rest:http://truenas:8000/hot" {
		t.Fatalf("expected repository %q, got %q", "rest:http://truenas:8000/hot", resp.Repository)
	}

	// Bogus token: 403, no password leaked.
	req = httptest.NewRequest(http.MethodPost, "/api/enroll/redeem", redeemBody("bogus"))
	req.Host = "truenas:8080"
	w = httptest.NewRecorder()
	srv.handleEnrollRedeem(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("bogus token: expected 403, got %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "hotpw-secret") {
		t.Fatalf("bogus token response leaked the password: %s", w.Body.String())
	}

	// Same good token redeemed twice: second attempt is 403.
	req = httptest.NewRequest(http.MethodPost, "/api/enroll/redeem", redeemBody(mintResp.Token))
	req.Host = "truenas:8080"
	w = httptest.NewRecorder()
	srv.handleEnrollRedeem(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("second redeem: expected 403, got %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "hotpw-secret") {
		t.Fatalf("second redeem response leaked the password: %s", w.Body.String())
	}
}
