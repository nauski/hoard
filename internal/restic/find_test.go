package restic

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nauski/hoard/internal/config"
)

func TestTimeline(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	snaps := []Snapshot{
		{ID: "c", ShortID: "c", Time: t0.Add(2 * time.Hour)},
		{ID: "a", ShortID: "a", Time: t0},
		{ID: "b", ShortID: "b", Time: t0.Add(time.Hour)},
	}
	dir := FindMatch{Path: "/h/wt-perf", Type: "dir"}
	child := FindMatch{Path: "/h/wt-perf/src/main.go", Type: "file"}
	keep := FindMatch{Path: "/h/notes/wt-perf", Type: "file"}
	hits := []FindHit{
		{Snapshot: "a", Matches: []FindMatch{dir, child, keep}},
		{Snapshot: "b", Matches: []FindMatch{dir, child, keep}},
		{Snapshot: "c", Matches: []FindMatch{keep}},
		{Snapshot: "other-host", Matches: []FindMatch{dir}},
	}
	got := Timeline(hits, snaps)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (child collapsed into its directory): %+v", len(got), got)
	}
	gone, present := got[0], got[1] // vanished paths sort first
	if gone.Path != "/h/wt-perf" || gone.Versions != 2 || gone.LastID != "b" || gone.GoneID != "c" || gone.GoneTime == nil {
		t.Fatalf("vanished entry wrong: %+v", gone)
	}
	if present.Path != "/h/notes/wt-perf" || present.Versions != 3 || present.LastID != "c" || present.GoneTime != nil {
		t.Fatalf("present entry wrong: %+v", present)
	}
}

// TestFindAndLastContaining uses the real CLI: a directory is backed up twice,
// deleted, and backed up again.
func TestFindAndLastContaining(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}
	ctx := context.Background()
	cl := New("restic", config.Repo{Repository: t.TempDir(), Password: "test"})
	if err := cl.EnsureInit(ctx); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	wt := filepath.Join(src, "wt-perf")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	backup := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(src, "keep.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := cl.Backup(ctx, []string{src}, nil, "h", nil, BackupHooks{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // distinct snapshot times
	}
	if err := os.WriteFile(filepath.Join(wt, "a.go"), []byte("package a"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup("1")
	backup("2")
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	backup("3")

	found, err := cl.FindTimeline(ctx, "h", "wt-perf")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Path != wt || found[0].Versions != 2 || found[0].GoneTime == nil {
		t.Fatalf("timeline = %+v, want %s in 2 versions, then missing", found, wt)
	}

	snap, typ, err := cl.LastContaining(ctx, "h", wt, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if typ != "dir" || snap.ShortID != found[0].LastID {
		t.Fatalf("LastContaining = %s (%s), want %s (dir)", snap.ShortID, typ, found[0].LastID)
	}
	// -before the second backup must pick the first.
	first, _, err := cl.LastContaining(ctx, "h", wt, snap.Time)
	if err != nil || !first.Time.Before(snap.Time) {
		t.Fatalf("before filter: %v %v", first, err)
	}
	if _, _, err := cl.LastContaining(ctx, "h", filepath.Join(src, "never"), time.Time{}); err == nil {
		t.Fatal("expected an error for a path that was never backed up")
	}
}

// TestPinnedSnapshotSurvivesRetention: keep-last 1 would drop the older
// snapshot, but it is pinned.
func TestPinnedSnapshotSurvivesRetention(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}
	ctx := context.Background()
	cl := New("restic", config.Repo{Repository: t.TempDir(), Password: "test"})
	if err := cl.EnsureInit(ctx); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	for i, tags := range [][]string{{TagPin, "note:before cleanup"}, nil, nil} {
		if err := os.WriteFile(filepath.Join(src, "f"), []byte{byte('a' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := cl.Backup(ctx, []string{src}, nil, "h", tags, BackupHooks{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cl.ForgetPrune(ctx, config.Retention{Last: 1}, StreamHooks{}); err != nil {
		t.Fatal(err)
	}
	snaps, _ := cl.Snapshots(ctx)
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots left, want 2 (newest + pinned)", len(snaps))
	}
	// Unpin, and retention takes it.
	for _, s := range snaps {
		if len(s.Tags) > 0 {
			if err := cl.Tag(ctx, s.ID, nil, []string{TagPin}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := cl.ForgetPrune(ctx, config.Retention{Last: 1}, StreamHooks{}); err != nil {
		t.Fatal(err)
	}
	if snaps, _ = cl.Snapshots(ctx); len(snaps) != 1 {
		t.Fatalf("%d snapshots left after unpin, want 1", len(snaps))
	}
}
