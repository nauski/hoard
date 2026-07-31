package restic

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nauski/hoard/internal/config"
)

func TestParseProgress(t *testing.T) {
	pct, done, total, ok := parseProgress("[0:01] 48.39%  15 / 31 packs copied")
	if !ok || done != 15 || total != 31 {
		t.Fatalf("got %v %d/%d ok=%v", pct, done, total, ok)
	}
	if pct < 48.38 || pct > 48.40 {
		t.Fatalf("pct %v", pct)
	}
	if _, _, _, ok := parseProgress("snapshot abc saved"); ok {
		t.Fatal("non-progress line matched")
	}
	if _, _, _, ok := parseProgress(""); ok {
		t.Fatal("empty matched")
	}
}

// TestCopyFromStreams exercises the real restic CLI: init two temp repos,
// back up a few MB into the source, then copy into the destination and
// verify the StreamHooks fire with a real progress line and activity output.
func TestCopyFromStreams(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	fixtures := t.TempDir()

	// A few MB of incompressible data so the copy takes long enough to emit
	// at least one textual progress line.
	data := make([]byte, 8<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "big.dat"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	src := New("restic", config.Repo{Repository: srcDir, Password: "test"})
	if err := src.EnsureInit(ctx); err != nil {
		t.Fatalf("init src: %v", err)
	}
	if _, _, err := src.Backup(ctx, []string{fixtures}, nil, "testhost", nil, BackupHooks{}); err != nil {
		t.Fatalf("backup src: %v", err)
	}

	dst := New("restic", config.Repo{Repository: dstDir, Password: "test"})
	if err := dst.EnsureInit(ctx); err != nil {
		t.Fatalf("init dst: %v", err)
	}

	var mu sync.Mutex
	var sawProgress bool
	var lastTotal int
	var lines []string

	hooks := StreamHooks{
		OnProgress: func(pct float64, done, total int) {
			mu.Lock()
			defer mu.Unlock()
			sawProgress = true
			lastTotal = total
		},
		OnActivity: func(line string) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, line)
		},
	}

	out, err := dst.CopyFrom(ctx, src.repo, hooks)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("expected non-empty output")
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawProgress || lastTotal <= 0 {
		t.Fatalf("expected OnProgress to fire with total>0, sawProgress=%v total=%d", sawProgress, lastTotal)
	}
	var sawSnapshot bool
	for _, l := range lines {
		if strings.Contains(l, "snapshot") {
			sawSnapshot = true
			break
		}
	}
	if !sawSnapshot {
		t.Fatalf("expected an activity line mentioning snapshot, got %v", lines)
	}
}
