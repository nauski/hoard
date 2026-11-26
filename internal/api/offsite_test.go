package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

func offsiteServer(t *testing.T, c *config.Config) (*Server, *config.Store, *state.Store) {
	t.Helper()
	if c.Hot.Repository == "" {
		c.Hot = config.Repo{Repository: "/h"}
	}
	cfg := config.NewStore(c, "")
	st, _ := state.Load("")
	return New(cfg, "restic", scheduler.New(cfg, "restic", st, testLogger()), st, testLogger(), nil), cfg, st
}

func post(t *testing.T, h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return w
}

const twoStorages = `{"offsite":[
 {"label":"Hetzner Box","default":true,"repository":"sftp:box:/r","password":"pw1"},
 {"label":"IDrive e2","repository":"s3:https://e2/b","password":"pw2","s3_access_key_id":"AK","s3_secret_access_key":"SECRETKEY"}]}`

func TestConfigOffsiteRoundTrip(t *testing.T) {
	srv, _, _ := offsiteServer(t, &config.Config{})
	if w := post(t, srv.handleSetConfig, "/api/config", twoStorages); w.Code != 200 {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	w := httptest.NewRecorder()
	srv.handleGetConfig(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var got struct {
		Offsite []struct {
			ID, Label   string
			Default     bool
			PasswordSet bool `json:"password_set"`
			SecretSet   bool `json:"secret_set"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Offsite) != 2 || got.Offsite[0].ID != "hetzner-box" || got.Offsite[1].ID != "idrive-e2" {
		t.Fatalf("ids: %+v", got.Offsite)
	}
	if !got.Offsite[0].PasswordSet || !got.Offsite[1].SecretSet || got.Offsite[0].SecretSet {
		t.Fatalf("flags: %+v", got.Offsite)
	}
	for _, leak := range []string{"pw1", "pw2", "SECRETKEY", `"cold"`, "exclude_hosts"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatalf("response contains %q: %s", leak, w.Body)
		}
	}
}

func TestConfigKeepsSecretsWhenBlank(t *testing.T) {
	srv, cfg, _ := offsiteServer(t, &config.Config{})
	post(t, srv.handleSetConfig, "/api/config", twoStorages)
	body := `{"offsite":[{"id":"hetzner-box","label":"Hetzner Box","repository":"sftp:box:/r2","password":""},
	 {"id":"idrive-e2","label":"IDrive e2","repository":"s3:https://e2/b","password":"","s3_secret_access_key":""}]}`
	if w := post(t, srv.handleSetConfig, "/api/config", body); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	o := cfg.Load().Offsite
	if o[0].Password != "pw1" || o[1].Password != "pw2" || o[1].S3SecretAccessKey != "SECRETKEY" || o[0].Repository != "sftp:box:/r2" {
		t.Fatalf("secrets lost: %+v", o)
	}
}

func TestConfigRemoveStorageCleansRouting(t *testing.T) {
	srv, cfg, _ := offsiteServer(t, &config.Config{
		Offsite: []config.Offsite{{ID: "a", Label: "A", Repo: config.Repo{Repository: "r:a"}}, {ID: "b", Label: "B", Repo: config.Repo{Repository: "r:b"}}},
		Routing: map[string][]string{"h": {"a", "b"}},
	})
	body := `{"offsite":[{"id":"a","label":"A","repository":"r:a"}]}`
	if w := post(t, srv.handleSetConfig, "/api/config", body); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := cfg.Load().Routing["h"]; !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("routing: %v", got)
	}
}

func TestConfigRejectsBadOffsite(t *testing.T) {
	srv, cfg, _ := offsiteServer(t, &config.Config{})
	for name, body := range map[string]string{
		"duplicate label": `{"offsite":[{"label":"X","repository":"r:1"},{"label":"x","repository":"r:2"}]}`,
		"empty label":     `{"offsite":[{"label":" ","repository":"r:1"}]}`,
		"empty repo":      `{"offsite":[{"label":"Y","repository":""}]}`,
	} {
		w := post(t, srv.handleSetConfig, "/api/config", body)
		if w.Code != 400 {
			t.Fatalf("%s: got %d", name, w.Code)
		}
	}
	if len(cfg.Load().Offsite) != 0 {
		t.Fatal("rejected post changed the list")
	}
}

func TestConfigRejectsDuplicateLabel(t *testing.T) {
	srv, _, _ := offsiteServer(t, &config.Config{})
	w := post(t, srv.handleSetConfig, "/api/config", `{"offsite":[{"label":"X","repository":"r:1"},{"label":"X","repository":"r:2"}]}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `\"X\"`) {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}

func TestConfigIgnoresLegacyCold(t *testing.T) {
	srv, cfg, _ := offsiteServer(t, &config.Config{})
	post(t, srv.handleSetConfig, "/api/config", `{"cold":{"repository":"s3:x"},"mirror":{"exclude_hosts":["h"]}}`)
	if c := cfg.Load(); c.Cold.Repository != "" || len(c.Mirror.ExcludeHosts) != 0 {
		t.Fatalf("legacy fields written: %+v %+v", c.Cold, c.Mirror)
	}
}

func TestClientOffsiteEndpoint(t *testing.T) {
	srv, cfg, _ := offsiteServer(t, &config.Config{
		Offsite: []config.Offsite{{ID: "a", Label: "A", Default: true, Repo: config.Repo{Repository: "r:a"}}, {ID: "b", Label: "B", Repo: config.Repo{Repository: "r:b"}}},
	})
	if w := post(t, srv.handleSetClientOffsite, "/api/clients/offsite", `{"host":"shared-host","storages":["b"]}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := cfg.Load().Routing["shared-host"]; !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("routing: %v", got)
	}
	if w := post(t, srv.handleSetClientOffsite, "/api/clients/offsite", `{"host":"shared-host","storages":["zzz"]}`); w.Code != 400 {
		t.Fatalf("unknown id: %d", w.Code)
	}
	if w := post(t, srv.handleSetClientOffsite, "/api/clients/offsite", `{"host":"shared-host","storages":null}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if _, ok := cfg.Load().Routing["shared-host"]; ok {
		t.Fatal("null should delete the routing entry")
	}
	post(t, srv.handleSetClientOffsite, "/api/clients/offsite", `{"host":"h","storages":[]}`)
	if got, ok := cfg.Load().Routing["h"]; !ok || len(got) != 0 {
		t.Fatalf("empty list means hot only: %v %v", got, ok)
	}
}

func twoOffsiteConfig() *config.Config {
	return &config.Config{Offsite: []config.Offsite{
		{ID: "a", Label: "Alpha", Default: true, Repo: config.Repo{Repository: "r:a"}},
		{ID: "b", Label: "Beta", Repo: config.Repo{Repository: "r:b"}},
	}}
}

// report posts to /api/report; login adds host's own Basic credentials.
func report(t *testing.T, srv *Server, body string, login bool) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/report", strings.NewReader(body))
	if login {
		r.SetBasicAuth("h", "httppw")
	}
	srv.handleReport(w, r)
	return w
}

func ownRepoConfig() *config.Config {
	c := twoOffsiteConfig()
	c.Clients = map[string]config.Client{"h": {HTTPPassword: "httppw"}}
	return c
}

func TestReportWishIgnoredForSharedRepoHost(t *testing.T) {
	srv, _, st := offsiteServer(t, twoOffsiteConfig())
	if w := report(t, srv, `{"host":"shared","checkin":true,"offsite":["b"]}`, false); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := st.ClientInfoFor("shared").OffsiteWish; len(got) != 0 {
		t.Fatalf("unauthenticated wish stored: %v", got)
	}
}

func TestReportWishCleaned(t *testing.T) {
	srv, _, st := offsiteServer(t, ownRepoConfig())
	report(t, srv, `{"host":"h","checkin":true,"offsite":["b","zzz","b","a","a"]}`, true)
	if got := st.ClientInfoFor("h").OffsiteWish; !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Fatalf("wish: %v", got)
	}
}

func TestReportCarriesWishAndStorages(t *testing.T) {
	srv, _, st := offsiteServer(t, ownRepoConfig())
	w := report(t, srv, `{"host":"h","checkin":true,"offsite":["b"]}`, true)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := st.ClientInfoFor("h").OffsiteWish; !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("wish: %v", got)
	}
	var reply struct {
		Storages []struct{ ID, Label string } `json:"offsite_storages"`
		Server   []string                     `json:"offsite_server"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Storages) != 2 || reply.Storages[0].Label != "Alpha" || reply.Storages[1].Label != "Beta" {
		t.Fatalf("storages: %+v", reply.Storages)
	}
	if !reflect.DeepEqual(reply.Server, []string{"a"}) {
		t.Fatalf("server: %v", reply.Server)
	}
	// field absent: wish untouched; explicit empty list: cleared
	report(t, srv, `{"host":"h","checkin":true}`, true)
	if got := st.ClientInfoFor("h").OffsiteWish; !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("absent field changed wish: %v", got)
	}
	report(t, srv, `{"host":"h","checkin":true,"offsite":[]}`, true)
	if got := st.ClientInfoFor("h").OffsiteWish; len(got) != 0 {
		t.Fatalf("empty list should clear: %v", got)
	}
}

func TestStatusListsStoragesPerClient(t *testing.T) {
	c := twoOffsiteConfig()
	c.Routing = map[string][]string{"routed": {"b"}}
	srv, _, st := offsiteServer(t, c)
	st.SetClients(map[string]state.Client{"routed": {Hostname: "routed"}, "plain": {Hostname: "plain"}})
	st.SetClientInfo("plain", state.ClientInfo{OffsiteWish: []string{"b"}})
	w := httptest.NewRecorder()
	srv.handleStatus(w, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var got struct {
		Offsite []struct{ ID, Label, Repository string }
		Clients map[string]struct {
			Offsite       []string  `json:"offsite"`
			OffsiteServer *[]string `json:"offsite_server"`
			OffsiteWish   []string  `json:"offsite_wish"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Offsite) != 2 || got.Offsite[1].Label != "Beta" {
		t.Fatalf("offsite: %+v", got.Offsite)
	}
	r, p := got.Clients["routed"], got.Clients["plain"]
	if !reflect.DeepEqual(r.Offsite, []string{"b"}) || r.OffsiteServer == nil || !reflect.DeepEqual(*r.OffsiteServer, []string{"b"}) {
		t.Fatalf("routed: %+v", r)
	}
	if !reflect.DeepEqual(p.Offsite, []string{"a", "b"}) || p.OffsiteServer != nil || !reflect.DeepEqual(p.OffsiteWish, []string{"b"}) {
		t.Fatalf("plain: %+v", p)
	}
}

func TestOffsiteEndpointsUnknownID(t *testing.T) {
	srv, _, _ := offsiteServer(t, twoOffsiteConfig())
	for _, h := range []http.HandlerFunc{srv.handleTestOffsite, srv.handleInitOffsite} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, "/x?id=nope", nil))
		if w.Code != 404 {
			t.Fatalf("got %d", w.Code)
		}
	}
}

