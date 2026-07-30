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

// TestBackupOutcomeExclusions verifies backupOutcome()'s filtering rules
// directly, without needing restic on PATH: restores never produce an
// outcome, a user-cancelled backup never produces an outcome, and a
// completed backup (success or genuine failure) reports its OK/Message
// as-is.
func TestBackupOutcomeExclusions(t *testing.T) {
	t.Run("restore is excluded", func(t *testing.T) {
		a := &Agent{lastRun: RunResult{Kind: "restore", OK: false, Message: "x"}}
		if got := a.backupOutcome(); got != nil {
			t.Fatalf("backupOutcome() = %+v, want nil for a restore run", got)
		}
	})

	t.Run("cancelled backup is excluded", func(t *testing.T) {
		a := &Agent{lastRun: RunResult{Kind: "backup", OK: false, Message: "cancelled"}}
		if got := a.backupOutcome(); got != nil {
			t.Fatalf("backupOutcome() = %+v, want nil for a cancelled backup", got)
		}
	})

	t.Run("successful backup is reported", func(t *testing.T) {
		a := &Agent{lastRun: RunResult{Kind: "backup", OK: true, Message: "done"}}
		got := a.backupOutcome()
		if got == nil || !got.OK || got.Message != "done" {
			t.Fatalf("backupOutcome() = %+v, want {OK:true, Message:done}", got)
		}
	})

	t.Run("failed backup is reported", func(t *testing.T) {
		a := &Agent{lastRun: RunResult{Kind: "backup", OK: false, Message: "boom"}}
		got := a.backupOutcome()
		if got == nil || got.OK || got.Message != "boom" {
			t.Fatalf("backupOutcome() = %+v, want {OK:false, Message:boom}", got)
		}
	})
}

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
