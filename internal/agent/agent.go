// Package agent is the desktop-side backup client: a small web GUI + scheduler
// that backs up user-chosen paths to a hoard server's restic REST endpoint.
// The "what to back up" decision lives here (editable in the GUI), not baked
// into system config.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
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
	// ServerURL is the hoard server dashboard base URL that destructive deletes
	// are delegated to (only the server can reach e2). Empty = derive from
	// Repository (rest://host:8000/... -> http://host:8080).
	ServerURL string `json:"server_url"`
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

	// live control/observation state for the running backup
	cancel      context.CancelFunc
	proc        *os.Process
	paused      bool
	startedAt   time.Time
	pausedAccum time.Duration // total time spent paused so far
	pauseStart  time.Time     // when the current pause began (zero if not paused)
	activity    []string      // rolling per-file log tail (terminal view)
}

// effectiveElapsedLocked returns wall time since the backup started, minus time
// spent paused. Caller holds the lock. restic doesn't report elapsed/ETA, so we
// derive them here.
func (a *Agent) effectiveElapsedLocked() time.Duration {
	if a.startedAt.IsZero() {
		return 0
	}
	e := time.Since(a.startedAt) - a.pausedAccum
	if !a.pauseStart.IsZero() {
		e -= time.Since(a.pauseStart)
	}
	if e < 0 {
		e = 0
	}
	return e
}

// maxActivity is how many recent per-file lines the agent keeps for the UI.
const maxActivity = 300

// Live is a snapshot of the running backup for the dashboard and for reporting
// to the server.
type Live struct {
	Running   bool             `json:"running"`
	Paused    bool             `json:"paused"`
	StartedAt time.Time        `json:"started_at"`
	Progress  *restic.Progress `json:"progress,omitempty"`
	Activity  []string         `json:"activity,omitempty"`
}

// Live returns the current running-backup state (safe copy).
func (a *Agent) Live() Live {
	a.mu.Lock()
	defer a.mu.Unlock()
	l := Live{Running: a.running, Paused: a.paused, StartedAt: a.startedAt}
	if a.progress != nil {
		p := *a.progress
		// restic doesn't report timing — derive elapsed from the start time
		// (minus paused time) and ETA from the byte rate.
		elapsed := a.effectiveElapsedLocked()
		p.SecondsElapsed = int(elapsed.Seconds())
		p.SecondsRemaining = 0
		// ETA from the byte rate over effective (un-paused) elapsed; stays
		// meaningful even while paused since both inputs are frozen.
		if p.BytesDone > 0 && elapsed > 0 && p.TotalBytes > p.BytesDone {
			rate := float64(p.BytesDone) / elapsed.Seconds()
			if rate > 0 {
				p.SecondsRemaining = int(float64(p.TotalBytes-p.BytesDone) / rate)
			}
		}
		l.Progress = &p
	}
	if len(a.activity) > 0 {
		l.Activity = append([]string(nil), a.activity...)
	}
	return l
}

// Pause suspends the running restic process (SIGSTOP). Best-effort.
func (a *Agent) Pause() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.proc == nil {
		return fmt.Errorf("no backup running")
	}
	if err := a.proc.Signal(syscall.SIGSTOP); err != nil {
		return err
	}
	a.paused = true
	a.pauseStart = time.Now()
	a.appendActivityLocked("— paused —")
	return nil
}

// Resume continues a paused restic process (SIGCONT).
func (a *Agent) Resume() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.proc == nil {
		return fmt.Errorf("no backup running")
	}
	if err := a.proc.Signal(syscall.SIGCONT); err != nil {
		return err
	}
	if !a.pauseStart.IsZero() {
		a.pausedAccum += time.Since(a.pauseStart)
		a.pauseStart = time.Time{}
	}
	a.paused = false
	a.appendActivityLocked("— resumed —")
	return nil
}

// Cancel stops the running backup (resuming first if paused, so the kill lands).
func (a *Agent) Cancel() error {
	a.mu.Lock()
	proc, cancel, paused := a.proc, a.cancel, a.paused
	if cancel == nil {
		a.mu.Unlock()
		return fmt.Errorf("no backup running")
	}
	a.appendActivityLocked("— cancelling —")
	a.mu.Unlock()
	if paused && proc != nil {
		_ = proc.Signal(syscall.SIGCONT)
	}
	cancel()
	return nil
}