func TestConfigRejectsRepeatedID(t *testing.T) {
	srv, cfg, _ := offsiteServer(t, twoOffsiteConfig())
	body := `{"offsite":[{"id":"a","label":"Alpha","repository":"r:a"},{"id":"a","label":"Gamma","repository":"r:g"}]}`
	if w := post(t, srv.handleSetConfig, "/api/config", body); w.Code != 400 {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if len(cfg.Load().Offsite) != 2 {
		t.Fatal("rejected post changed the list")
	}
}

func TestConfigNeverIssuesHotID(t *testing.T) {
	srv, cfg, _ := offsiteServer(t, &config.Config{})
	post(t, srv.handleSetConfig, "/api/config", `{"offsite":[{"label":"Hot","repository":"r:1"}]}`)
	if id := cfg.Load().Offsite[0].ID; id == "hot" {
		t.Fatal("storage got the reserved id hot")
	}
}

func TestConfigOffsiteResetsRecoveryKitAck(t *testing.T) {
	c := twoOffsiteConfig()
	c.RecoveryKitAck = true
	srv, cfg, _ := offsiteServer(t, c)
	same := `{"offsite":[{"id":"a","label":"Alpha","default":true,"repository":"r:a"},{"id":"b","label":"Beta","repository":"r:b"}]}`
	post(t, srv.handleSetConfig, "/api/config", same)
	if !cfg.Load().RecoveryKitAck {
		t.Fatal("unchanged storages must keep the ack")
	}
	relabel := `{"offsite":[{"id":"a","label":"Renamed","default":true,"repository":"r:a"},{"id":"b","label":"Beta","repository":"r:b"}]}`
	post(t, srv.handleSetConfig, "/api/config", relabel)
	if !cfg.Load().RecoveryKitAck {
		t.Fatal("a label change must keep the ack")
	}
	for name, body := range map[string]string{
		"password": `{"offsite":[{"id":"a","label":"Alpha","repository":"r:a","password":"new"},{"id":"b","label":"Beta","repository":"r:b"}]}`,
		"repo":     `{"offsite":[{"id":"a","label":"Alpha","repository":"r:a2"},{"id":"b","label":"Beta","repository":"r:b"}]}`,
		"added":    `{"offsite":[{"id":"a","label":"Alpha","repository":"r:a"},{"id":"b","label":"Beta","repository":"r:b"},{"label":"New","repository":"r:n"}]}`,
	} {
		cfg.Update(func(c *config.Config) { c.RecoveryKitAck = true })
		if w := post(t, srv.handleSetConfig, "/api/config", body); w.Code != 200 {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
		if cfg.Load().RecoveryKitAck {
			t.Errorf("%s: ack should be reset", name)
		}
		// restore the original list for the next case
		post(t, srv.handleSetConfig, "/api/config", `{"offsite":[{"id":"a","label":"Alpha","repository":"r:a"},{"id":"b","label":"Beta","repository":"r:b"}]}`)
	}
}

func TestConfigRetiredIDsNotReused(t *testing.T) {
	srv, cfg, _ := offsiteServer(t, &config.Config{})
	post(t, srv.handleSetConfig, "/api/config", `{"offsite":[{"label":"Box","repository":"r:1"}]}`)
	post(t, srv.handleSetConfig, "/api/config", `{"offsite":[]}`)
	if got := cfg.Load().Offsite; got == nil || len(got) != 0 {
		t.Fatalf("empty list must be non-nil empty: %#v", got)
	}
	post(t, srv.handleSetConfig, "/api/config", `{"offsite":[{"label":"Box","repository":"r:2"}]}`)
	if id := cfg.Load().Offsite[0].ID; id != "box-2" {
		t.Fatalf("id reused: %s", id)
	}
}

func TestConfigWithoutOffsiteKeepsList(t *testing.T) {
	srv, cfg, _ := offsiteServer(t, twoOffsiteConfig())
	if w := post(t, srv.handleSetConfig, "/api/config", `{"retention":{"keep_daily":3}}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := cfg.Load().Offsite; len(got) != 2 || got[0].ID != "a" || got[1].Repository != "r:b" {
		t.Fatalf("list changed: %+v", got)
	}
}

func TestStatusHotOnlyReflectsEffectiveSet(t *testing.T) {
	c := twoOffsiteConfig()
	c.Routing = map[string][]string{"lab": {}, "wisher": {}}
	srv, _, st := offsiteServer(t, c)
	st.SetClients(map[string]state.Client{"lab": {Hostname: "lab"}, "wisher": {Hostname: "wisher"}, "plain": {Hostname: "plain"}})
	st.SetClientInfo("wisher", state.ClientInfo{OffsiteWish: []string{"b"}})
	w := httptest.NewRecorder()
	srv.handleStatus(w, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var got struct {
		Clients map[string]struct {
			HotOnly bool `json:"hot_only"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Clients["lab"].HotOnly || got.Clients["wisher"].HotOnly || got.Clients["plain"].HotOnly {
		t.Fatalf("hot_only: %+v", got.Clients)
	}
}

func TestRetentionPreviewSurvivesUnreachableStorage(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "restic")
	script := `#!/bin/sh
[ "$RESTIC_REPOSITORY" = r:a ] && { echo "no route to host" >&2; exit 1; }
case " $* " in
*" snapshots "*) echo '[{"id":"x","short_id":"x","hostname":"h1","time":"2026-01-01T00:00:00Z"}]' ;;
*" forget "*) echo '[{"remove":[{"id":"s1","short_id":"s1","hostname":"h1","time":"2026-01-01T00:00:00Z"}]}]' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewStore(twoOffsiteConfig(), "")
	cfg.Update(func(c *config.Config) { c.Hot = config.Repo{Repository: "/h"} })
	st, _ := state.Load("")
	srv := New(cfg, bin, scheduler.New(cfg, bin, st, testLogger()), st, testLogger(), nil)
	w := post(t, srv.handleRetentionPreview, "/api/config/retention-preview", `{"keep_daily":1}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var got struct {
		Offsite []struct {
			ID, Error string
			Count     int
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Offsite) != 2 || !strings.Contains(got.Offsite[0].Error, "no route to host") || got.Offsite[1].Error != "" || got.Offsite[1].Count != 1 {
		t.Fatalf("offsite: %+v", got.Offsite)
	}
}

func TestStatusMapsLegacyJobsToFirstStorage(t *testing.T) {
	srv, _, st := offsiteServer(t, twoOffsiteConfig())
	st.RecordJob(state.JobResult{Job: "mirror", OK: true, Message: "old mirror"})
	st.RecordJob(state.JobResult{Job: "check", OK: true, Message: "old check"})
	st.RecordJob(state.JobResult{Job: "check", Target: "b", OK: true, Message: "new check"})
	w := httptest.NewRecorder()
	srv.handleStatus(w, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var got struct {
		Jobs map[string]state.JobResult
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Jobs["mirror@a"].Message != "old mirror" {
		t.Errorf("legacy mirror not shown for first storage: %+v", got.Jobs)
	}
	if _, ok := got.Jobs["check@a"]; ok {
		t.Errorf("check already has per-storage entries, legacy must not be mapped: %+v", got.Jobs)
	}
	if got.Jobs["mirror"].Message != "old mirror" {
		t.Error("history key must stay")
	}
}
