package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/restic"
	"github.com/nauski/hoard/internal/state"
)

type captureNotifier struct {
	mu     sync.Mutex
	titles []string
}

func (c *captureNotifier) Notify(_ context.Context, title, _ string) {
	c.mu.Lock()
	c.titles = append(c.titles, title)
	c.mu.Unlock()
}

// TestMassDeletionHoldsPreviousSnapshot: a backup that lost most of its files
// raises an alert, flags the client, and holds the snapshot before it so
// retention cannot drop the last good copy.
func TestMassDeletionHoldsPreviousSnapshot(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}
	ctx := context.Background()
	hotRepo := config.Repo{Repository: t.TempDir(), Password: "test"}
	hot := restic.New("restic", hotRepo)
	if err := hot.EnsureInit(ctx); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	wt := filepath.Join(src, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(filepath.Join(wt, fmt.Sprint(i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	backup := func() {
		t.Helper()
		if _, _, err := hot.Backup(ctx, []string{src}, nil, "vm", nil, restic.BackupHooks{}); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.NewStore(&config.Config{
		Hot: hotRepo, Anomaly: config.Anomaly{Percent: 50},
		Retention: config.Retention{Last: 1},
	}, "")
	st, _ := state.Load("")
	s := New(cfg, "restic", st, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	note := &captureNotifier{}
	s.SetNotifier(note)

	backup()
	s.RefreshClients(ctx) // host becomes known; nothing to compare yet
	if a := s.store.Snapshot().Clients["vm"].Anomaly; a != nil {
		t.Fatalf("anomaly on first backup: %+v", a)
	}

	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	backup()
	s.RefreshClients(ctx)

	a := s.store.Snapshot().Clients["vm"].Anomaly
	if a == nil || !a.Held || a.FilesBefore != 10 || a.FilesAfter != 0 {
		t.Fatalf("anomaly = %+v, want held, 10 -> 0 files", a)
	}
	note.mu.Lock()
	alerted := slices.ContainsFunc(note.titles, func(s string) bool { return strings.Contains(s, "vanished") })
	note.mu.Unlock()
	if !alerted {
		t.Fatalf("no alert sent: %v", note.titles)
	}

	// A later refresh must not re-alert or lose the flag.
	s.RefreshClients(ctx)
	if got := s.store.Snapshot().Clients["vm"].Anomaly; got == nil || got.PrevID != a.PrevID {
		t.Fatalf("anomaly changed on refresh: %+v", got)
	}
	note.mu.Lock()
	n := len(note.titles)
	note.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d alerts, want 1", n)
	}

	// keep-last 1 would drop the pre-deletion snapshot; the hold keeps it.
	if _, err := hot.ForgetPrune(ctx, cfg.Load().Retention, restic.StreamHooks{}); err != nil {
		t.Fatal(err)
	}
	snaps, _ := hot.Snapshots(ctx)
	held := slices.ContainsFunc(snaps, func(sn restic.Snapshot) bool {
		return sn.ShortID == a.PrevID && slices.Contains(sn.Tags, restic.TagHold)
	})
	if len(snaps) != 2 || !held {
		t.Fatalf("after retention: %d snapshots, held copy present = %v", len(snaps), held)
	}

	// Snapshots from a pre-0.17 client carry no summary; the server must then
	// size them itself rather than skip the check.
	var before, after restic.Snapshot
	for _, sn := range snaps {
		sn.Summary = nil
		if sn.ShortID == a.PrevID {
			before = sn
		} else {
			after = sn
		}
	}
	old := s.checkAnomaly(ctx, "vm", before, after)
	// The walk counts directories too, so only the size of the drop is exact.
	if old == nil || old.FilesBefore-old.FilesAfter < 10 {
		t.Fatalf("no-summary snapshots: anomaly = %+v, want a drop of at least 10", old)
	}

	s.store.ClearAnomaly("vm")
	s.RefreshClients(ctx)
	if got := s.store.Snapshot().Clients["vm"].Anomaly; got != nil {
		t.Fatalf("anomaly came back after dismiss: %+v", got)
	}
}
