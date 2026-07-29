package agent

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/nauski/hoard/internal/restic"
)

// Server exposes the agent's web GUI and JSON API.
type Server struct {
	agent *Agent
	log   *slog.Logger
	webFS fs.FS
}

func NewServer(a *Agent, log *slog.Logger, webFS fs.FS) *Server {
	return &Server{agent: a, log: log, webFS: webFS}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("POST /api/config", s.setConfig)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/snapshots", s.snapshots)
	mux.HandleFunc("POST /api/backup", s.backup)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /", http.FileServerFS(s.webFS))
	return mux
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.agent.GetConfig())
}

func (s *Server) setConfig(w http.ResponseWriter, r *http.Request) {
	var c Config
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.agent.SetConfig(c); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.agent.GetConfig())
}

type statusResponse struct {
	Config   Config           `json:"config"`
	Running  bool             `json:"running"`
	LastRun  RunResult        `json:"last_run"`
	Progress *restic.Progress `json:"progress,omitempty"`
	Now      time.Time        `json:"now"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	last, running := s.agent.LastRun()
	writeJSON(w, http.StatusOK, statusResponse{
		Config:   s.agent.GetConfig(),
		Running:  running,
		LastRun:  last,
		Progress: s.agent.Progress(),
		Now:      time.Now(),
	})
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	snaps, err := s.agent.Snapshots(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snaps)
}

func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	_, running := s.agent.LastRun()
	if running {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a backup is already running"})
		return
	}
	go func() {
		if err := s.agent.Backup(context.Background()); err != nil {
			s.log.Error("manual backup failed", "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
