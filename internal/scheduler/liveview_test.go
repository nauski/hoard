package scheduler

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/state"
)

// newMirrorScheduler builds a scheduler with real local Hot AND Cold restic
// repos (Hot seeded with one snapshot of several MB of random data, Cold
// pre-initialized), so Mirror/Check exercise the real restic streaming path
// long enough for a concurrent goroutine to observe the live job view.
// Skips if restic is absent.
func newMirrorScheduler(t *testing.T) *Scheduler {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}
	hotDir := t.TempDir()
	coldDir := t.TempDir()
	fixtures := t.TempDir()

	// Enough incompressible data that copy/check take long enough for a
	// poller to observe the job mid-flight (mirrors stream_test.go's approach).
	data := make([]byte, 16<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "big.dat"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(repo string, a ...string) {
		cmd := exec.Command("restic", a...)
		cmd.Env = append(os.Environ(), "RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD=test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restic %v: %v\n%s", a, err, out)
		}
	}
	run(hotDir, "init")
	run(hotDir, "backup", "--host", "vhost", fixtures)
	run(coldDir, "init")

	cfg := config.NewStore(&config.Config{
		Hot:  config.Repo{Repository: hotDir, Password: "test"},
		Cold: config.Repo{Repository: coldDir, Password: "test"},
	}, "")
	st, _ := state.Load("")
	return New(cfg, "restic", st, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// recordingNotifier is a Notifier stub that records every alert fired, so
// cancel tests can assert a user-initiated cancel never raises one.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []string
}

func (n *recordingNotifier) Notify(_ context.Context, title, body string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, title+": "+body)
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

// pollUntil waits for cond() to become true, polling every interval, and
// fails the test if it doesn't happen within timeout.
func pollUntil(t *testing.T, timeout, interval time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(interval)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

func TestServerJobNilWhenIdle(t *testing.T) {
	s := newVerifyScheduler(t)
	if v := s.ServerJob(); v != nil {
		t.Fatalf("expected nil ServerJob when idle, got %+v", v)
	}
}

func TestServerJobDuringAndAfterMirror(t *testing.T) {
	s := newMirrorScheduler(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Mirror(context.Background())
	}()

	// Observe the live view mid-flight.
	pollUntil(t, 5*time.Second, 5*time.Millisecond, func() bool {
		return s.ServerJob() != nil
	})
	v := s.ServerJob()
	if v == nil {
		t.Fatal("expected non-nil ServerJob while mirror runs")
	}
	if v.Name == "" {
		t.Fatalf("expected a job name, got %+v", v)
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("mirror did not finish in time")
	}

	if v := s.ServerJob(); v != nil {
		t.Fatalf("expected nil ServerJob after mirror finished, got %+v", v)
	}
}

func TestCancelServerJobIdleFalse(t *testing.T) {
	s := newVerifyScheduler(t)
	if s.CancelServerJob() {
		t.Fatal("expected CancelServerJob to return false when idle")
	}
}

func TestCancelServerJobTrueWhenStored(t *testing.T) {
	s := newVerifyScheduler(t)
	var cancelled bool
	s.beginJob("mirror", func() { cancelled = true })
	if !s.CancelServerJob() {
		t.Fatal("expected CancelServerJob to return true when a cancel func is stored")
	}
	if !cancelled {
		t.Fatal("expected the stored cancel func to have been invoked")
	}
}

// TestCancelMirrorRecordsNoAlert cancels a real in-flight mirror and asserts
// the recorded job is a "cancelled" result with no alert fired — a
// user-initiated cancel is not a failure. Mirrors the agent's backup-cancel
// handling (internal/agent/agent.go).
func TestCancelMirrorRecordsNoAlert(t *testing.T) {
	s := newMirrorScheduler(t)
	note := &recordingNotifier{}
	s.SetNotifier(note)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Mirror(context.Background())
	}()

	pollUntil(t, 5*time.Second, 5*time.Millisecond, func() bool {
		return s.ServerJob() != nil
	})
	if !s.CancelServerJob() {
		t.Fatal("expected CancelServerJob to return true while mirror runs")
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("cancelled mirror did not finish in time")
	}

	last := s.store.Snapshot().LastByJob["mirror"]
	if last.OK {
		t.Fatalf("expected cancelled mirror to record OK=false, got %+v", last)
	}
	if last.Message != "cancelled" {
		t.Fatalf(`expected message "cancelled", got %q`, last.Message)
	}
	if n := note.count(); n != 0 {
		t.Fatalf("expected no alerts fired on cancel, got %d: %v", n, note.calls)
	}

	if v := s.ServerJob(); v != nil {
		t.Fatalf("expected nil ServerJob after cancelled mirror, got %+v", v)
	}
}

// TestJobLineCap exercises jobLine's 200-line tail cap directly, without a
// real restic run.
func TestJobLineCap(t *testing.T) {
	s := &Scheduler{}
	s.beginJob("mirror", func() {})
	for i := 0; i < 250; i++ {
		s.jobLine("line")
	}
	v := s.ServerJob()
	if v == nil {
		t.Fatal("expected active job")
	}
	if len(v.Tail) != 200 {
		t.Fatalf("expected tail capped at 200, got %d", len(v.Tail))
	}
}

// TestServerJobCopiesTail ensures ServerJob returns an independent copy of
// the tail slice so a concurrent jobLine append can't race the reader.
func TestServerJobCopiesTail(t *testing.T) {
	s := &Scheduler{}
	s.beginJob("mirror", func() {})
	s.jobLine("first")
	v := s.ServerJob()
	s.jobLine("second")
	if len(v.Tail) != 1 || v.Tail[0] != "first" {
		t.Fatalf("expected snapshot unaffected by later append, got %v", v.Tail)
	}
}
