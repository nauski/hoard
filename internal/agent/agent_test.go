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
