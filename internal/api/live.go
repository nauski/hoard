package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/nauski/hoard/internal/state"
)

// liveStore holds the most recent live-backup report from each client, plus a
// pending control command to hand back on the client's next report. Clients
// push reports (they can't be reached directly — they bind localhost), so
// control travels back as a command the agent picks up and applies.
type liveStore struct {
	mu      sync.Mutex
	clients map[string]*liveClient
}

type liveClient struct {
	Host     string          `json:"host"`
	Live     json.RawMessage `json:"live"` // opaque {running,paused,progress,activity,...}
	LastSeen time.Time       `json:"last_seen"`
	Command  string          `json:"-"` // pending control command for this host
}

func newLiveStore() *liveStore { return &liveStore{clients: map[string]*liveClient{}} }

// report records a client's live state and returns any pending command (which
// it then clears).
func (s *liveStore) report(host string, live json.RawMessage, now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.clients[host]
	if c == nil {
		c = &liveClient{Host: host}
		s.clients[host] = c
	}
	c.Live = live
	c.LastSeen = now
	cmd := c.Command
	c.Command = ""
	return cmd
}

// setCommand queues a control command for a host.
func (s *liveStore) setCommand(host, cmd string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.clients[host]
	if c == nil {
		c = &liveClient{Host: host}
		s.clients[host] = c
	}
	c.Command = cmd
}

// running returns clients seen within the freshness window (stale reports are
// dropped so a crashed agent doesn't linger as "running").
func (s *liveStore) running(now time.Time, fresh time.Duration) []liveClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []liveClient{}
	for _, c := range s.clients {
		if now.Sub(c.LastSeen) <= fresh {
			out = append(out, liveClient{Host: c.Host, Live: c.Live, LastSeen: c.LastSeen})
		}
	}
	return out
}

// --- handlers ---

type reportOutcome struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

type reportRequest struct {
	Host       string          `json:"host"`
	Live       json.RawMessage `json:"live"`
	LastResult *reportOutcome  `json:"last_result"`
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	var req reportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing host"})
		return
	}
	cmd := s.live.report(req.Host, req.Live, time.Now())
	if req.LastResult != nil {
		s.recordOutcome(req.Host, req.LastResult.OK, req.LastResult.Message)
	}
	writeJSON(w, http.StatusOK, map[string]string{"command": cmd})
}

// recordOutcome maintains the per-client consecutive-failure count and fires the
// failure alert once when it first crosses the configured threshold.
func (s *Server) recordOutcome(host string, ok bool, message string) {
	prev := s.store.OutcomeFor(host)
	count := prev.ConsecutiveFailures
	if ok {
		count = 0
	} else {
		count++
	}
	s.store.SetOutcome(host, state.Outcome{OK: ok, Message: message, ConsecutiveFailures: count, At: time.Now()})

	al := s.cfg.Load().Alert
	threshold := al.FailureThreshold
	if threshold <= 0 {
		threshold = 3
	}
	if !ok && al.OnFailure && count >= threshold && prev.ConsecutiveFailures < threshold {
		title := "client backup failing"
		body := fmt.Sprintf("%s: %s (failed %d times)", host, message, count)
		go s.Notify(context.Background(), title, body)
	}
}

// handleRunning lists clients currently reporting a live backup.
func (s *Server) handleRunning(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.live.running(time.Now(), 15*time.Second))
}

// handleClientControl queues a pause/resume/cancel for a client, delivered on
// its next report.
func (s *Server) handleClientControl(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host   string `json:"host"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Host == "" || req.Action == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing host or action"})
		return
	}
	switch req.Action {
	case "pause", "resume", "cancel":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad action"})
		return
	}
	if req.Host == serverRestoreHost && req.Action == "cancel" {
		s.restoreMu.Lock()
		c := s.restoreCancel
		s.restoreMu.Unlock()
		if c != nil {
			c()
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "action": "cancel"})
		return
	}
	s.live.setCommand(req.Host, req.Action)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "action": req.Action})
}