func (a *Agent) appendActivityLocked(line string) {
	a.activity = append(a.activity, line)
	if len(a.activity) > maxActivity {
		a.activity = a.activity[len(a.activity)-maxActivity:]
	}
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
	if v := os.Getenv("HOARD_AGENT_SERVER_URL"); v != "" {
		a.cfg.ServerURL = v
	}
}

// Host returns the snapshot hostname this agent backs up as.
func (a *Agent) Host() string { return a.GetConfig().Host }

// ServerBaseURL returns the hoard server dashboard URL for delegating deletes.
// It uses the configured ServerURL, or derives it from the restic REST
// repository: rest:http://host:8000/hot -> http://host:8080.
func (a *Agent) ServerBaseURL() string {
	c := a.GetConfig()
	if c.ServerURL != "" {
		return strings.TrimRight(c.ServerURL, "/")
	}
	repo := c.Repository
	repo = strings.TrimPrefix(repo, "rest:")
	u, err := url.Parse(repo)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return u.Scheme + "://" + u.Hostname() + ":8080"
}

// reportLoop pushes the agent's live state to the server every second during a
// backup, and applies any control command the server hands back (pause/resume/
// cancel). This is how the server both sees and controls client backups even
// though agents bind localhost-only: the agent always initiates the connection.
func (a *Agent) reportLoop(ctx context.Context) {
	base := a.ServerBaseURL()
	if base == "" {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.report(client, base, &Live{Running: false}) // final: mark idle
			return
		case <-t.C:
			live := a.Live()
			switch a.report(client, base, &live) {
			case "pause":
				_ = a.Pause()
			case "resume":
				_ = a.Resume()
			case "cancel":
				_ = a.Cancel()
			}
		}
	}
}

// report POSTs one live snapshot to the server and returns any queued command.
func (a *Agent) report(client *http.Client, base string, live *Live) string {
	body, _ := json.Marshal(map[string]any{"host": a.Host(), "live": live})
	req, err := http.NewRequest(http.MethodPost, base+"/api/report", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Command string `json:"command"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Command
}

// Ls lists one directory level inside a snapshot on the server (hot repo).
func (a *Agent) Ls(ctx context.Context, snapID, dir string) ([]restic.LsEntry, error) {
	cl, err := a.resticClient()
	if err != nil {
		return nil, err
	}
	return cl.Ls(ctx, snapID, dir)
}

// Dump streams a file from a snapshot (hot repo) to w.
func (a *Agent) Dump(ctx context.Context, snapID, filePath string, w io.Writer) error {
	cl, err := a.resticClient()
	if err != nil {
		return err
	}
	return cl.Dump(ctx, snapID, filePath, w)
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
	runCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		cancel()
		return fmt.Errorf("a backup is already running")
	}
	a.running = true
	a.paused = false
	a.cancel = cancel
	a.startedAt = time.Now()
	a.pausedAccum = 0
	a.pauseStart = time.Time{}
	a.activity = nil
	a.progress = nil
	cfg := a.cfg
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.running = false
		a.paused = false
		a.progress = nil // clear live progress once the run ends
		a.proc = nil
		a.cancel = nil
		a.mu.Unlock()
		cancel()
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

	hooks := restic.BackupHooks{
		OnProgress: func(p restic.Progress) {
			a.mu.Lock()
			pc := p
			a.progress = &pc
			a.mu.Unlock()
		},
		OnActivity: func(action, item string) {
			a.mu.Lock()
			a.appendActivityLocked(action + "  " + item)
			a.mu.Unlock()
		},
		OnStart: func(p *os.Process) {
			a.mu.Lock()
			a.proc = p
			a.mu.Unlock()
		},
	}
	go a.reportLoop(runCtx) // stream live state to the server (and pick up commands)
	summary, out, err := cl.Backup(runCtx, cfg.Paths, cfg.Excludes, cfg.Host, cfg.Tags, hooks)
	rr.EndedAt = time.Now()
	rr.Output = out
	rr.Summary = summary
	if err != nil {
		rr.OK = false
		if runCtx.Err() == context.Canceled {
			rr.Message = "cancelled"
			a.log.Info("backup cancelled")
		} else {
			rr.Message = err.Error()
			a.log.Error("backup failed", "err", err)
		}
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
