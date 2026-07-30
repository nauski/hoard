package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

func TestColdLimitsRoundTrip(t *testing.T) {
	cfg := config.NewStore(&config.Config{Hot: config.Repo{Repository: "/h"}, Cold: config.Repo{Repository: "s3:x"}}, "")
	st, _ := state.Load("")
	srv := New(cfg, "restic", scheduler.New(cfg, "restic", st, testLogger()), st, testLogger(), nil)

	body := `{"cold":{"repository":"s3:x","s3_access_key_id":"","s3_secret_access_key":"","password":"","limit_upload_kibps":1500,"limit_download_kibps":800}}`
	w := httptest.NewRecorder()
	srv.handleSetConfig(w, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}
	if cfg.Load().Cold.LimitUploadKiBps != 1500 || cfg.Load().Cold.LimitDownloadKiBps != 800 {
		t.Fatalf("limits not stored: %+v", cfg.Load().Cold)
	}
	w = httptest.NewRecorder()
	srv.handleGetConfig(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if !bytes.Contains(w.Body.Bytes(), []byte(`"limit_upload_kibps":1500`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"limit_download_kibps":800`)) {
		t.Fatalf("limits not in config view: %s", w.Body.String())
	}
}

func TestHandleRetentionPreview_BadBody(t *testing.T) {
	cfg := config.NewStore(&config.Config{Hot: config.Repo{Repository: "/h"}, Cold: config.Repo{Repository: "s3:x"}}, "")
	st, _ := state.Load("")
	srv := New(cfg, "restic", scheduler.New(cfg, "restic", st, testLogger()), st, testLogger(), nil)

	w := httptest.NewRecorder()
	srv.handleRetentionPreview(w, httptest.NewRequest(http.MethodPost, "/api/config/retention-preview", strings.NewReader("not json")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad body, got %d: %s", w.Code, w.Body.String())
	}
}
