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
