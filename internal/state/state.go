// Package state tracks the outcome of recurring jobs and per-client freshness,
// persisting to a JSON file so history survives restarts. It is the source of
// truth the dashboard and staleness alerting read from.
package state

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// JobResult records one execution of a named job (mirror/check/prune).
type JobResult struct {
	Job       string    `json:"job"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	OK        bool      `json:"ok"`
	Message   string    `json:"message"`
	Output    string    `json:"output,omitempty"`
}

// Client is the freshness view of one host that pushes to the hot repo.
type Client struct {
	Hostname     string    `json:"hostname"`
	LastSnapshot time.Time `json:"last_snapshot"`
	SnapshotID   string    `json:"snapshot_id"`
	Paths        []string  `json:"paths"`
	Stale        bool      `json:"stale"`
	Size         uint64    `json:"size"` // latest snapshot's logical size
}

// Store is the persisted, concurrency-safe application state.
type Store struct {
	mu      sync.Mutex
	path    string
	History []JobResult       `json:"history"`
	Clients map[string]Client `json:"clients"`
	// LastByJob is the most recent result per job name, for quick dashboard reads.
	LastByJob map[string]JobResult `json:"last_by_job"`
}

// Load reads the store from path, or returns an empty one if it doesn't exist.
func Load(path string) (*Store, error) {
	s := &Store{path: path, Clients: map[string]Client{}, LastByJob: map[string]JobResult{}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, err
	}
	if s.Clients == nil {
		s.Clients = map[string]Client{}
	}
	if s.LastByJob == nil {
		s.LastByJob = map[string]JobResult{}
	}
	s.path = path
	return s, nil
}

// RecordJob appends a result, updates the per-job index, and persists.
func (s *Store) RecordJob(r JobResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.History = append(s.History, r)
	// Cap history to the most recent 200 entries.
	if len(s.History) > 200 {
		s.History = s.History[len(s.History)-200:]
	}
	s.LastByJob[r.Job] = r
	s.save()
}

// SetClients replaces the client freshness map and persists.
func (s *Store) SetClients(clients map[string]Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Clients = clients
	s.save()
}

// View is a lock-free, read-only copy of the store for handlers to consume.
type View struct {
	History   []JobResult          `json:"history"`
	Clients   map[string]Client    `json:"clients"`
	LastByJob map[string]JobResult `json:"last_by_job"`
}

// Snapshot returns a deep-ish copy safe for read-only handlers.
func (s *Store) Snapshot() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := View{
		History:   append([]JobResult(nil), s.History...),
		Clients:   make(map[string]Client, len(s.Clients)),
		LastByJob: make(map[string]JobResult, len(s.LastByJob)),
	}
	for k, v := range s.Clients {
		cp.Clients[k] = v
	}
	for k, v := range s.LastByJob {
		cp.LastByJob[k] = v
	}
	return cp
}

// save writes atomically (temp file + rename). Caller must hold the lock.
func (s *Store) save() {
	if s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}
