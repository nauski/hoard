package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
