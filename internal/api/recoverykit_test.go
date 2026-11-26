package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

func testKitConfig() *config.Config {
	return &config.Config{
		Hot: config.Repo{Repository: "rest:http://nas:8000/hot", Password: "hotpw"},
		Offsite: []config.Offsite{
			{ID: "e2", Label: "IDrive e2", Default: true, Repo: config.Repo{
				Repository:        "s3:https://s3.example.com/mybucket",
				Password:          "coldpw",
				S3AccessKeyID:     "AKIAEXAMPLE",
				S3SecretAccessKey: "supersecret",
			}},
			{ID: "box", Label: "Hetzner Box", Repo: config.Repo{Repository: "sftp:box:/repo", Password: "boxpw"}},
		},
	}
}

func TestBuildRecoveryKitContainsColdAndHot(t *testing.T) {
	out := buildRecoveryKit(testKitConfig(), "0.18.0", time.Unix(0, 0).UTC())
	for _, want := range []string{
		"s3:https://s3.example.com/mybucket", "coldpw", "AKIAEXAMPLE", "supersecret",
		"restic restore latest", "IDrive e2", "Hetzner Box", "boxpw",
		"restic -r 's3:https://s3.example.com/mybucket' snapshots", "restic -r 'sftp:box:/repo' snapshots", "rest:http://nas:8000/hot", "hotpw", "0.18.0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("kit missing %q\n---\n%s", want, out)
		}
	}
}

func TestBuildRecoveryKitOnlyS3BlockHasKeys(t *testing.T) {
	out := buildRecoveryKit(testKitConfig(), "0.18.0", time.Unix(0, 0).UTC())
	if n := strings.Count(out, "AWS_ACCESS_KEY_ID"); n != 1 {
		t.Fatalf("want S3 keys only for the S3 storage, got %d blocks\n%s", n, out)
	}
}

func TestBuildRecoveryKitUnconfiguredCold(t *testing.T) {
	c := testKitConfig()
	c.Offsite = nil // nothing configured
	out := buildRecoveryKit(c, "0.18.0", time.Unix(0, 0).UTC())
	if strings.Contains(out, "supersecret") {
		t.Fatal("kit leaked a secret for an unconfigured repo")
	}
	if !strings.Contains(out, "not configured") {
		t.Fatalf("expected 'not configured' marker for empty cold repo\n---\n%s", out)
	}
}

func TestBuildRecoveryKitPartialCold(t *testing.T) {
	c := testKitConfig()
	c.Offsite = []config.Offsite{{ID: "x", Label: "X", Repo: config.Repo{Password: "coldpw"}}} // repository is empty, password is set
	out := buildRecoveryKit(c, "0.18.0", time.Unix(0, 0).UTC())
	if strings.Contains(out, "export RESTIC_REPOSITORY=''") {
		t.Fatal("kit has misleading blank export for unconfigured repository")
	}
	if !strings.Contains(out, "not configured") {
		t.Fatalf("expected 'not configured' marker for partial cold repo\n---\n%s", out)
	}
}

func TestBuildRecoveryKitHasSecurityWarning(t *testing.T) {
	out := buildRecoveryKit(testKitConfig(), "0.18.0", time.Unix(0, 0).UTC())
	if !strings.Contains(out, "SENSITIVE") {
		t.Fatalf("expected 'SENSITIVE' warning in kit\n---\n%s", out)
	}
}

// newKitServer builds a Server around a config with cold+hot creds. No restic
// repo is needed — the kit/ack endpoints never shell out — so this never skips.
func newKitServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.NewStore(testKitConfig(), "")
	st, _ := state.Load("")
	sched := scheduler.New(cfg, "restic", st, testLogger())
	return New(cfg, "restic", sched, st, testLogger(), nil)
}

func TestAckKitSetsFlag(t *testing.T) {
	srv := newKitServer(t)
	// Initially false in the config view.
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	w := httptest.NewRecorder()
	srv.handleGetConfig(w, req)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"recovery_kit_ack":false`)) {
		t.Fatalf("expected recovery_kit_ack:false initially: %s", w.Body.String())
	}
	// Ack it.
	w = httptest.NewRecorder()
	srv.handleAckKit(w, httptest.NewRequest(http.MethodPost, "/api/config/ack-kit", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("ack: %d %s", w.Code, w.Body.String())
	}
	// Now true.
	w = httptest.NewRecorder()
	srv.handleGetConfig(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if !bytes.Contains(w.Body.Bytes(), []byte(`"recovery_kit_ack":true`)) {
		t.Fatalf("expected recovery_kit_ack:true after ack: %s", w.Body.String())
	}
}

func TestHandleRecoveryKit(t *testing.T) {
	srv := newKitServer(t)
	w := httptest.NewRecorder()
	srv.handleRecoveryKit(w, httptest.NewRequest(http.MethodGet, "/api/recovery-kit", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") ||
		!strings.Contains(cd, "hoard-recovery-kit.txt") {
		t.Fatalf("bad Content-Disposition: %q", cd)
	}
	if !strings.Contains(w.Body.String(), "s3:https://s3.example.com/mybucket") {
		t.Fatalf("kit body missing configured repo:\n%s", w.Body.String())
	}
}

func TestBuildRecoveryKitListsHostsPerStorage(t *testing.T) {
	c := testKitConfig() // e2 is the default, box is not
	c.Clients = map[string]config.Client{"alpha": {}, "beta": {}, "gamma": {}}
	c.Routing = map[string][]string{"beta": {"box"}, "gamma": {}, "shared": {"box", "e2"}}
	out := buildRecoveryKit(c, "0.18.0", time.Unix(0, 0).UTC())
	for _, want := range []string{
		"Hosts copied to IDrive e2: alpha, shared (and every other host that follows the defaults)",
		"Hosts copied to Hetzner Box: beta, shared\n",
		"so they can only be restored from the local repo below: gamma\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("kit missing %q\n---\n%s", want, out)
		}
	}
}

func TestBuildRecoveryKitNoDefaultStorage(t *testing.T) {
	c := testKitConfig()
	c.Offsite[0].Default = false
	c.Clients = map[string]config.Client{"alpha": {}}
	out := buildRecoveryKit(c, "0.18.0", time.Unix(0, 0).UTC())
	if !strings.Contains(out, "restored from the local repo below: alpha") || !strings.Contains(out, "No storage is a default") {
		t.Fatalf("host following defaults with none set must be listed as not offsite\n---\n%s", out)
	}
}
