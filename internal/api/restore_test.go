package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

func newRestoreServer(t *testing.T) (*Server, string) {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}
	repoDir := t.TempDir()
	fixtures := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixtures, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(a ...string) {
		cmd := exec.Command("restic", a...)
		cmd.Env = append(os.Environ(), "RESTIC_REPOSITORY="+repoDir, "RESTIC_PASSWORD=test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restic %v: %v\n%s", a, err, out)
		}
	}
	run("init")
	run("backup", fixtures)

	cfg := config.NewStore(&config.Config{Hot: config.Repo{Repository: repoDir, Password: "test"}}, "")
	st, _ := state.Load("")
	sched := scheduler.New(cfg, "restic", st, testLogger())
	srv := New(cfg, "restic", sched, st, testLogger(), nil)
	return srv, t.TempDir()
}

func TestHandleRestore(t *testing.T) {
	srv, target := newRestoreServer(t)
	body, _ := json.Marshal(map[string]string{"id": "latest", "target": target})
	req := httptest.NewRequest(http.MethodPost, "/api/restore", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRestore(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body.String())
	}
	// The async restore should land a.txt under target within a few seconds.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var found bool
		filepath.Walk(target, func(p string, info os.FileInfo, _ error) error {
			if info != nil && info.Name() == "a.txt" {
				found = true
			}
			return nil
		})
		if found {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("restore did not produce a.txt in time")
}

// TestHandleRestore_RejectsConcurrent guards against a lost-cancel race: if a
// restore is already in flight (restoreCancel set), a second POST must be
// rejected with 409 and must NOT touch the existing restoreCancel — otherwise
// a rejected request's cleanup could nil out the winner's cancel func, and a
// later cancel click would silently no-op while the real restore kept running.
func TestHandleRestore_RejectsConcurrent(t *testing.T) {
	srv, target := newRestoreServer(t)

	// Simulate an in-flight restore by pre-populating restoreCancel, as the
	// real handler would while a restore goroutine is running.
	sentinel := func() {}
	srv.restoreMu.Lock()
	srv.restoreCancel = sentinel
	srv.restoreMu.Unlock()

	body, _ := json.Marshal(map[string]string{"id": "latest", "target": target})
	req := httptest.NewRequest(http.MethodPost, "/api/restore", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRestore(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body.String())
	}

	srv.restoreMu.Lock()
	got := srv.restoreCancel
	srv.restoreMu.Unlock()
	if got == nil {
		t.Fatal("rejected request cleared restoreCancel; the in-flight restore's cancel was lost")
	}
}

func TestStatusIncludesLastVerify(t *testing.T) {
	srv, _ := newRestoreServer(t)
	srv.store.SetVerify(state.VerifyResult{Time: time.Now(), OK: true, Client: "vhost", File: "/a.txt", Bytes: 5})
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	srv.handleStatus(w, req)
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["last_verify"] == nil {
		t.Fatalf("status missing last_verify: %s", w.Body.String())
	}
}

func TestConfigRoundTripsVerify(t *testing.T) {
	srv, _ := newRestoreServer(t)
	body := `{"schedule":{"mirror":"","check":"","check_weekday":null,"stale_after":"26h","verify":"03:30"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	srv.handleSetConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("set config: %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/config", nil)
	w = httptest.NewRecorder()
	srv.handleGetConfig(w, req)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"verify":"03:30"`)) {
		t.Fatalf("verify not persisted in config: %s", w.Body.String())
	}
}
