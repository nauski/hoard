package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/nauski/hoard/internal/restic"
)

const serverRestoreHost = "server (restore)"

// handleRestore restores from the hot repo to a NAS path, async, with progress
// streamed to the "Running backups" panel via the liveStore.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID        string `json:"id"`
		Path      string `json:"path"`
		Target    string `json:"target"`
		Overwrite string `json:"overwrite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing snapshot id"})
		return
	}
	if s.sched.Running() != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a job is already running: " + s.sched.Running()})
		return
	}
	if req.Target == "" {
		req.Target = fmt.Sprintf("/data/restore/%s-%d/", short(req.ID), time.Now().Unix())
	}
	if req.Overwrite == "" {
		req.Overwrite = "always"
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.restoreMu.Lock()
	s.restoreCancel = cancel
	s.restoreMu.Unlock()

	go func() {
		defer cancel()
		started := time.Now()
		var activity []string
		push := func(running bool, p *restic.RestoreProgress) {
			live := map[string]any{"running": running, "kind": "restore", "started_at": started}
			if p != nil {
				live["progress"] = map[string]any{
					"percent_done": p.PercentDone, "files_done": p.FilesRestored, "total_files": p.TotalFiles,
					"bytes_done": p.BytesRestored, "total_bytes": p.TotalBytes, "seconds_elapsed": p.SecondsElapsed,
				}
			}
			if len(activity) > 0 {
				live["activity"] = activity
			}
			raw, _ := json.Marshal(live)
			s.live.report(serverRestoreHost, raw, time.Now())
		}
		hooks := restic.RestoreHooks{
			OnProgress: func(p restic.RestoreProgress) { push(true, &p) },
			OnActivity: func(action, item string) {
				activity = append(activity, action+"  "+item)
				if len(activity) > 300 {
					activity = activity[len(activity)-300:]
				}
			},
		}
		push(true, nil)
		if _, err := s.sched.Restore(ctx, req.ID, req.Path, req.Target, req.Overwrite, hooks); err != nil {
			s.log.Error("restore failed", "err", err)
		}
		s.live.report(serverRestoreHost, mustJSON(map[string]any{"running": false, "kind": "restore"}), time.Now())
		s.restoreMu.Lock()
		s.restoreCancel = nil
		s.restoreMu.Unlock()
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "target": req.Target})
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// short returns the first 8 chars of an id for display/paths.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
