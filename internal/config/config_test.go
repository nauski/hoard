package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAlertFailureDefaults(t *testing.T) {
	// A config that omits on_failure/failure_threshold gets the defaults.
	p := writeCfg(t, `{"hot":{"repository":"/h"},"cold":{"repository":"s3:x"},"alert":{"on_stale":true}}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Alert.OnFailure {
		t.Fatal("OnFailure should default to true")
	}
	if c.Alert.FailureThreshold != 3 {
		t.Fatalf("FailureThreshold should default to 3, got %d", c.Alert.FailureThreshold)
	}
	// Explicit false is honored.
	p2 := writeCfg(t, `{"hot":{"repository":"/h"},"cold":{"repository":"s3:x"},"alert":{"on_failure":false,"failure_threshold":5}}`)
	c2, _ := Load(p2)
	if c2.Alert.OnFailure || c2.Alert.FailureThreshold != 5 {
		t.Fatalf("explicit alert values not honored: %+v", c2.Alert)
	}
}

func TestRetentionForAndValidate(t *testing.T) {
	r := Retention{Daily: 7, Hosts: map[string]Retention{"vm": {Within: "48h", Daily: 14}}}
	if got := r.For("vm"); got.Daily != 14 || got.Within != "48h" {
		t.Fatalf("For(vm) = %+v", got)
	}
	if got := r.For("desktop"); got.Daily != 7 || got.Within != "" {
		t.Fatalf("For(desktop) = %+v", got)
	}
	for _, ok := range []string{"", "48h", "7d", "1y6m2d"} {
		if err := (Retention{Within: ok}).Validate(); err != nil {
			t.Errorf("Within %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"48", "2 days", "1w", "90s"} {
		if err := (Retention{Within: bad}).Validate(); err == nil {
			t.Errorf("Within %q accepted", bad)
		}
	}
	if err := (Retention{Hosts: map[string]Retention{"vm": {Within: "soon"}}}).Validate(); err == nil {
		t.Error("bad per-host keep_within accepted")
	}
}

func TestStaleForAndMirrorExcludes(t *testing.T) {
	s := Schedule{StaleAfter: Duration(26 * time.Hour), StaleAfterHosts: map[string]Duration{"vm": Duration(3 * time.Hour)}}
	if s.StaleFor("vm") != 3*time.Hour || s.StaleFor("desktop") != 26*time.Hour {
		t.Fatalf("StaleFor: vm=%v desktop=%v", s.StaleFor("vm"), s.StaleFor("desktop"))
	}
	m := Mirror{ExcludeHosts: []string{"vm"}}
	if !m.Excludes("vm") || m.Excludes("desktop") {
		t.Fatal("Mirror.Excludes wrong")
	}
}

func TestAnomalyTriggered(t *testing.T) {
	a := Anomaly{Files: 10000, Percent: 50}
	cases := []struct {
		before, after int
		want          bool
	}{
		{1_100_000, 1_052_000, true},  // 48k files gone: over the file threshold
		{1_100_000, 1_095_000, false}, // 5k gone: under both
		{2000, 900, true},             // small host, more than half gone
		{2000, 1500, false},
		{2000, 0, true}, // unmounted disk
		{1000, 50_000, false},
		{0, 0, false},
	}
	for _, c := range cases {
		if got := a.Triggered(c.before, c.after); got != c.want {
			t.Errorf("Triggered(%d,%d) = %v, want %v", c.before, c.after, got, c.want)
		}
	}
	// One worktree (~3k files) out of 1.1M: under the default, over a per-host 2000.
	perHost := Anomaly{Files: 10000, Percent: 50, Hosts: map[string]Anomaly{"vm": {Files: 2000, Percent: 50}}}
	if perHost.For("desktop").Triggered(1_100_000, 1_097_000) {
		t.Error("default threshold fired on 3k files")
	}
	if !perHost.For("vm").Triggered(1_100_000, 1_097_000) {
		t.Error("per-host threshold missed 3k files")
	}
	if (Anomaly{}).Triggered(1000, 0) {
		t.Error("zero-value Anomaly must be off")
	}
}

func TestManagedMissingFrom(t *testing.T) {
	m := ManagedSettings{Paths: []string{"/home/me", "/etc/", "/srv/data"}}
	if got := m.MissingFrom([]string{"/home/me", "/etc", "/srv/data", "/extra"}); len(got) != 0 {
		t.Fatalf("all covered, got missing %v", got)
	}
	// A snapshot of a parent directory covers the required path.
	if got := m.MissingFrom([]string{"/home", "/etc", "/srv"}); len(got) != 0 {
		t.Fatalf("parents cover, got missing %v", got)
	}
	got := m.MissingFrom([]string{"/home/me", "/srv/database"})
	if len(got) != 2 || got[0] != "/etc" || got[1] != "/srv/data" {
		t.Fatalf("missing = %v, want [/etc /srv/data]", got)
	}
}
