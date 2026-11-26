package api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nauski/hoard/internal/config"
	"maps"
	"net/http"
	"slices"
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
	// Sent by agents 0.32+ in their periodic check-in.
	CheckIn         bool            `json:"checkin"`
	Managed         bool            `json:"managed"`
	AppliedVersion  int             `json:"applied_version"`
	Settings        json.RawMessage `json:"settings"`
	AgentVersion    string          `json:"agent_version"`
	IgnoredExcludes []string        `json:"ignored_excludes"`
	// Offsite is the client's wish; nil (field absent) leaves the stored one
	// alone, an empty list clears it.
	Offsite []string `json:"offsite"`
}

// managedPush is a signed settings push handed to a client in a report reply.
type managedPush struct {
	Version      int    `json:"version"`
	SettingsJSON string `json:"settings_json"`
	MAC          string `json:"mac"`
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	var req reportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing host"})
		return
	}
	// A client with its own login must present it (the dashboard password
	// also works); hosts on the shared repo have nothing to present.
	if _, own := s.cfg.Load().Clients[req.Host]; own && !s.isClient(r, req.Host) && !s.isAdmin(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "client login required"})
		return
	}
	cmd := s.live.report(req.Host, req.Live, time.Now())
	if req.LastResult != nil {
		s.recordOutcome(req.Host, req.LastResult.OK, req.LastResult.Message)
	}
	reply := map[string]any{"command": cmd}
	if req.CheckIn {
		// Tell someone when a client that was accepting the server's settings
		// stops doing so.
		if prev := s.store.ClientInfoFor(req.Host); prev.Managed && !req.Managed {
			if cl, ok := s.cfg.Load().Clients[req.Host]; ok && cl.Managed != nil {
				go s.Notify(context.Background(), "remote management was turned off on a client",
					req.Host+" no longer accepts the backup settings saved on the server; it now decides by itself what it backs up.")
			}
		}
		wish := s.store.ClientInfoFor(req.Host).OffsiteWish
		// Only the client itself (or the admin) may choose storages; a host on
		// the shared repository has no login, so its claim is not trusted.
		if req.Offsite != nil && (s.isClient(r, req.Host) || s.isAdmin(r)) {
			wish = cleanWish(s.cfg.Load(), req.Offsite)
		}
		s.store.SetClientInfo(req.Host, state.ClientInfo{
			SeenAt: time.Now(), AgentVersion: req.AgentVersion, Managed: req.Managed,
			AppliedVersion: req.AppliedVersion, Settings: req.Settings, IgnoredExcludes: req.IgnoredExcludes,
			OffsiteWish: wish,
		})
		// Hand over settings saved on the dashboard that this client has not
		// applied yet. Only clients with their own repository can be managed:
		// the push is signed with that repository's password.
		if cl, ok := s.cfg.Load().Clients[req.Host]; ok && req.Managed && cl.Managed != nil && cl.ManagedVersion > req.AppliedVersion {
			raw, _ := json.Marshal(cl.Managed)
			reply["config"] = managedPush{
				Version: cl.ManagedVersion, SettingsJSON: string(raw),
				MAC: config.SignManaged(cl.Password, req.Host, cl.ManagedVersion, string(raw)),
			}
		}
		cfg := s.cfg.Load()
		stores := make([]map[string]string, 0, len(cfg.Offsite))
		for _, o := range cfg.Offsite {
			stores = append(stores, map[string]string{"id": o.ID, "label": o.Label})
		}
		reply["offsite_storages"] = stores
		reply["offsite_server"] = cfg.ServerOffsiteFor(req.Host)
	}
	writeJSON(w, http.StatusOK, reply)
}

// cleanWish keeps the known storage ids once each, at most one per storage.
func cleanWish(c *config.Config, ids []string) []string {
	out := []string{}
	for _, id := range ids {
		if _, ok := c.OffsiteByID(id); ok && !slices.Contains(out, id) && len(out) < len(c.Offsite) {
			out = append(out, id)
		}
	}
	return out
}

// handleSetClientOffsite sets which offsite storages a host is mirrored to:
// storages null drops the routing entry (back to the defaults), a list
// (possibly empty = hot only) sets it. Works for any host name, including
// machines on the shared repository.
func (s *Server) handleSetClientOffsite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host     string    `json:"host"`
		Storages *[]string `json:"storages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing host"})
		return
	}
	if req.Storages != nil {
		for _, id := range *req.Storages {
			if _, ok := s.cfg.Load().OffsiteByID(id); !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown offsite storage " + id})
				return
			}
		}
	}
	err := s.cfg.Update(func(c *config.Config) {
		routing := maps.Clone(c.Routing)
		if req.Storages == nil {
			delete(routing, req.Host)
		} else {
			if routing == nil {
				routing = map[string][]string{}
			}
			routing[req.Host] = slices.Clone(*req.Storages)
		}
		c.Routing = routing
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleClientConfig (GET) returns what a client last reported about itself
// and what the dashboard has saved for it.
func (s *Server) handleClientConfig(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	cl, own := s.cfg.Load().Clients[host]
	info := s.store.ClientInfoFor(host)
	writeJSON(w, http.StatusOK, map[string]any{
		"host":            host,
		"own_repo":        own,
		"info":            info,
		"desired":         cl.Managed,
		"desired_version": cl.ManagedVersion,
		"pending":         own && cl.Managed != nil && cl.ManagedVersion > info.AppliedVersion,
	})
}

// handleSetClientConfig saves new settings for a client. The client applies
// them at its next check-in, if it has remote management turned on.
func (s *Server) handleSetClientConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host     string                 `json:"host"`
		Settings config.ManagedSettings `json:"settings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing host or settings"})
		return
	}
	if _, ok := s.cfg.Load().Clients[req.Host]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "only clients with their own repository can be managed from here"})
		return
	}
	if err := req.Settings.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var ver int
	err := s.cfg.Update(func(c *config.Config) {
		clients := maps.Clone(c.Clients)
		cl := clients[req.Host]
		// Stay ahead of whatever the client says it applied, e.g. after the
		// server's config was restored from an older copy.
		ver = max(cl.ManagedVersion, s.store.ClientInfoFor(req.Host).AppliedVersion) + 1
		cl.Managed, cl.ManagedVersion, cl.ManagedAt = &req.Settings, ver, time.Now()
		clients[req.Host] = cl
		c.Clients = clients
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": ver})
}

// handleClientBackup asks a managed client to start a backup at its next
// check-in. Only an agent that runs continuously can act on it.
func (s *Server) handleClientBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing host"})
		return
	}
	s.live.setCommand(req.Host, "backup")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
