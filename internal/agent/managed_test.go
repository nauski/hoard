package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/nauski/hoard/internal/config"
)

func managedAgent(t *testing.T, serverURL string) *Agent {
	t.Helper()
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte("repo-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Agent{
		cfgPath: filepath.Join(dir, "config.json"),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: Config{
			Host: "frame", PasswordFile: pw, ServerURL: serverURL,
			Repository: "rest:http://frame:login@nas:8000/frame/",
			Paths:      []string{"/original"}, Schedule: "21:00", Tags: []string{"keep"},
		},
	}
}

func push(key, host string, ver int, m config.ManagedSettings) (int, string, string) {
	raw, _ := json.Marshal(m)
	return ver, string(raw), config.SignManaged(key, host, ver, string(raw))
}

func TestApplyManaged(t *testing.T) {
	want := config.ManagedSettings{Paths: []string{"/home/me"}, Schedule: "2h", Nice: 19, ExcludeCaches: true}

	// Off by default: even a correctly signed push is refused.
	a := managedAgent(t, "")
	if err := a.applyManaged(push("repo-secret", "frame", 1, want)); err == nil {
		t.Fatal("push applied although remote management is off")
	}
	if a.cfg.Paths[0] != "/original" {
		t.Fatal("config changed although remote management is off")
	}

	a.cfg.RemoteManagement = true
	// Wrong key (someone who does not hold the repository password).
	if err := a.applyManaged(push("guess", "frame", 1, want)); err == nil {
		t.Fatal("push signed with the wrong key was applied")
	}
	// Signed for another host.
	if err := a.applyManaged(push("repo-secret", "vm", 1, want)); err == nil {
		t.Fatal("push signed for another host was applied")
	}
	// Settings altered after signing.
	ver, _, mac := push("repo-secret", "frame", 1, want)
	if err := a.applyManaged(ver, `{"paths":["/etc"]}`, mac); err == nil {
		t.Fatal("tampered push was applied")
	}
	// Invalid settings, correctly signed.
	if err := a.applyManaged(push("repo-secret", "frame", 1, config.ManagedSettings{Schedule: "hourly"})); err == nil {
		t.Fatal("invalid schedule was applied")
	}
	if a.cfg.Paths[0] != "/original" || a.cfg.ManagedVersion != 0 {
		t.Fatalf("a rejected push changed the config: %+v", a.cfg)
	}

	// The real thing.
	if err := a.applyManaged(push("repo-secret", "frame", 1, want)); err != nil {
		t.Fatalf("valid push rejected: %v", err)
	}
	c := a.GetConfig()
	if c.Paths[0] != "/home/me" || c.Schedule != "2h" || c.Nice != 19 || !c.ExcludeCaches || c.ManagedVersion != 1 {
		t.Fatalf("settings not applied: %+v", c)
	}
	// What the server may never touch is unchanged.
	if c.Repository != "rest:http://frame:login@nas:8000/frame/" || c.Host != "frame" || c.Tags[0] != "keep" || c.PasswordFile == "" {
		t.Fatalf("a push changed where backups go: %+v", c)
	}
	// Persisted.
	raw, err := os.ReadFile(a.cfgPath)
	var onDisk Config
	if err != nil || json.Unmarshal(raw, &onDisk) != nil || onDisk.Schedule != "2h" {
		t.Fatalf("not persisted: %v", err)
	}

	// Replaying the old push after a newer one does nothing.
	newer := config.ManagedSettings{Paths: []string{"/home/me", "/srv"}, Schedule: "1h"}
	if err := a.applyManaged(push("repo-secret", "frame", 2, newer)); err != nil {
		t.Fatal(err)
	}
	_ = a.applyManaged(push("repo-secret", "frame", 1, want))
	if got := a.GetConfig(); got.Schedule != "1h" || got.ManagedVersion != 2 {
		t.Fatalf("an old push was replayed: %+v", got)
	}
}

// TestCheckInAppliesPush runs the whole exchange against a fake server.
func TestCheckInAppliesPush(t *testing.T) {
	var got struct {
		Host           string          `json:"host"`
		CheckIn        bool            `json:"checkin"`
		Managed        bool            `json:"managed"`
		AppliedVersion int             `json:"applied_version"`
		Settings       json.RawMessage `json:"settings"`
		AgentVersion   string          `json:"agent_version"`
	}
	var user, pass string
	want := config.ManagedSettings{Paths: []string{"/home/me"}, Schedule: "2h"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ = r.BasicAuth()
		_ = json.NewDecoder(r.Body).Decode(&got)
		ver, js, mac := push("repo-secret", "frame", 5, want)
		_ = json.NewEncoder(w).Encode(map[string]any{"command": "", "config": map[string]any{"version": ver, "settings_json": js, "mac": mac}})
	}))
	defer srv.Close()

	a := managedAgent(t, srv.URL)
	a.cfg.RemoteManagement = true
	a.CheckIn(context.Background(), false)

	if !got.CheckIn || got.Host != "frame" || !got.Managed || got.AgentVersion == "" || len(got.Settings) == 0 {
		t.Fatalf("check-in payload = %+v", got)
	}
	if user != "frame" || pass != "login" {
		t.Fatalf("check-in did not use the client's own login: %q %q", user, pass)
	}
	if c := a.GetConfig(); c.Schedule != "2h" || c.ManagedVersion != 5 {
		t.Fatalf("push from check-in not applied: %+v", c)
	}
}
