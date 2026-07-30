package config

import (
	"os"
	"path/filepath"
	"testing"
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
