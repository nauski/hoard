package scheduler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/state"
)

// fakeRestic writes a restic stand-in that logs "<RESTIC_REPOSITORY> <args>"
// per call. The hot repo reports hosts h1 and h2; `copy` fails when the
// destination is $FAIL_REPO. Returns a scheduler over storages a (default) and
// b, and a func returning the logged lines.
func fakeRestic(t *testing.T, offsite ...string) (*Scheduler, func() []string) {
	t.Helper()
	dir := t.TempDir()
	logf := filepath.Join(dir, "log")
	script := `#!/bin/sh
echo "$RESTIC_REPOSITORY $*" >> ` + logf + `
case " $* " in
*" snapshots "*)
  if [ "$RESTIC_REPOSITORY" = /hot ]; then
    echo '[{"id":"a1","short_id":"a1","hostname":"h1","time":"2026-01-01T00:00:00Z"},{"id":"b1","short_id":"b1","hostname":"h2","time":"2026-01-01T00:00:00Z"}]'
  else echo '[]'; fi ;;
*" stats "*) echo '{"total_size":5}' ;;
*" copy "*) [ "$RESTIC_REPOSITORY" = "$FAIL_REPO" ] && { echo boom >&2; exit 1; } ;;
esac
exit 0
`
	bin := filepath.Join(dir, "restic")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &config.Config{Hot: config.Repo{Repository: "/hot", Password: "x"}}
	for i, id := range offsite {
		c.Offsite = append(c.Offsite, config.Offsite{ID: id, Label: strings.ToUpper(id) + " storage", Default: i == 0,
			Repo: config.Repo{Repository: "/" + id, Password: "x"}})
	}
	st, _ := state.Load("")
	s := New(config.NewStore(c, ""), bin, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s, func() []string {
		b, _ := os.ReadFile(logf)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

// copyLine returns the logged copy command into repo.
func copyLine(lines []string, repo string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, repo+" ") && strings.Contains(l, " copy ") {
			return l
		}
	}
	return ""
}

func TestMirrorRoutesHostsPerStorage(t *testing.T) {
	s, lines := fakeRestic(t, "a", "b")
	s.cfg.Update(func(c *config.Config) { c.Routing = map[string][]string{"h2": {"b"}} })
	s.Mirror(context.Background())

	a, b := copyLine(lines(), "/a"), copyLine(lines(), "/b")
	if !strings.Contains(a, "--host h1") || strings.Contains(a, "h2") {
		t.Errorf("copy into a = %q, want h1 only", a)
	}
	if !strings.Contains(b, "--host h2") || strings.Contains(b, "h1") {
		t.Errorf("copy into b = %q, want h2 only", b)
	}
	last := s.store.Snapshot().LastByJob
	for _, k := range []string{"mirror@a", "mirror@b"} {
		if !last[k].OK {
			t.Errorf("%s = %+v, want OK", k, last[k])
		}
	}
	for _, k := range []string{"prune@a", "prune@b"} {
		if _, ok := last[k]; !ok {
			t.Errorf("%s not recorded", k)
		}
	}
}

func TestMirrorContinuesAfterOneStorageFails(t *testing.T) {
	t.Setenv("FAIL_REPO", "/a")
	s, _ := fakeRestic(t, "a", "b")
	note := &recordingNotifier{}
	s.SetNotifier(note)
	s.Mirror(context.Background())

	last := s.store.Snapshot().LastByJob
	if last["mirror@a"].OK || !last["mirror@b"].OK {
		t.Fatalf("mirror@a = %+v, mirror@b = %+v", last["mirror@a"], last["mirror@b"])
	}
	if note.count() != 1 || !strings.Contains(note.calls[0], "A storage") {
		t.Fatalf("alerts = %v, want exactly one naming A storage", note.calls)
	}
}

func TestMirrorClientWishAdds(t *testing.T) {
	s, lines := fakeRestic(t, "a", "b")
	s.cfg.Update(func(c *config.Config) { c.Routing = map[string][]string{"h1": {}} })
	s.store.SetClientInfo("h1", state.ClientInfo{OffsiteWish: []string{"b"}})
	s.Mirror(context.Background())

	if l := copyLine(lines(), "/a"); strings.Contains(l, "h1") {
		t.Errorf("h1 copied to a: %q", l)
	}
	if l := copyLine(lines(), "/b"); !strings.Contains(l, "--host h1") {
		t.Errorf("h1 not copied to b: %q", l)
	}
}

func TestCheckRunsEveryStorage(t *testing.T) {
	s, _ := fakeRestic(t, "a", "b")
	s.Check(context.Background())
	last := s.store.Snapshot().LastByJob
	if !last["check@a"].OK || !last["check@b"].OK {
		t.Fatalf("check@a = %+v, check@b = %+v", last["check@a"], last["check@b"])
	}
}

func TestPurgeReachesEveryStorage(t *testing.T) {
	s, lines := fakeRestic(t, "a", "b")
	if err := s.PurgePath(context.Background(), "h1", "/secret"); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"/hot", "/a", "/b"} {
		if !slices.ContainsFunc(lines(), func(l string) bool {
			return strings.HasPrefix(l, repo+" ") && strings.Contains(l, " rewrite ")
		}) {
			t.Errorf("no rewrite against %s", repo)
		}
	}
}

func TestNoStorageMirrorIsOK(t *testing.T) {
	s, _ := fakeRestic(t)
	note := &recordingNotifier{}
	s.SetNotifier(note)
	s.Mirror(context.Background())
	m := s.store.Snapshot().LastByJob["mirror"]
	if !m.OK || m.Message != "no offsite storage configured" || note.count() != 0 {
		t.Fatalf("mirror = %+v, alerts %v", m, note.calls)
	}
}
