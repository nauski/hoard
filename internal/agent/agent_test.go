package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newAgent(t *testing.T) (*Agent, string) {
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

	a := &Agent{
		cfgPath:   filepath.Join(t.TempDir(), "cfg.json"),
		log:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		resticBin: "restic",
		cfg:       Config{Repository: repoDir, Host: "ahost"},
	}
	os.Setenv("RESTIC_PASSWORD", "test")
	return a, t.TempDir()
}

func TestAgentRestore(t *testing.T) {
	a, target := newAgent(t)
	if err := a.Restore(context.Background(), "latest", "", target, "folder"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	rr, running := a.LastRun()
	if running {
		t.Fatal("should not be running after restore returns")
	}
	if rr.Kind != "restore" || !rr.OK {
		t.Fatalf("unexpected run result: %+v", rr)
	}
	var found bool
	filepath.Walk(target, func(p string, info os.FileInfo, _ error) error {
		if info != nil && info.Name() == "a.txt" {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatal("a.txt not restored")
	}
}

func TestMakeDir(t *testing.T) {
	parent := t.TempDir()

	// Creates a new folder and returns its absolute path.
	abs, err := makeDir(parent, "new one")
	if err != nil {
		t.Fatalf("makeDir: %v", err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		t.Fatalf("expected a directory at %s, err=%v", abs, err)
	}
	if got := filepath.Dir(abs); got != parent {
		t.Fatalf("created outside parent: %s not in %s", abs, parent)
	}

	// Idempotent: creating the same folder again succeeds.
	if _, err := makeDir(parent, "new one"); err != nil {
		t.Fatalf("re-create should succeed, got %v", err)
	}

	// Rejects names that could escape the parent.
	for _, bad := range []string{"", "  ", ".", "..", "a/b", `a\b`} {
		if _, err := makeDir(parent, bad); err == nil {
			t.Fatalf("expected error for name %q", bad)
		}
	}
	// A traversal attempt must not create anything above parent.
	if _, err := os.Stat(filepath.Join(filepath.Dir(parent), "b")); err == nil {
		t.Fatal("traversal name created a dir outside parent")
	}
}

func TestAgentConfigRoundTripsLimits(t *testing.T) {
	a := &Agent{cfgPath: t.TempDir() + "/c.json", log: slog.New(slog.NewTextHandler(os.Stderr, nil)), resticBin: "restic",
		cfg: Config{Host: "h"}}
	if err := a.SetConfig(Config{Repository: "rest:http://x", Host: "h", LimitUploadKiBps: 1200, LimitDownloadKiBps: 600}); err != nil {
		t.Fatal(err)
	}
	c := a.GetConfig()
	if c.LimitUploadKiBps != 1200 || c.LimitDownloadKiBps != 600 {
		t.Fatalf("limits not round-tripped: %+v", c)
	}
}

func TestSetConfigPreservesUnsentFields(t *testing.T) {
	a := &Agent{
		cfgPath: t.TempDir() + "/c.json",
		log:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		cfg:     Config{Host: "h", ServerURL: "http://srv:8080", Tags: []string{"mytag"}},
	}
	// A settings save that omits ServerURL and Tags (as the agent UI does) must
	// not wipe them.
	if err := a.SetConfig(Config{Repository: "rest:http://x", Host: "h"}); err != nil {
		t.Fatal(err)
	}
	c := a.GetConfig()
	if c.ServerURL != "http://srv:8080" {
		t.Fatalf("ServerURL wiped: %q", c.ServerURL)
	}
	if len(c.Tags) != 1 || c.Tags[0] != "mytag" {
		t.Fatalf("Tags wiped: %v", c.Tags)
	}
	if c.Repository != "rest:http://x" {
		t.Fatalf("Repository not set: %q", c.Repository)
	}
}

func TestBackupWithUnreadableFileIsOKWithWarnings(t *testing.T) {
	a, _ := newAgent(t)
	if os.Geteuid() == 0 {
		t.Skip("root can read chmod 000 files")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "ok.txt"), []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "locked.txt"), []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	a.cfg.Paths = []string{src}

	if err := a.Backup(context.Background()); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	rr, _ := a.LastRun()
	if !rr.OK {
		t.Fatalf("run not OK: %s", rr.Message)
	}
	if len(rr.Warnings) != 1 || !strings.Contains(rr.Warnings[0], "locked.txt") {
		t.Fatalf("warnings = %q", rr.Warnings)
	}
	if !strings.Contains(rr.Message, "1 files could not be read") {
		t.Fatalf("message = %q", rr.Message)
	}
}

// TestRestorePathBesideOriginal: a deleted directory and a deleted file each
// come back from the newest snapshot that has them, into a side folder.
func TestRestorePathBesideOriginal(t *testing.T) {
	a, _ := newAgent(t)
	src := t.TempDir()
	wt := filepath.Join(src, "wt-perf")
	if err := os.MkdirAll(filepath.Join(wt, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(src, "notes.txt")
	for p, c := range map[string]string{filepath.Join(wt, "sub", "a.go"): "package a", file: "remember"} {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a.cfg.Paths = []string{src}
	ctx := context.Background()
	if err := a.BackupPin(ctx, "before, cleanup"); err != nil {
		t.Fatal(err)
	}
	snaps, err := a.Snapshots(ctx)
	if err != nil || len(snaps) == 0 {
		t.Fatalf("snapshots: %v", err)
	}
	if got := strings.Join(snaps[0].Tags, "|"); got != "pin|note:before  cleanup" {
		t.Fatalf("pin tags = %q", got)
	}

	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := a.Backup(ctx); err != nil { // the deletion is now the newest snapshot
		t.Fatal(err)
	}

	dest, err := a.RestorePath(ctx, wt, time.Time{}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dest, wt+".restored-") {
		t.Fatalf("dest = %s", dest)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "sub", "a.go")); err != nil || string(b) != "package a" {
		t.Fatalf("directory not restored into %s: %v", dest, err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("restore touched the original location")
	}

	to := filepath.Join(t.TempDir(), "out")
	if _, err := a.RestorePath(ctx, file, time.Time{}, to, false); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(to, "notes.txt")); err != nil || string(b) != "remember" {
		t.Fatalf("file not restored into %s: %v", to, err)
	}
}

// TestRetryLockOnlyOnNewRestic: --retry-lock arrived in restic 0.16; passing
// it to an older binary would fail every backup.
func TestRetryLockOnlyOnNewRestic(t *testing.T) {
	fake := func(version string) *Agent {
		bin := filepath.Join(t.TempDir(), "restic")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'restic "+version+" compiled with go'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return &Agent{resticBin: bin}
	}
	if fake("0.12.1").canRetryLock() {
		t.Error("0.12.1 must not get --retry-lock")
	}
	if !fake("0.16.4").canRetryLock() {
		t.Error("0.16.4 should get --retry-lock")
	}
	if !fake("0.19.1").canRetryLock() {
		t.Error("0.19.1 should get --retry-lock")
	}
	if (&Agent{resticBin: "/nonexistent/restic"}).canRetryLock() {
		t.Error("unknown version must not get --retry-lock")
	}
}
