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

// TestPerClientRepos: one host still in the shared repo, two clients with
// their own repositories. The server must see all three as clients, mirror
// what is allowed offsite, apply retention to each repository, and route
// snapshot operations to the repository that holds the snapshot.
func TestPerClientRepos(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}
	ctx := context.Background()
	dir := t.TempDir()
	c := &config.Config{
		Hot:       config.Repo{Repository: filepath.Join(dir, "hot"), Password: "shared"},
		Cold:      config.Repo{Repository: filepath.Join(dir, "cold"), Password: "cold"},
		Clients:   map[string]config.Client{"vm": {Password: "vm-pw", HTTPPassword: "x"}, "laptop": {Password: "laptop-pw", HTTPPassword: "y"}},
		Retention: config.Retention{Last: 1, ApplyToHot: true},
		Mirror:    config.Mirror{ExcludeHosts: []string{"vm"}},
	}
	cfg := config.NewStore(c, "")
	shared := restic.New("restic", c.Hot)
	cold := restic.New("restic", c.Cold)
	vm := restic.New("restic", c.ClientRepo("vm"))
	laptop := restic.New("restic", c.ClientRepo("laptop"))
	if err := shared.EnsureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cold.EnsureInit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*restic.Client{vm, laptop} {
		if err := r.InitLike(ctx, &c.Hot); err != nil {
			t.Fatal(err)
		}
	}
	src := t.TempDir()
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(filepath.Join(src, "f"), []byte{byte('a' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		for host, r := range map[string]*restic.Client{"desktop": shared, "vm": vm, "laptop": laptop} {
			if _, _, err := r.Backup(ctx, []string{src}, nil, host, nil, restic.BackupHooks{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	count := func(r *restic.Client) map[string]int {
		snaps, err := r.Snapshots(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := map[string]int{}
		for _, s := range snaps {
			n[s.Hostname]++
		}
		return n
	}

	st, _ := state.Load("")
	s := New(cfg, "restic", st, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	s.RefreshClients(ctx)
	cl := s.store.Snapshot().Clients
	if len(cl) != 3 || !cl["vm"].OwnRepo || !cl["laptop"].OwnRepo || cl["desktop"].OwnRepo || !cl["vm"].HotOnly {
		t.Fatalf("clients = %+v", cl)
	}

	s.Mirror(ctx)
	// Retention (keep last 1) ran on every hot repository.
	for name, r := range map[string]*restic.Client{"shared": shared, "vm": vm, "laptop": laptop} {
		total := 0
		for _, n := range count(r) {
			total += n
		}
		if total != 1 {
			t.Errorf("%s repo has %d snapshots after retention, want 1", name, total)
		}
	}
	// Offsite holds desktop and laptop, never the hot-only vm.
	if got := count(cold); got["vm"] != 0 || got["desktop"] != 1 || got["laptop"] != 1 {
		t.Fatalf("cold = %v, want desktop:1 laptop:1 and no vm", got)
	}

	// A snapshot operation lands in the repository that holds the snapshot.
	s.RefreshClients(ctx)
	id := s.store.Snapshot().Clients["vm"].SnapshotID
	if err := s.DeleteSnapshot(ctx, id); err != nil {
		t.Fatalf("delete vm snapshot: %v", err)
	}
	if got := count(vm); got["vm"] != 0 {
		t.Fatalf("vm snapshot not deleted from its own repo: %v", got)
	}
	if got := count(laptop); got["laptop"] != 1 {
		t.Fatalf("deleting vm's snapshot touched laptop's repo: %v", got)
	}
}

// TestRequiredPathCompliance: the server flags and alerts when a managed
// client's newest backup does not cover a required path.
func TestRequiredPathCompliance(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}
	ctx := context.Background()
	dir := t.TempDir()
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := &config.Config{
		Hot: config.Repo{Repository: filepath.Join(dir, "hot"), Password: "shared"},
		Clients: map[string]config.Client{"vm": {
			Password: "vm-pw", HTTPPassword: "x",
			Managed: &config.ManagedSettings{Paths: []string{a, b}}, ManagedVersion: 1,
		}},
	}
	cfg := config.NewStore(c, "")
	vm := restic.New("restic", c.ClientRepo("vm"))
	if err := vm.InitLike(ctx, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load("")
	s := New(cfg, "restic", st, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	note := &captureNotifier{}
	s.SetNotifier(note)
	alerts := func() int { note.mu.Lock(); defer note.mu.Unlock(); return len(note.titles) }

	// Compliant backup: both required paths.
	if _, _, err := vm.Backup(ctx, []string{a, b}, nil, "vm", nil, restic.BackupHooks{}); err != nil {
		t.Fatal(err)
	}
	s.RefreshClients(ctx)
	if m := s.store.Snapshot().Clients["vm"].MissingPaths; len(m) != 0 || alerts() != 0 {
		t.Fatalf("compliant backup flagged: %v, alerts %d", m, alerts())
	}
	// The client drops a required path.
	if _, _, err := vm.Backup(ctx, []string{a}, nil, "vm", nil, restic.BackupHooks{}); err != nil {
		t.Fatal(err)
	}
	s.RefreshClients(ctx)
	if m := s.store.Snapshot().Clients["vm"].MissingPaths; len(m) != 1 || m[0] != b {
		t.Fatalf("missing = %v, want [%s]", m, b)
	}
	s.RefreshClients(ctx) // no repeat alert
	if alerts() != 1 {
		t.Fatalf("%d alerts, want exactly 1", alerts())
	}
	// Back in line: the flag clears.
	if _, _, err := vm.Backup(ctx, []string{a, b}, nil, "vm", nil, restic.BackupHooks{}); err != nil {
		t.Fatal(err)
	}
	s.RefreshClients(ctx)
	if m := s.store.Snapshot().Clients["vm"].MissingPaths; len(m) != 0 {
		t.Fatalf("flag did not clear: %v", m)
	}
}
