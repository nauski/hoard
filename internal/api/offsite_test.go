package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestReportCarriesWishAndStorages(t *testing.T) {
	srv, _, st := offsiteServer(t, twoOffsiteConfig())
	w := post(t, srv.handleReport, "/api/report", `{"host":"h","checkin":true,"offsite":["b"]}`)
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
	post(t, srv.handleReport, "/api/report", `{"host":"h","checkin":true}`)
	if got := st.ClientInfoFor("h").OffsiteWish; !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("absent field changed wish: %v", got)
	}
	post(t, srv.handleReport, "/api/report", `{"host":"h","checkin":true,"offsite":[]}`)
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
