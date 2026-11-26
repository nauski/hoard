package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nauski/hoard/internal/config"
)

// reportBody does one report against a fake server answering reply and returns
// the raw request body it received.
func reportBody(t *testing.T, a *Agent, reply string) string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	live := a.Live()
	a.reportFull(srv.Client(), srv.URL, &live, nil, map[string]any{"checkin": true})
	return got
}

func TestOffsiteWishInReport(t *testing.T) {
	a := managedAgent(t, "")
	a.cfg.Offsite = []string{"b"}
	if body := reportBody(t, a, `{}`); !strings.Contains(body, `"offsite":["b"]`) {
		t.Fatalf("report %s lacks the wish", body)
	}
	a.cfg.Offsite = nil
	if body := reportBody(t, a, `{}`); !strings.Contains(body, `"offsite":[]`) {
		t.Fatalf("report %s must send [] to withdraw the wish", body)
	}
}

func TestOffsiteStatusFromReply(t *testing.T) {
	a := managedAgent(t, "")
	status := func() (stores []OffsiteStorage, server []string, raw string) {
		rec := httptest.NewRecorder()
		NewServer(a, a.log, nil).status(rec, httptest.NewRequest("GET", "/api/status", nil))
		var r struct {
			S []OffsiteStorage `json:"offsite_storages"`
			V []string         `json:"offsite_server"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r.S, r.V, rec.Body.String()
	}
	if _, _, raw := status(); !strings.Contains(raw, `"offsite_storages":null`) {
		t.Fatalf("before any reply the list must be null: %s", raw)
	}

	reportBody(t, a, `{"offsite_storages":[{"id":"a","label":"Box A"},{"id":"b","label":"Box B"}],"offsite_server":["a"]}`)
	s, v, _ := status()
	if len(s) != 2 || s[1].Label != "Box B" || len(v) != 1 || v[0] != "a" {
		t.Fatalf("status = %+v %+v", s, v)
	}

	// A reply without the fields keeps the last known lists.
	reportBody(t, a, `{}`)
	if s, _, _ := status(); len(s) != 2 {
		t.Fatalf("lists lost on a reply without them: %+v", s)
	}

	// The lists survive a restart (state.json) ...
	b, err := Load(a.cfgPath, "restic", "", a.log)
	if err != nil {
		t.Fatal(err)
	}
	if s, v := b.Offsite(); len(s) != 2 || len(v) != 1 {
		t.Fatalf("after reload: %+v %+v", s, v)
	}
	// ... and a server with no storage at all is told apart from "not reached".
	reportBody(t, a, `{"offsite_storages":null,"offsite_server":null}`)
	if s, v := a.Offsite(); s == nil || v == nil {
		t.Fatalf("null lists on a check-in must become empty, got %+v %+v", s, v)
	}
}

func TestManagedPushKeepsOffsiteWish(t *testing.T) {
	a := managedAgent(t, "")
	a.cfg.RemoteManagement = true
	a.cfg.Offsite = []string{"b"}
	if err := a.applyManaged(push("repo-secret", "frame", 1, config.ManagedSettings{Paths: []string{"/home/me"}})); err != nil {
		t.Fatal(err)
	}
	if got := a.cfg.Offsite; len(got) != 1 || got[0] != "b" {
		t.Fatalf("Offsite = %v after a managed push", got)
	}
}
