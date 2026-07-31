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

// SizeSample records a repo-size measurement at a point in time.
type SizeSample struct {
	At         time.Time `json:"at"`
	HotStored  int64     `json:"hot_stored"`
	ColdStored int64     `json:"cold_stored"`
}

// Outcome is the result of a client's most recent backup run (report-derived).
// Kept separate from Client (which is rebuilt from snapshots each tick) so the
// freshness refresh can't clobber it. ConsecutiveFailures is maintained server-side.
type Outcome struct {
	OK                  bool      `json:"ok"`
	Message             string    `json:"message"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	At                  time.Time `json:"at"`
}

// VerifyResult records the most recent restore fire-drill.
type VerifyResult struct {
	Time   time.Time `json:"time"`
	OK     bool      `json:"ok"`
	Client string    `json:"client"`
	File   string    `json:"file"`
	Bytes  uint64    `json:"bytes"`
	Err    string    `json:"err,omitempty"`
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
	LastByJob      map[string]JobResult `json:"last_by_job"`
	ClientOutcomes map[string]Outcome   `json:"client_outcomes"`
	LastVerify     *VerifyResult        `json:"last_verify,omitempty"`
	SizeSamples    []SizeSample         `json:"size_samples,omitempty"`
}

// Load reads the store from path, or returns an empty one if it doesn't exist.
func Load(path string) (*Store, error) {
	s := &Store{path: path, Clients: map[string]Client{}, LastByJob: map[string]JobResult{}, ClientOutcomes: map[string]Outcome{}}
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
	if s.ClientOutcomes == nil {
		s.ClientOutcomes = map[string]Outcome{}
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

// SetOutcome records a client's latest run outcome and persists.
func (s *Store) SetOutcome(host string, o Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ClientOutcomes[host] = o
	s.save()
}

// OutcomeFor returns the stored outcome for host (zero value if none).
func (s *Store) OutcomeFor(host string) Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ClientOutcomes[host]
}

// SetVerify records the latest restore-verification result and persists.
func (s *Store) SetVerify(r VerifyResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastVerify = &r
	s.save()
}

// AppendSizeSample records a repo-size sample, caps the series at 400 newest,
// and persists.
func (s *Store) AppendSizeSample(sample SizeSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SizeSamples = append(s.SizeSamples, sample)
	if len(s.SizeSamples) > 400 {
		s.SizeSamples = s.SizeSamples[len(s.SizeSamples)-400:]
	}
	s.save()
}

// SizeSamplesSnapshot returns a copy of the size series under lock.
func (s *Store) SizeSamplesSnapshot() []SizeSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SizeSample, len(s.SizeSamples))
	copy(out, s.SizeSamples)
	return out
}

// LastSizeSampleAt returns the timestamp of the newest sample (zero if none).
func (s *Store) LastSizeSampleAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.SizeSamples) == 0 {
		return time.Time{}
	}
	return s.SizeSamples[len(s.SizeSamples)-1].At
}

// View is a lock-free, read-only copy of the store for handlers to consume.
type View struct {
	History        []JobResult          `json:"history"`
	Clients        map[string]Client    `json:"clients"`
	LastByJob      map[string]JobResult `json:"last_by_job"`
	ClientOutcomes map[string]Outcome   `json:"client_outcomes"`
	LastVerify     *VerifyResult        `json:"last_verify,omitempty"`
}

// Snapshot returns a deep-ish copy safe for read-only handlers.
func (s *Store) Snapshot() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := View{
		History:        append([]JobResult(nil), s.History...),
		Clients:        make(map[string]Client, len(s.Clients)),
		LastByJob:      make(map[string]JobResult, len(s.LastByJob)),
		ClientOutcomes: make(map[string]Outcome, len(s.ClientOutcomes)),
	}
	for k, v := range s.Clients {
		cp.Clients[k] = v
	}
	for k, v := range s.LastByJob {
		cp.LastByJob[k] = v
	}
	for k, v := range s.ClientOutcomes {
		cp.ClientOutcomes[k] = v
	}
	if s.LastVerify != nil {
		v := *s.LastVerify
		cp.LastVerify = &v
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
