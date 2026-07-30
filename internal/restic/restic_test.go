package restic

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nauski/hoard/internal/config"
)

// newTestRepo inits a local restic repo in a temp dir and backs up a fixture
// tree, returning a Client and the fixture root. Skips if restic is absent.
func newTestRepo(t *testing.T) (*Client, string) {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}
	repoDir := t.TempDir()
	fixtures := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixtures, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(fixtures, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "nested.txt"), []byte("nested data"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New("restic", config.Repo{Repository: repoDir, Password: "test"})
	ctx := context.Background()
	if err := c.EnsureInit(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, _, err := c.Backup(ctx, []string{fixtures}, nil, "testhost", nil, BackupHooks{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	return c, fixtures
}

func TestRestoreWholeSnapshot(t *testing.T) {
	c, _ := newTestRepo(t)
	ctx := context.Background()
	target := t.TempDir()

	res, _, err := c.Restore(ctx, "latest", "", target, "always", false, RestoreHooks{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res == nil || res.FilesRestored == 0 {
		t.Fatalf("expected files restored, got %+v", res)
	}
	// hello.txt must exist somewhere under target with the right contents.
	var found bool
	filepath.Walk(target, func(p string, info os.FileInfo, _ error) error {
		if info != nil && info.Name() == "hello.txt" {
			b, _ := os.ReadFile(p)
			if string(b) == "hello world" {
				found = true
			}
		}
		return nil
	})
	if !found {
		t.Fatal("hello.txt not restored with correct contents")
	}
}

func TestRestoreSubpath(t *testing.T) {
	c, fixtures := newTestRepo(t)
	ctx := context.Background()
	target := t.TempDir()
	// Restore only the "sub" folder from the fixtures.
	sub := filepath.Join(fixtures, "sub")
	if _, _, err := c.Restore(ctx, "latest", sub, target, "always", false, RestoreHooks{}); err != nil {
		t.Fatalf("restore subpath: %v", err)
	}
	var sawNested, sawHello bool
	filepath.Walk(target, func(p string, info os.FileInfo, _ error) error {
		if info == nil {
			return nil
		}
		if info.Name() == "nested.txt" {
			sawNested = true
		}
		if info.Name() == "hello.txt" {
			sawHello = true
		}
		return nil
	})
	if !sawNested {
		t.Fatal("subpath restore missing nested.txt")
	}
	if sawHello {
		t.Fatal("subpath restore should not include hello.txt (outside sub)")
	}
}

func TestRestoreActivityHook(t *testing.T) {
	c, _ := newTestRepo(t)
	ctx := context.Background()
	target := t.TempDir()

	var mu sync.Mutex
	type activity struct{ action, item string }
	var activities []activity

	hooks := RestoreHooks{
		OnActivity: func(action, item string) {
			mu.Lock()
			defer mu.Unlock()
			activities = append(activities, activity{action, item})
		},
	}
	if _, _, err := c.Restore(ctx, "latest", "", target, "always", false, hooks); err != nil {
		t.Fatalf("restore: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(activities) == 0 {
		t.Fatal("OnActivity never fired; expected at least one per-file event")
	}
	var sawHello bool
	for _, a := range activities {
		if filepath.Base(a.item) == "hello.txt" {
			sawHello = true
		}
	}
	if !sawHello {
		t.Fatalf("OnActivity never named hello.txt, got %+v", activities)
	}
}

func TestRestoreCancel(t *testing.T) {
	c, _ := newTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before running
	_, _, err := c.Restore(ctx, "latest", "", t.TempDir(), "always", false, RestoreHooks{})
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestListFiles(t *testing.T) {
	c, _ := newTestRepo(t)
	files, err := c.ListFiles(context.Background(), "latest")
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	var names = map[string]bool{}
	for _, f := range files {
		if f.Type != "file" {
			t.Fatalf("ListFiles returned non-file: %+v", f)
		}
		names[filepath.Base(f.Path)] = true
	}
	if !names["hello.txt"] || !names["nested.txt"] {
		t.Fatalf("expected hello.txt and nested.txt, got %v", names)
	}
}
