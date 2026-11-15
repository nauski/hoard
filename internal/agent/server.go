package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/nauski/hoard/internal/version"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"
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
	mux.HandleFunc("GET /api/browse", s.browseDir)
	mux.HandleFunc("POST /api/mkdir", s.mkdir)
	mux.HandleFunc("POST /api/backup", s.backup)
	mux.HandleFunc("POST /api/restore", s.restore)
	mux.HandleFunc("GET /api/find", s.find)
	mux.HandleFunc("POST /api/restore-path", s.restorePath)
	mux.HandleFunc("POST /api/backup/pause", s.control("pause"))
	mux.HandleFunc("POST /api/backup/resume", s.control("resume"))
	mux.HandleFunc("POST /api/backup/cancel", s.control("cancel"))
	// Backup browser: ls/download read the hot repo directly; deletes are
	// delegated to the server (only it can reach e2).
	mux.HandleFunc("GET /api/ls", s.ls)
	mux.HandleFunc("GET /api/download", s.download)
	mux.HandleFunc("POST /api/purge", s.purge)
	mux.HandleFunc("POST /api/delete-version", s.deleteVersion)
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
	// Decode onto the current config, so settings the GUI does not render
	// (exclude files, priority caps, ...) survive a save instead of being wiped.
	// The JSON round trip gives a deep copy: decoding straight onto GetConfig's
	// result would write into slices the agent is still using.
	var c Config
	cur, _ := json.Marshal(s.agent.GetConfig())
	_ = json.Unmarshal(cur, &c)
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.agent.SetConfig(c); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.agent.GetConfig())
}

type statusResponse struct {
	Version string     `json:"version"`
	Config  Config     `json:"config"`
	Running bool       `json:"running"`
	LastRun RunResult  `json:"last_run"`
	Live    Live       `json:"live"`
	Now     time.Time  `json:"now"`
	NextRun *time.Time `json:"next_run,omitempty"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	last, running := s.agent.LastRun()
	now := time.Now()
	resp := statusResponse{
		Version: version.Version,
		Config:  s.agent.GetConfig(),
		Running: running,
		LastRun: last,
		Live:    s.agent.Live(),
		Now:     now,
	}
	if next, err := s.agent.NextRun(now); err == nil && !next.IsZero() {
		if next.Before(now) {
			next = now // overdue: the scheduler fires within a minute
		}
		resp.NextRun = &next
	}
	writeJSON(w, http.StatusOK, resp)
}

// control applies pause/resume/cancel to the running backup.
func (s *Server) control(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var err error
		switch action {
		case "pause":
			err = s.agent.Pause()
		case "resume":
			err = s.agent.Resume()
		case "cancel":
			err = s.agent.Cancel()
		}
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": action})
	}
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

func (s *Server) browseDir(w http.ResponseWriter, r *http.Request) {
	res, err := browse(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// mkdir creates a new folder inside the given parent path (the folder picker's
// current directory) and returns its absolute path so the GUI can navigate into
// it. Used by the picker's "New folder" button.
func (s *Server) mkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	abs, err := makeDir(req.Path, req.Name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": abs})
}

func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	_, running := s.agent.LastRun()
	if running {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a backup is already running"})
		return
	}
	// Optional body {"pin":"note"} pins the snapshot (see Agent.BackupPin).
	var req struct {
		Pin string `json:"pin"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	go func() {
		if err := s.agent.BackupPin(context.Background(), req.Pin); err != nil {
			s.log.Error("manual backup failed", "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Path   string `json:"path"`
		Target string `json:"target"`
		Mode   string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing snapshot id"})
		return
	}
	if req.Mode != "inplace" && req.Target == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing target"})
		return
	}
	_, running := s.agent.LastRun()
	if running {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a backup or restore is already running"})
		return
	}
	go func() {
		if err := s.agent.Restore(context.Background(), req.ID, req.Path, req.Target, req.Mode); err != nil {
			s.log.Error("restore failed", "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// find searches this host's snapshots for a file-name pattern.
func (s *Server) find(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing q"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	found, err := s.agent.Find(ctx, q)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, found)
}

// restorePath restores one path from the newest snapshot that has it, beside
// the original (never in place).
func (s *Server) restorePath(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing path"})
		return
	}
	if _, running := s.agent.LastRun(); running {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a backup or restore is already running"})
		return
	}
	go func() {
		if _, err := s.agent.RestorePath(context.Background(), req.Path, time.Time{}, "", false); err != nil {
			s.log.Error("restore failed", "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) ls(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing snapshot id"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	entries, err := s.agent.Ls(ctx, id, r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	p := r.URL.Query().Get("path")
	if id == "" || p == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id or path"})
		return
	}
	name := p[strings.LastIndex(p, "/")+1:]
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	if err := s.agent.Dump(r.Context(), id, p, w); err != nil {
		s.log.Error("download failed", "err", err, "path", p)
	}
}

// purge delegates a delete to the server, which owns both repos. The agent
// forces host to its own hostname so a client can only purge its own data.
func (s *Server) purge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing path"})
		return
	}
	body := map[string]string{"host": s.agent.Host(), "path": req.Path}
	if req.Version != "" {
		body["version"] = req.Version
	}
	s.delegate(w, r, "/api/purge", body)
}

func (s *Server) deleteVersion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing snapshot id"})
		return
	}
	s.delegate(w, r, "/api/delete-version", map[string]string{"id": req.ID})
}

// delegate forwards a destructive request to the hoard server's API.
func (s *Server) delegate(w http.ResponseWriter, r *http.Request, path string, body map[string]string) {
	base := s.agent.ServerBaseURL()
	if base == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "no server URL configured (set server_url)"})
		return
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, base+path, bytes.NewReader(raw))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "server unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
