package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

func postReport(t *testing.T, srv *Server, host string, ok bool, msg string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"host": host, "live": map[string]any{"running": false},
		"last_result": map[string]any{"ok": ok, "message": msg},
	})
	w := httptest.NewRecorder()
	srv.handleReport(w, httptest.NewRequest(http.MethodPost, "/api/report", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("report %d: %s", w.Code, w.Body.String())
	}
}

func TestFailureAlertFiresOnceAtThreshold(t *testing.T) {
	var hits int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer hook.Close()

	cfg := config.NewStore(&config.Config{
		Hot:  config.Repo{Repository: "/h"},
		Cold: config.Repo{Repository: "s3:x"},
		Alert: config.Alert{WebhookURL: hook.URL, OnFailure: true, FailureThreshold: 3},
	}, "")
	st, _ := state.Load("")
	srv := New(cfg, "restic", scheduler.New(cfg, "restic", st, testLogger()), st, testLogger(), nil)

	waitHits := func(want int32) {
		for i := 0; i < 50; i++ {
			if atomic.LoadInt32(&hits) == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("expected %d webhook hits, got %d", want, atomic.LoadInt32(&hits))
	}

	postReport(t, srv, "nas", false, "boom") // count 1
	postReport(t, srv, "nas", false, "boom") // count 2
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("alert fired before threshold")
	}
	postReport(t, srv, "nas", false, "boom") // count 3 == threshold → fire once
	waitHits(1)
	postReport(t, srv, "nas", false, "boom") // count 4, already crossed → no re-fire
	time.Sleep(120 * time.Millisecond)
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("alert re-fired past threshold: %d", atomic.LoadInt32(&hits))
	}
	if o := st.OutcomeFor("nas"); o.ConsecutiveFailures != 4 {
		t.Fatalf("count should be 4, got %d", o.ConsecutiveFailures)
	}
	postReport(t, srv, "nas", true, "ok") // success resets
	if o := st.OutcomeFor("nas"); o.ConsecutiveFailures != 0 || !o.OK {
		t.Fatalf("success should reset: %+v", o)
	}
}

func TestStatusIncludesOutcomes(t *testing.T) {
	cfg := config.NewStore(&config.Config{Hot: config.Repo{Repository: "/h"}, Cold: config.Repo{Repository: "s3:x"}}, "")
	st, _ := state.Load("")
	st.SetOutcome("nas", state.Outcome{OK: false, Message: "boom", ConsecutiveFailures: 2})
	srv := New(cfg, "restic", scheduler.New(cfg, "restic", st, testLogger()), st, testLogger(), nil)
	w := httptest.NewRecorder()
	srv.handleStatus(w, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if !bytes.Contains(w.Body.Bytes(), []byte(`"outcomes"`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"boom"`)) {
		t.Fatalf("status missing outcomes: %s", w.Body.String())
	}
}
