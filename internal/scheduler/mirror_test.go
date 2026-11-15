package scheduler

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/restic"
	"github.com/nauski/hoard/internal/state"
)

// TestMirrorHotOnlyAndHotRetention runs the whole nightly job against two
// local repos: a hot-only host must never reach cold, hot retention is opt-in,
// and once on it cleans up what had accumulated.
func TestMirrorHotOnlyAndHotRetention(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}
	ctx := context.Background()
	hotRepo := config.Repo{Repository: t.TempDir(), Password: "test"}
	coldRepo := config.Repo{Repository: t.TempDir(), Password: "test"}
	hot, cold := restic.New("restic", hotRepo), restic.New("restic", coldRepo)
	for _, c := range []*restic.Client{hot, cold} {
		if err := c.EnsureInit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	src := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte{byte('a' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, h := range []string{"vm", "desk"} {
			if _, _, err := hot.Backup(ctx, []string{src}, nil, h, nil, restic.BackupHooks{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	count := func(c *restic.Client) map[string]int {
		snaps, err := c.Snapshots(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := map[string]int{}
		for _, s := range snaps {
			n[s.Hostname]++
		}
		return n
	}

	cfg := config.NewStore(&config.Config{
		Hot: hotRepo, Cold: coldRepo,
		Retention: config.Retention{Last: 1, Hosts: map[string]config.Retention{"vm": {Last: 2}}},
		Mirror:    config.Mirror{ExcludeHosts: []string{"vm"}},
	}, "")
	st, _ := state.Load("")
	s := New(cfg, "restic", st, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Hot retention off: hot keeps everything, cold gets desk only, pruned to 1.
	s.Mirror(ctx)
	if got := count(hot); got["vm"] != 3 || got["desk"] != 3 {
		t.Fatalf("hot changed with apply_to_hot off: %v", got)
	}
	if got := count(cold); got["vm"] != 0 || got["desk"] != 1 {
		t.Fatalf("cold = %v, want desk:1 and no vm", got)
	}

	// Turned on afterwards: the next run cleans up the backlog on hot.
	if err := cfg.Update(func(c *config.Config) { c.Retention.ApplyToHot = true }); err != nil {
		t.Fatal(err)
	}
	s.Mirror(ctx)
	if got := count(hot); got["vm"] != 2 || got["desk"] != 1 {
		t.Fatalf("hot after opt-in = %v, want vm:2 desk:1", got)
	}
	if got := count(cold); got["vm"] != 0 || got["desk"] != 1 {
		t.Fatalf("cold = %v, want desk:1 and no vm", got)
	}

	s.RefreshClients(ctx)
	cl := s.store.Snapshot().Clients
	if !cl["vm"].HotOnly || cl["desk"].HotOnly {
		t.Fatalf("hot_only flags wrong: vm=%v desk=%v", cl["vm"].HotOnly, cl["desk"].HotOnly)
	}
}
