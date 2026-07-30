package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nauski/hoard/internal/restic"
)

func TestSchedulerRestore(t *testing.T) {
	s := newVerifyScheduler(t) // hot repo has one snapshot with a.txt
	target := t.TempDir()
	res, err := s.Restore(context.Background(), "latest", "", target, "always", restic.RestoreHooks{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res == nil || res.FilesRestored == 0 {
		t.Fatalf("expected restored files, got %+v", res)
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
