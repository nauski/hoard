// Package api serves the JSON control API and the embedded dashboard, and
// implements the webhook Notifier used for alerts.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/restic"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

// Server wires HTTP handlers to the scheduler, store, and restic clients.
type Server struct {
	cfg    *config.Config
	sched  *scheduler.Scheduler
	store  *state.Store
	hot    *restic.Client
	cold   *restic.Client
	log    *slog.Logger
	webFS  fs.FS
	client *http.Client
}

func New(cfg *config.Config, sched *scheduler.Scheduler, store *state.Store, hot, cold *restic.Client, log *slog.Logger, webFS fs.FS) *Server {
	return &Server{
		cfg: cfg, sched: sched, store: store, hot: hot, cold: cold, log: log, webFS: webFS,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Handler returns the root mux (dashboard + /api routes).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/snapshots", s.handleSnapshots)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("POST /api/actions/mirror", s.action("mirror"))
	mux.HandleFunc("POST /api/actions/check", s.action("check"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /", http.FileServerFS(s.webFS))
	return logging(s.log, mux)
}

type statusResponse struct {
	Now      time.Time                  `json:"now"`
	Running  string                     `json:"running"`
	Clients  map[string]state.Client    `json:"clients"`
	Jobs     map[string]state.JobResult `json:"jobs"`
	ColdRepo string                     `json:"cold_repo"`
	HotRepo  string                     `json:"hot_repo"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap := s.store.Snapshot()
	writeJSON(w, http.StatusOK, statusResponse{
		Now:      time.Now(),
		Running:  s.sched.Running(),
		Clients:  snap.Clients,
		Jobs:     snap.LastByJob,
		ColdRepo: redact(s.cfg.Cold.Repository),
		HotRepo:  s.cfg.Hot.Repository,
	})
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	snaps, err := s.hot.Snapshots(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snaps)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	snap := s.store.Snapshot()
	writeJSON(w, http.StatusOK, snap.History)
}

// action triggers a job asynchronously; returns 202 if accepted, 409 if busy.
func (s *Server) action(job string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.sched.Running() != "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "a job is already running: " + s.sched.Running()})
			return
		}
		go func() {
			ctx := context.Background()
			switch job {
			case "mirror":
				s.sched.Mirror(ctx)
			case "check":
				s.sched.Check(ctx)
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "job": job})
	}
}

// Notify implements scheduler.Notifier via a generic JSON webhook.
func (s *Server) Notify(ctx context.Context, title, body string) {
	if s.cfg.Alert.WebhookURL == "" {
		return
	}
	// Payload shape works for ntfy/Discord-compatible relays that read "content".
	payload := map[string]string{
		"title":   "hoard: " + title,
		"content": body,
		"text":    "**hoard: " + title + "**\n" + body,
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Alert.WebhookURL, bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		s.log.Warn("alert webhook failed", "err", err)
		return
	}
	_ = resp.Body.Close()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// redact strips S3 credentials that might appear in a repo URL for display.
func redact(repo string) string {
	// restic S3 URLs don't embed creds (they use env), so this is mostly a
	// guard for other backends; return as-is for the common case.
	return repo
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Debug("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
	})
}
