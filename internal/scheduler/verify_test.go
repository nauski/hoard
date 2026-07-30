package scheduler

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/state"
)

// newVerifyScheduler builds a scheduler whose HOT repo is a real local restic
// repo seeded with one snapshot. Skips if restic is absent.
func newVerifyScheduler(t *testing.T) *Scheduler {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}
	repoDir := t.TempDir()
	fixtures := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixtures, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := func(a ...string) {
		cmd := exec.Command("restic", a...)
		cmd.Env = append(os.Environ(), "RESTIC_REPOSITORY="+repoDir, "RESTIC_PASSWORD=test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restic %v: %v\n%s", a, err, out)
		}
	}
	env("init")
	env("backup", "--host", "vhost", fixtures)

	cfg := config.NewStore(&config.Config{
		Hot: config.Repo{Repository: repoDir, Password: "test"},
	}, "")
	st, _ := state.Load("")
	st.SetClients(map[string]state.Client{
		"vhost": {Hostname: "vhost", SnapshotID: "latest"},
	})
	return New(cfg, "restic", st, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

func TestVerifyOK(t *testing.T) {
	s := newVerifyScheduler(t)
	s.Verify(context.Background())
	v := s.store.Snapshot().LastVerify
	if v == nil || !v.OK {
		t.Fatalf("expected successful verify, got %+v", v)
	}
}

func TestVerifyNoClients(t *testing.T) {
	s := newVerifyScheduler(t)
	s.store.SetClients(map[string]state.Client{}) // no clients
	s.Verify(context.Background())
	v := s.store.Snapshot().LastVerify
	// Neutral skip: either nil or OK with a "no eligible" note — never a false failure.
	if v != nil && !v.OK && v.Err != "" {
		t.Fatalf("no-clients verify must not be a hard failure, got %+v", v)
	}
}
