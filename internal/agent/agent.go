// Package agent is the desktop-side backup client: a small web GUI + scheduler
// that backs up user-chosen paths to a hoard server's restic REST endpoint.
// The "what to back up" decision lives here (editable in the GUI), not baked
// into system config.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/restic"
)

// Config is the agent's persisted configuration, edited via the GUI.
type Config struct {
	// Repository is the hoard server's restic REST URL, e.g.
	// "rest:http://truenas:8000/hot".
	Repository string `json:"repository"`
	// PasswordFile holds the hot-repo password (sops-provisioned). If empty,
	// RESTIC_PASSWORD from the environment is used.
	PasswordFile string `json:"password_file"`
	// Host overrides the snapshot hostname (defaults to os.Hostname()).
	Host string `json:"host"`
	// Paths are the directories/files to back up.
	Paths []string `json:"paths"`
	// Excludes are restic --exclude patterns.
	Excludes []string `json:"excludes"`
	// Schedule is a daily "HH:MM" backup time; empty disables the timer.
	Schedule string `json:"schedule"`
	// Tags applied to each snapshot.
	Tags []string `json:"tags"`
}

// Agent owns config persistence, the restic client, and run state.
type Agent struct {
	mu        sync.Mutex
	cfg       Config
	cfgPath   string
	log       *slog.Logger
	resticBin string

	running  bool
	lastRun  RunResult
	progress *restic.Progress // live progress while running, nil when idle
}

// RunResult records the outcome of the most recent backup.
type RunResult struct {
	StartedAt time.Time            `json:"started_at"`
	EndedAt   time.Time            `json:"ended_at"`
	OK        bool                 `json:"ok"`
	Message   string               `json:"message"`
	Summary   *restic.BackupResult `json:"summary,omitempty"`
	Output    string               `json:"output,omitempty"`
}

// Load reads the agent config from path (creating a default if absent).
func Load(path, resticBin string, log *slog.Logger) (*Agent, error) {
	if resticBin == "" {
		resticBin = "restic"
	}
	a := &Agent{cfgPath: path, log: log, resticBin: resticBin}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		host, _ := os.Hostname()
		a.cfg = Config{Host: host, Paths: []string{}, Excludes: defaultExcludes(), Tags: []string{}}
		a.applyEnv()
		return a, a.persist()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &a.cfg); err != nil {
		return nil, fmt.Errorf("parse agent config: %w", err)
	}
	if a.cfg.Host == "" {
		a.cfg.Host, _ = os.Hostname()
	}
	a.applyEnv()
	return a, nil
}

// applyEnv lets a declarative deployment (e.g. a NixOS systemd unit) pin the
// server URL, password file, and host without touching the GUI-editable JSON,
// which continues to own the user's path/exclude/schedule choices. Env wins at
// startup so infra stays declarative even if the JSON drifts.
func (a *Agent) applyEnv() {
	if v := os.Getenv("HOARD_AGENT_REPOSITORY"); v != "" {
		a.cfg.Repository = v
	}
	if v := os.Getenv("HOARD_AGENT_PASSWORD_FILE"); v != "" {
		a.cfg.PasswordFile = v
	}
	if v := os.Getenv("HOARD_AGENT_HOST"); v != "" {
		a.cfg.Host = v
	}
}

func defaultExcludes() []string {
	return []string{
		"**/.cache", "**/node_modules", "**/.git", "**/target",
		"**/*.tmp", "**/Downloads", "**/.local/share/Trash",
	}
}

// Snapshot returns a copy of the current config.
func (a *Agent) GetConfig() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// SetConfig replaces the editable fields and persists.
func (a *Agent) SetConfig(c Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Preserve fields not exposed for edit if the caller left them blank.
	if c.Host == "" {
		c.Host = a.cfg.Host
	}
	c.Paths = cleanList(c.Paths)
	c.Excludes = cleanList(c.Excludes)
	c.Tags = cleanList(c.Tags)
	a.cfg = c
	return a.persist()
}

// LastRun returns the most recent backup result and whether one is in flight.
func (a *Agent) LastRun() (RunResult, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastRun, a.running
}

// Progress returns the live backup progress, or nil when idle.
func (a *Agent) Progress() *restic.Progress {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.progress == nil {
		return nil
	}
	p := *a.progress
	return &p
}

func (a *Agent) resticClient() (*restic.Client, error) {
	pw := os.Getenv("RESTIC_PASSWORD")
	if a.cfg.PasswordFile != "" {
		b, err := os.ReadFile(a.cfg.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("read password file: %w", err)
		}
		pw = trimNewline(string(b))
	}
	if pw == "" {
		return nil, fmt.Errorf("no repo password (set password_file or RESTIC_PASSWORD)")
	}
	if a.cfg.Repository == "" {
		return nil, fmt.Errorf("no server repository configured")
	}
	return restic.New(a.resticBin, config.Repo{Repository: a.cfg.Repository, Password: pw}), nil
}

// Backup runs one backup now. It is safe to call concurrently; a second call
// while one is running returns an error.
func (a *Agent) Backup(ctx context.Context) error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return fmt.Errorf("a backup is already running")
	}
	a.running = true
	cfg := a.cfg
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.running = false
		a.progress = nil // clear live progress once the run ends
		a.mu.Unlock()
	}()

	start := time.Now()
	rr := RunResult{StartedAt: start}

	cl, err := a.resticClient()
	if err != nil {
		rr.EndedAt = time.Now()
		rr.OK = false
		rr.Message = err.Error()
		a.storeRun(rr)
		return err
	}

	onProgress := func(p restic.Progress) {
		a.mu.Lock()
		pc := p
		a.progress = &pc
		a.mu.Unlock()
	}
	summary, out, err := cl.Backup(ctx, cfg.Paths, cfg.Excludes, cfg.Host, cfg.Tags, onProgress)
	rr.EndedAt = time.Now()
	rr.Output = out
	rr.Summary = summary
	if err != nil {
		rr.OK = false
		rr.Message = err.Error()
		a.log.Error("backup failed", "err", err)
		a.storeRun(rr)
		return err
	}
	rr.OK = true
	if summary != nil {
		rr.Message = fmt.Sprintf("snapshot %s: %d files, %s added",
			short(summary.SnapshotID), summary.TotalFiles, humanBytes(summary.DataAddedPacked))
	} else {
		rr.Message = "backup completed"
	}
	a.log.Info("backup ok", "msg", rr.Message)
	a.storeRun(rr)
	return nil
}

// Snapshots lists this host's snapshots on the server.
func (a *Agent) Snapshots(ctx context.Context) ([]restic.Snapshot, error) {
	cl, err := a.resticClient()
	if err != nil {
		return nil, err
	}
	all, err := cl.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	host := a.GetConfig().Host
	out := all[:0]
	for _, s := range all {
		if s.Hostname == host {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// ScheduleTime returns the configured daily backup time, or "".
func (a *Agent) ScheduleTime() string { return a.GetConfig().Schedule }

func (a *Agent) storeRun(rr RunResult) {
	a.mu.Lock()
	a.lastRun = rr
	a.mu.Unlock()
}

func (a *Agent) persist() error {
	raw, err := json.MarshalIndent(a.cfg, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(a.cfgPath); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	tmp := a.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.cfgPath)
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = trimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
