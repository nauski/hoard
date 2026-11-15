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
	"strings"
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
	if c.Managed == nil || c.Managed.Paths[0] != "/home/me" || c.ManagedVersion != 1 {
		t.Fatalf("settings not stored: %+v", c)
	}
	// This machine's own settings are left alone; the server's are a floor.
	if len(c.Paths) != 1 || c.Paths[0] != "/original" || c.Schedule != "21:00" {
		t.Fatalf("local settings were overwritten: %+v", c)
	}
	eff, _ := a.Effective()
	if len(eff.Paths) != 2 || eff.Paths[0] != "/home/me" || eff.Paths[1] != "/original" || eff.Schedule != "2h" || eff.Nice != 19 || !eff.ExcludeCaches {
		t.Fatalf("effective settings wrong: %+v", eff)
	}
	// What the server may never touch is unchanged.
	if c.Repository != "rest:http://frame:login@nas:8000/frame/" || c.Host != "frame" || c.Tags[0] != "keep" || c.PasswordFile == "" {
		t.Fatalf("a push changed where backups go: %+v", c)
	}
	// Persisted.
	raw, err := os.ReadFile(a.cfgPath)
	var onDisk Config
	if err != nil || json.Unmarshal(raw, &onDisk) != nil || onDisk.Managed == nil || onDisk.Managed.Schedule != "2h" {
		t.Fatalf("not persisted: %v", err)
	}

	// Replaying the old push after a newer one does nothing.
	newer := config.ManagedSettings{Paths: []string{"/home/me", "/srv"}, Schedule: "1h"}
	if err := a.applyManaged(push("repo-secret", "frame", 2, newer)); err != nil {
		t.Fatal(err)
	}
	_ = a.applyManaged(push("repo-secret", "frame", 1, want))
	if got := a.GetConfig(); got.Managed.Schedule != "1h" || got.ManagedVersion != 2 {
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
	if c, _ := a.Effective(); c.Schedule != "2h" || c.ManagedVersion != 5 {
		t.Fatalf("push from check-in not applied: %+v", c)
	}
}

// TestEffectiveIsAFloor: the server dictates a minimum; this machine can add
// paths, but cannot remove a required path or exclude its way into one.
func TestEffectiveIsAFloor(t *testing.T) {
	server := &config.ManagedSettings{
		Paths:    []string{"/home/me", "/etc"},
		Excludes: []string{"node_modules"},
		Schedule: "1h", ExcludeLargerThan: "1G",
	}
	c := Config{
		RemoteManagement: true, Managed: server,
		// Local: drops /etc, adds /srv/media, repeats /home/me, adds a folder inside a required path.
		Paths: []string{"/home/me", "/srv/media", "/home/me/projects"},
		Excludes: []string{
			"/srv/media/cache",      // below a path only this machine adds: allowed
			"/home/me/secret",       // inside a required path: ignored
			"/home/me/projects/big", // the local path lies inside a required one: ignored
			"**/.git",               // would apply everywhere: ignored
			"/etc",                  // a required path itself: ignored
			"/srv/mediaXcache",      // not below /srv/media: ignored
		},
		Schedule: "", ExcludeLargerThan: "1K", ExcludeCaches: true,
		ExcludeFiles: []string{"/home/me/.ignore"},
	}
	eff, ignored := c.effective()

	wantPaths := []string{"/home/me", "/etc", "/srv/media", "/home/me/projects"}
	if strings.Join(eff.Paths, " ") != strings.Join(wantPaths, " ") {
		t.Fatalf("paths = %v, want %v (required first, local added, nothing removed)", eff.Paths, wantPaths)
	}
	if got := strings.Join(eff.Excludes, " "); got != "node_modules /srv/media/cache" {
		t.Fatalf("excludes = %q", got)
	}
	if len(ignored) != 5 {
		t.Fatalf("ignored = %v, want the 5 patterns that could reach a required path", ignored)
	}
	// Settings that apply to every path come from the server only.
	if eff.ExcludeLargerThan != "1G" || eff.ExcludeCaches || len(eff.ExcludeFiles) != 0 {
		t.Fatalf("global exclusions not taken from the server: %+v", eff)
	}
	// The schedule cannot be switched off locally.
	if eff.Schedule != "1h" {
		t.Fatalf("schedule = %q, want the server's", eff.Schedule)
	}
	// A local path that contains a required one gets no local excludes either.
	c2 := Config{RemoteManagement: true, Managed: &config.ManagedSettings{Paths: []string{"/home/me"}},
		Paths: []string{"/home"}, Excludes: []string{"/home/other/tmp", "/home/me"}}
	if eff2, ign := c2.effective(); len(eff2.Excludes) != 0 || len(ign) != 2 {
		t.Fatalf("excludes under a local parent of a required path were accepted: %v", eff2.Excludes)
	}

	// With management off, or nothing pushed yet, the config applies as written.
	c.RemoteManagement = false
	if eff, ignored := c.effective(); len(eff.Paths) != 3 || len(ignored) != 0 || eff.ExcludeLargerThan != "1K" {
		t.Fatalf("unmanaged config was altered: %+v", eff)
	}
}
