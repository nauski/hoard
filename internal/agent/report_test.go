package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func TestFailedBackupReportsOutcome(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}

	// Capture reports the agent POSTs to the "server".
	var mu sync.Mutex
	var sawFailure bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			LastResult *struct {
				OK      bool   `json:"ok"`
				Message string `json:"message"`
			} `json:"last_result"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.LastResult != nil && !body.LastResult.OK {
			mu.Lock()
			sawFailure = true
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	a := &Agent{
		cfgPath:   t.TempDir() + "/c.json",
		log:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		resticBin: "restic",
		// Point at a nonexistent repo dir so the backup fails fast, and at our
		// fake server so reports land on it.
		cfg: Config{Repository: t.TempDir() + "/nope", Host: "ah", ServerURL: srv.URL, Paths: []string{t.TempDir()}},
	}
	os.Setenv("RESTIC_PASSWORD", "x")

	// A backup against an uninitialised repo fails.
	_ = a.Backup(context.Background())

	// Give the final report a moment to land.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := sawFailure
		mu.Unlock()
		if got {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server never received a failing last_result from the agent")
}
