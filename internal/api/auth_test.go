package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/restic"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

// authServer builds a Server whose data lives under a temp dir shaped like the
// container's /data (hot repo at <dir>/hot).
func authServer(t *testing.T, mut func(*config.Config)) (*Server, *config.Store, string) {
	t.Helper()
	dir := t.TempDir()
	c := &config.Config{Hot: config.Repo{Repository: filepath.Join(dir, "hot"), Password: "shared-pw"}}
	if mut != nil {
		mut(c)
	}
	cfg := config.NewStore(c, "")
	st, _ := state.Load("")
	sched := scheduler.New(cfg, "restic", st, testLogger())
	return New(cfg, "restic", sched, st, testLogger(), os.DirFS(t.TempDir())), cfg, dir
}

func do(t *testing.T, s *Server, method, path, body string, mut func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if mut != nil {
		mut(req)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestDashboardPassword(t *testing.T) {
	open, _, _ := authServer(t, nil)
	if rec := do(t, open, "GET", "/api/config", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("no password configured: /api/config = %d, want 200", rec.Code)
	}

	s, _, _ := authServer(t, func(c *config.Config) { c.DashboardPassword = "s3cret" })
	admin := func(r *http.Request) { r.SetBasicAuth("anyone", "s3cret") }

	for _, path := range []string{"/api/config", "/api/status", "/api/recovery-kit", "/api/history", "/"} {
		if rec := do(t, s, "GET", path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without password = %d, want 401", path, rec.Code)
		} else if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("GET %s: no WWW-Authenticate header, the browser would not ask", path)
		}
	}
	if rec := do(t, s, "GET", "/api/config", "", func(r *http.Request) { r.SetBasicAuth("x", "wrong") }); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password = %d, want 401", rec.Code)
	}
	if rec := do(t, s, "GET", "/api/config", "", admin); rec.Code != http.StatusOK {
		t.Errorf("right password = %d, want 200", rec.Code)
	}
	if rec := do(t, s, "GET", "/api/recovery-kit", "", admin); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "shared-pw") {
		t.Errorf("recovery kit with password = %d", rec.Code)
	}
	// The password never appears in the config view.
	if rec := do(t, s, "GET", "/api/config", "", admin); strings.Contains(rec.Body.String(), "s3cret") {
		t.Error("config view leaks the dashboard password")
	}

	// Client-facing routes stay reachable without the dashboard password.
	if rec := do(t, s, "GET", "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/report", `{"host":"legacy","live":{"running":false}}`, nil); rec.Code != http.StatusOK {
		t.Errorf("report from a shared-repo host = %d, want 200", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/enroll/redeem", `{"token":"nope"}`, nil); rec.Code != http.StatusForbidden {
		t.Errorf("redeem with a bad token = %d, want 403 (reached the handler)", rec.Code)
	}
	// Destructive routes agents used to call are admin-only now.
	if rec := do(t, s, "POST", "/api/delete-version", `{"id":"abc"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("delete without password = %d, want 401", rec.Code)
	}

	// Another site cannot ride on the browser's saved credentials.
	cross := func(r *http.Request) { admin(r); r.Header.Set("Sec-Fetch-Site", "cross-site") }
	if rec := do(t, s, "POST", "/api/anomaly/dismiss", `{"host":"x"}`, cross); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403", rec.Code)
	}
	origin := func(r *http.Request) { admin(r); r.Header.Set("Origin", "http://evil.example") }
	if rec := do(t, s, "POST", "/api/anomaly/dismiss", `{"host":"x"}`, origin); rec.Code != http.StatusForbidden {
		t.Errorf("foreign Origin POST = %d, want 403", rec.Code)
	}
	same := func(r *http.Request) { admin(r); r.Header.Set("Sec-Fetch-Site", "same-origin") }
	if rec := do(t, s, "POST", "/api/anomaly/dismiss", `{"host":"x"}`, same); rec.Code != http.StatusOK {
		t.Errorf("same-origin POST = %d, want 200", rec.Code)
	}

	// Setting the password through the API.
	s2, cfg2, _ := authServer(t, nil)
	if rec := do(t, s2, "POST", "/api/config", `{"dashboard_password":"new-pw"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("set password = %d", rec.Code)
	}
	if cfg2.Load().DashboardPassword != "new-pw" {
		t.Fatal("password not stored")
	}
	if rec := do(t, s2, "GET", "/api/config", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("after setting a password, open access = %d, want 401", rec.Code)
	}
}

func TestPrivateEnrollment(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}
	s, cfg, dir := authServer(t, func(c *config.Config) { c.PrivateRepos = true })
	// A shared repo exists, as on an upgraded server.
	if err := restic.New("restic", cfg.Load().Hot).EnsureInit(context.Background()); err != nil {
		t.Fatal(err)
	}
	enroll := func(body string) (int, map[string]string) {
		tok, _, err := s.tokens.mint()
		if err != nil {
			t.Fatal(err)
		}
		rec := do(t, s, "POST", "/api/enroll/redeem", strings.Replace(body, "TOKEN", tok, 1), func(r *http.Request) { r.Host = "nas.lan:8080" })
		var out map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// An old agent sends no host name.
	if code, out := enroll(`{"token":"TOKEN"}`); code != http.StatusBadRequest || !strings.Contains(out["error"], "0.29") {
		t.Fatalf("enroll without host = %d %v", code, out)
	}
	if code, _ := enroll(`{"token":"TOKEN","host":"../etc"}`); code != http.StatusBadRequest {
		t.Fatalf("enroll with a path as host = %d, want 400", code)
	}

	code, vm := enroll(`{"token":"TOKEN","host":"vm"}`)
	if code != http.StatusOK {
		t.Fatalf("enroll vm = %d %v", code, vm)
	}
	cl := cfg.Load().Clients["vm"]
	if cl.Password == "" || cl.HTTPPassword == "" || cl.Password == cl.HTTPPassword || cl.Password == "shared-pw" {
		t.Fatalf("client credentials wrong: %+v", cl)
	}
	if want := "rest:http://vm:" + cl.HTTPPassword + "@nas.lan:8000/vm/"; vm["repository"] != want || vm["password"] != cl.Password {
		t.Fatalf("enroll response = %v, want repository %s", vm, want)
	}
	// Its repository exists on disk and opens with its own password only.
	repo := cfg.Load().ClientRepo("vm")
	if repo.Repository != filepath.Join(dir, "repos", "vm") {
		t.Fatalf("repo path = %s", repo.Repository)
	}
	if _, err := restic.New("restic", repo).Snapshots(context.Background()); err != nil {
		t.Fatalf("client repo does not open: %v", err)
	}
	if _, err := restic.New("restic", config.Repo{Repository: repo.Repository, Password: "shared-pw"}).Snapshots(context.Background()); err == nil {
		t.Fatal("client repo opens with the shared password")
	}

	_, laptop := enroll(`{"token":"TOKEN","host":"laptop"}`)
	if laptop["password"] == vm["password"] {
		t.Fatal("two clients got the same repository password")
	}
	login, err := os.ReadFile(filepath.Join(dir, "htpasswd"))
	if err != nil || !strings.Contains(string(login), "vm:{SHA}") || !strings.Contains(string(login), "laptop:{SHA}") {
		t.Fatalf("login file = %q, %v", login, err)
	}
	if strings.Contains(string(login), cl.HTTPPassword) {
		t.Fatal("login file holds a plain password")
	}

	// Enrolling the same machine again returns the same credentials.
	if _, again := enroll(`{"token":"TOKEN","host":"vm"}`); again["password"] != vm["password"] || again["repository"] != vm["repository"] {
		t.Fatal("re-enrolling changed the credentials")
	}

	// Reports for a client with its own login need that login.
	body := `{"host":"vm","live":{"running":false}}`
	if rec := do(t, s, "POST", "/api/report", body, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("report without login = %d, want 401", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/report", body, func(r *http.Request) { r.SetBasicAuth("laptop", cfg.Load().Clients["laptop"].HTTPPassword) }); rec.Code != http.StatusUnauthorized {
		t.Errorf("report for vm with laptop's login = %d, want 401", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/report", body, func(r *http.Request) { r.SetBasicAuth("vm", cl.HTTPPassword) }); rec.Code != http.StatusOK {
		t.Errorf("report with own login = %d, want 200", rec.Code)
	}
}

// TestStatusListsEnrolledAndBusyClients: a machine that enrolled but has no
// finished backup, and one that is uploading right now, must both be visible.
func TestStatusListsEnrolledAndBusyClients(t *testing.T) {
	s, _, _ := authServer(t, func(c *config.Config) {
		c.Clients = map[string]config.Client{
			"vm":    {Password: "a", HTTPPassword: "vm-login"},
			"frame": {Password: "b", HTTPPassword: "frame-login"},
		}
	})
	if rec := do(t, s, "POST", "/api/report", `{"host":"vm","live":{"running":true}}`, func(r *http.Request) { r.SetBasicAuth("vm", "vm-login") }); rec.Code != http.StatusOK {
		t.Fatalf("report = %d", rec.Code)
	}
	rec := do(t, s, "GET", "/api/status", "", nil)
	var st struct {
		Clients map[string]struct {
			Busy    bool `json:"busy"`
			OwnRepo bool `json:"own_repo"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if c, ok := st.Clients["vm"]; !ok || !c.Busy || !c.OwnRepo {
		t.Fatalf("vm = %+v (present %v), want listed, own repo, busy", c, ok)
	}
	if c, ok := st.Clients["frame"]; !ok || c.Busy {
		t.Fatalf("frame = %+v (present %v), want listed and idle", c, ok)
	}
}

// TestManagedSettingsPush: settings saved on the dashboard reach a client that
// opted in, signed with its repository password, and nobody else.
func TestManagedSettingsPush(t *testing.T) {
	s, cfg, _ := authServer(t, func(c *config.Config) {
		c.DashboardPassword = "dash"
		c.Clients = map[string]config.Client{
			"frame": {Password: "frame-repo-pw", HTTPPassword: "frame-login"},
			"vm":    {Password: "vm-repo-pw", HTTPPassword: "vm-login"},
		}
	})
	admin := func(r *http.Request) { r.SetBasicAuth("x", "dash") }
	frame := func(r *http.Request) { r.SetBasicAuth("frame", "frame-login") }
	save := `{"host":"frame","settings":{"paths":["/home/me"],"schedule":"2h","nice":19}}`

	if rec := do(t, s, "POST", "/api/clients/config", save, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("saving settings without the dashboard password = %d, want 401", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/clients/config", save, frame); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a client saving settings with its own login = %d, want 401", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/clients/config", `{"host":"frame","settings":{"paths":["relative"]}}`, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("relative path accepted: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/clients/config", `{"host":"frame","settings":{"schedule":"hourly"}}`, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad schedule accepted: %d", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/clients/config", save, admin); rec.Code != http.StatusOK {
		t.Fatalf("save = %d %s", rec.Code, rec.Body)
	}
	if cfg.Load().Clients["frame"].ManagedVersion != 1 || cfg.Load().Clients["vm"].Managed != nil {
		t.Fatalf("stored: %+v", cfg.Load().Clients)
	}

	checkin := func(host string, managed bool, applied int, auth func(*http.Request)) *managedPush {
		body := `{"host":"` + host + `","checkin":true,"managed":` + map[bool]string{true: "true", false: "false"}[managed] +
			`,"applied_version":` + string(rune('0'+applied)) + `,"live":{"running":false},"settings":{"paths":["/old"]},"agent_version":"0.32"}`
		rec := do(t, s, "POST", "/api/report", body, auth)
		if rec.Code != http.StatusOK {
			t.Fatalf("check-in %s = %d %s", host, rec.Code, rec.Body)
		}
		var out struct {
			Config *managedPush `json:"config"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Config
	}

	// A client that has not opted in gets nothing.
	if p := checkin("frame", false, 0, frame); p != nil {
		t.Fatal("settings pushed to a client with remote management off")
	}
	// Another client gets nothing either.
	if p := checkin("vm", true, 0, func(r *http.Request) { r.SetBasicAuth("vm", "vm-login") }); p != nil {
		t.Fatal("frame's settings pushed to vm")
	}
	p := checkin("frame", true, 0, frame)
	if p == nil || p.Version != 1 || !strings.Contains(p.SettingsJSON, `"/home/me"`) {
		t.Fatalf("push = %+v", p)
	}
	if !config.VerifyManaged("frame-repo-pw", "frame", p.Version, p.SettingsJSON, p.MAC) {
		t.Fatal("push is not signed with the client's repository password")
	}
	// Tampering, another client's key, another host or an older version all fail.
	if config.VerifyManaged("frame-repo-pw", "frame", p.Version, strings.Replace(p.SettingsJSON, "/home/me", "/etc", 1), p.MAC) ||
		config.VerifyManaged("vm-repo-pw", "frame", p.Version, p.SettingsJSON, p.MAC) ||
		config.VerifyManaged("frame-repo-pw", "vm", p.Version, p.SettingsJSON, p.MAC) ||
		config.VerifyManaged("frame-repo-pw", "frame", p.Version+1, p.SettingsJSON, p.MAC) {
		t.Fatal("signature accepted something it must not")
	}
	// Once applied, it is not sent again.
	if p := checkin("frame", true, 1, frame); p != nil {
		t.Fatal("already-applied settings pushed again")
	}
	// The dashboard sees the check-in.
	rec := do(t, s, "GET", "/api/clients/config?host=frame", "", admin)
	if !strings.Contains(rec.Body.String(), `"applied_version":1`) || !strings.Contains(rec.Body.String(), `"pending":false`) {
		t.Fatalf("client config view = %s", rec.Body)
	}
}
