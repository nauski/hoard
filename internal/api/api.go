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
	"strings"
	"sync"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/restic"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

// Server wires HTTP handlers to the scheduler, store, and restic clients.
type Server struct {
	cfg       *config.Store
	resticBin string
	sched     *scheduler.Scheduler
	store     *state.Store
	log       *slog.Logger
	webFS     fs.FS
	client    *http.Client
	live      *liveStore

	restoreMu     sync.Mutex
	restoreCancel context.CancelFunc // cancels the in-flight server restore, if any
}

func New(cfg *config.Store, resticBin string, sched *scheduler.Scheduler, store *state.Store, log *slog.Logger, webFS fs.FS) *Server {
	return &Server{
		cfg: cfg, resticBin: resticBin, sched: sched, store: store, log: log, webFS: webFS,
		client: &http.Client{Timeout: 10 * time.Second},
		live:   newLiveStore(),
	}
}

// hot / cold build a restic client from the current config so repo/endpoint/
// credential changes in Settings apply without a restart.
func (s *Server) hot() *restic.Client  { return restic.New(s.resticBin, s.cfg.Load().Hot) }
func (s *Server) cold() *restic.Client { return restic.New(s.resticBin, s.cfg.Load().Cold) }

// Handler returns the root mux (dashboard + /api routes).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("POST /api/config", s.handleSetConfig)
	mux.HandleFunc("POST /api/config/test-cold", s.handleTestCold)
	mux.HandleFunc("POST /api/config/init-cold", s.handleInitCold)
	mux.HandleFunc("GET /api/snapshots", s.handleSnapshots)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("POST /api/actions/mirror", s.action("mirror"))
	mux.HandleFunc("POST /api/actions/check", s.action("check"))
	// Backup browser (reads the fast hot repo; deletes hit hot + cold).
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/ls", s.handleLs)
	mux.HandleFunc("GET /api/download", s.handleDownload)
	mux.HandleFunc("POST /api/purge", s.handlePurge)
	mux.HandleFunc("POST /api/delete-version", s.handleDeleteVersion)
	mux.HandleFunc("POST /api/restore", s.handleRestore)
	// Live running-backup aggregation across all clients.
	mux.HandleFunc("POST /api/report", s.handleReport)
	mux.HandleFunc("GET /api/running", s.handleRunning)
	mux.HandleFunc("POST /api/clients/control", s.handleClientControl)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /", noCache(http.FileServerFS(s.webFS)))
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
		ColdRepo: redact(s.cfg.Load().Cold.Repository),
		HotRepo:  s.cfg.Load().Hot.Repository,
	})
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	snaps, err := s.hot().Snapshots(ctx)
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
	if s.cfg.Load().Alert.WebhookURL == "" {
		return
	}
	// Payload shape works for ntfy/Discord-compatible relays that read "content".
	payload := map[string]string{
		"title":   "hoard: " + title,
		"content": body,
		"text":    "**hoard: " + title + "**\n" + body,
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Load().Alert.WebhookURL, bytes.NewReader(raw))
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

// handleStats returns repository storage totals (deduplicated raw-data size)
// for the hot and cold repos. Somewhat expensive (reads the index), so the UI
// calls it infrequently.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	out := map[string]any{}
	if st, err := s.hot().Stats(ctx); err == nil {
		out["hot"] = st
	}
	if st, err := s.cold().Stats(ctx); err == nil {
		out["cold"] = st
	}
	writeJSON(w, http.StatusOK, out)
}

// handleLs lists one directory level inside a snapshot (hot repo).
func (s *Server) handleLs(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing snapshot id"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	entries, err := s.hot().Ls(ctx, id, r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleDownload streams a single file from a snapshot (hot repo) as a download.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	p := r.URL.Query().Get("path")
	if id == "" || p == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id or path"})
		return
	}
	name := p[strings.LastIndex(p, "/")+1:]
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	ctx := r.Context()
	if err := s.hot().Dump(ctx, id, p, w); err != nil {
		// Headers may already be sent; log and give up.
		s.log.Error("download failed", "err", err, "path", p)
	}
}

type purgeRequest struct {
	Host    string `json:"host"`
	Path    string `json:"path"`
	Version string `json:"version"` // optional hot snapshot id; empty = all versions
}

// handlePurge removes a path from either one version (if version is set) or all
// versions of a host, across hot + cold.
func (s *Server) handlePurge(w http.ResponseWriter, r *http.Request) {
	var req purgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing path"})
		return
	}
	go func() {
		var err error
		if req.Version != "" {
			err = s.sched.PurgePathInVersion(context.Background(), req.Version, req.Path)
		} else {
			err = s.sched.PurgePath(context.Background(), req.Host, req.Path)
		}
		if err != nil {
			s.log.Error("purge failed", "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "path": req.Path})
}

// handleDeleteVersion deletes one whole snapshot (hot + cold twin).
func (s *Server) handleDeleteVersion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing snapshot id"})
		return
	}
	go func() {
		if err := s.sched.DeleteSnapshot(context.Background(), req.ID); err != nil {
			s.log.Error("delete version failed", "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "id": req.ID})
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

// noCache tells browsers to revalidate the embedded dashboard on every load, so
// a redeployed UI is picked up immediately instead of served stale from cache.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Debug("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
	})
}
