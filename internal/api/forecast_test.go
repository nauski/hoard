package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

func TestHandleForecast(t *testing.T) {
	cfg := config.NewStore(&config.Config{Hot: config.Repo{Repository: "/h"}, Cold: config.Repo{Repository: "s3:x"}}, "")
	st, _ := state.Load("")
	base := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 3; i++ {
		st.AppendSizeSample(state.SizeSample{
			At:        base.Add(time.Duration(i) * 24 * time.Hour),
			HotStored: int64(i+1) * (1 << 30),
		})
	}
	srv := New(cfg, "restic", scheduler.New(cfg, "restic", st, testLogger()), st, testLogger(), nil)

	w := httptest.NewRecorder()
	srv.handleForecast(w, httptest.NewRequest(http.MethodGet, "/api/forecast", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var p struct {
		HaveData  bool  `json:"have_data"`
		HotPerDay int64 `json:"hot_per_day"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v, body=%s", err, w.Body.String())
	}
	if !p.HaveData {
		t.Fatal("want have_data=true for a 3-day linear series")
	}
	if p.HotPerDay <= 0 {
		t.Fatalf("want hot_per_day > 0, got %d", p.HotPerDay)
	}
}

func TestHandleForecast_EmptyStore(t *testing.T) {
	cfg := config.NewStore(&config.Config{Hot: config.Repo{Repository: "/h"}, Cold: config.Repo{Repository: "s3:x"}}, "")
	st, _ := state.Load("")
	srv := New(cfg, "restic", scheduler.New(cfg, "restic", st, testLogger()), st, testLogger(), nil)

	w := httptest.NewRecorder()
	srv.handleForecast(w, httptest.NewRequest(http.MethodGet, "/api/forecast", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var p struct {
		HaveData bool `json:"have_data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v, body=%s", err, w.Body.String())
	}
	if p.HaveData {
		t.Fatal("want have_data=false for an empty store")
	}
}
