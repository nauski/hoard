package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

func TestCancelIdleReturns409(t *testing.T) {
	cfg := config.NewStore(&config.Config{Hot: config.Repo{Repository: "/h"}, Cold: config.Repo{Repository: "s3:x"}}, "")
	st, _ := state.Load("")
	srv := New(cfg, "restic", scheduler.New(cfg, "restic", st, testLogger()), st, testLogger(), nil)

	w := httptest.NewRecorder()
	srv.handleCancel(w, httptest.NewRequest(http.MethodPost, "/api/actions/cancel", nil))

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "no job running" {
		t.Fatalf("expected error %q, got %q", "no job running", body["error"])
	}
}

func TestStatusServerJobAbsentWhenIdle(t *testing.T) {
	cfg := config.NewStore(&config.Config{Hot: config.Repo{Repository: "/h"}, Cold: config.Repo{Repository: "s3:x"}}, "")
	st, _ := state.Load("")
	srv := New(cfg, "restic", scheduler.New(cfg, "restic", st, testLogger()), st, testLogger(), nil)

	w := httptest.NewRecorder()
	srv.handleStatus(w, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if bytes.Contains(w.Body.Bytes(), []byte(`"server_job"`)) {
		t.Fatalf("expected server_job to be omitted when idle: %s", w.Body.String())
	}

	var status statusResponse
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.ServerJob != nil {
		t.Fatalf("expected ServerJob nil, got %+v", status.ServerJob)
	}
}
