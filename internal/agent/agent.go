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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/restic"
	"github.com/nauski/hoard/internal/version"
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
	// ExcludeFiles are files of restic exclude patterns, one per line
	// (--exclude-file); keep the list in a dotfile instead of the GUI. restic
	// patterns allow "!" negation: "node_modules" then "!/home/me/keep/**/node_modules".
	ExcludeFiles []string `json:"exclude_files,omitempty"`
	// ExcludeCaches skips directories marked with a CACHEDIR.TAG file.
	ExcludeCaches bool `json:"exclude_caches,omitempty"`
	// ExcludeIfPresent skips any directory containing one of these file names
	// (e.g. ".nobackup").
	ExcludeIfPresent []string `json:"exclude_if_present,omitempty"`
	// ExcludeLargerThan skips files above this size ("1G", "500M").
	ExcludeLargerThan string `json:"exclude_larger_than,omitempty"`
	// Nice (1-19) and IONiceClass ("idle" or "best-effort") lower the backup's
	// CPU and disk priority. ReadConcurrency is restic's --read-concurrency
	// (0 = restic's default of 2). NoScan skips the pre-scan that totals the
	// tree: one tree walk less per run, at the price of no percentage or ETA.
	Nice            int    `json:"nice,omitempty"`
	IONiceClass     string `json:"ionice_class,omitempty"`
	ReadConcurrency int    `json:"read_concurrency,omitempty"`
	NoScan          bool   `json:"no_scan,omitempty"`
	// Schedule is either a daily "HH:MM" backup time or an interval such as
	// "1h" / "30m"; empty disables the scheduler.
	Schedule string `json:"schedule"`
	// Tags applied to each snapshot.
	Tags []string `json:"tags"`
	// ServerURL is the hoard server dashboard base URL that destructive deletes
	// are delegated to (only the server can reach e2). Empty = derive from
	// Repository (rest://host:8000/... -> http://host:8080).
	ServerURL string `json:"server_url"`
	// LimitUploadKiBps / LimitDownloadKiBps cap this agent's backup transfer
	// speed (KiB/s); 0 = unlimited.
	LimitUploadKiBps   int `json:"limit_upload_kibps"`
	LimitDownloadKiBps int `json:"limit_download_kibps"`
	// RemoteManagement lets the hoard server's dashboard change this agent's
	// backup settings (paths, excludes, schedule, priority) and start a backup.
	// Off by default; turn it on locally with `hoard-agent manage on`. The
	// server can never change the repository, password file or server address.
	RemoteManagement bool `json:"remote_management,omitempty"`
	// ManagedVersion is the last settings push from the server that was applied.
	ManagedVersion int `json:"managed_version,omitempty"`
	// NotifyDesktop controls whether desktop notifications are sent on backup completion.
	// Defaults to enabled (nil or true).
	NotifyDesktop *bool `json:"notify_desktop,omitempty"`
}

// Agent owns config persistence, the restic client, and run state.
type Agent struct {
	mu        sync.Mutex
	cfg       Config
	cfgPath   string
	log       *slog.Logger
	resticBin string
	notifyBin string

	running   bool
	lastRun   RunResult
	verOnce   sync.Once
	retryLock bool             // local restic supports --retry-lock
	lastStart time.Time        // when the most recent backup started (persisted in state.json)
	boot      time.Time        // when this process loaded; see nextRun
	progress  *restic.Progress // live progress while running, nil when idle

	// live control/observation state for the running backup
	cancel      context.CancelFunc
	proc        *os.Process
	paused      bool
	kind        string // "backup" or "restore" while running, "" when idle
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
	Kind      string           `json:"kind,omitempty"`
	StartedAt time.Time        `json:"started_at"`
	Progress  *restic.Progress `json:"progress,omitempty"`
	Activity  []string         `json:"activity,omitempty"`
}

// Live returns the current running-backup state (safe copy).
func (a *Agent) Live() Live {
	a.mu.Lock()
	defer a.mu.Unlock()
	l := Live{Running: a.running, Paused: a.paused, StartedAt: a.startedAt}
	l.Kind = a.kind
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
	Kind      string               `json:"kind,omitempty"`
	Message   string               `json:"message"`
	Summary   *restic.BackupResult `json:"summary,omitempty"`
	// Warnings lists files restic could not read; the run is still OK.
	Warnings []string `json:"warnings,omitempty"`
	Output   string   `json:"output,omitempty"`
}

// Load reads the agent config from path (creating a default if absent).
func Load(path, resticBin, notifyBin string, log *slog.Logger) (*Agent, error) {
	if resticBin == "" {
		resticBin = "restic"
	}
	a := &Agent{cfgPath: path, log: log, resticBin: resticBin, notifyBin: notifyBin, boot: time.Now()}
	a.loadLastStart()
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

// applyEnv SEEDS empty settings from the environment (a declarative deploy like
// NixOS can supply an initial server URL / password file / host) but does NOT
// override values already set in the GUI-editable JSON. The Settings panel is
// the source of truth for where the client connects; env just provides the
// first-run default so a fresh install works out of the box.
func (a *Agent) applyEnv() {
	if a.cfg.Repository == "" {
		a.cfg.Repository = os.Getenv("HOARD_AGENT_REPOSITORY")
	}
	if a.cfg.PasswordFile == "" {
		a.cfg.PasswordFile = os.Getenv("HOARD_AGENT_PASSWORD_FILE")
	}
	if a.cfg.Host == "" {
		a.cfg.Host = os.Getenv("HOARD_AGENT_HOST")
	}
	if a.cfg.ServerURL == "" {
		a.cfg.ServerURL = os.Getenv("HOARD_AGENT_SERVER_URL")
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
			a.report(client, base, &Live{Running: false}, a.backupOutcome()) // final: mark idle + outcome
			return
		case <-t.C:
			live := a.Live()
			switch a.report(client, base, &live, nil) {
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

// reportOutcome is the completed-backup result the agent appends to its final
// report so the server can track failures.
type reportOutcome struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// backupOutcome returns the just-finished run's outcome IF it was a backup
// that ran to completion (not a restore, not a user cancel); otherwise nil.
func (a *Agent) backupOutcome() *reportOutcome {
	a.mu.Lock()
	defer a.mu.Unlock()
	rr := a.lastRun
	if rr.Kind != "backup" {
		return nil
	}
	if !rr.OK && rr.Message == "cancelled" {
		return nil
	}
	return &reportOutcome{OK: rr.OK, Message: rr.Message}
}

// managedSettings returns the part of the config the server may manage.
func (c Config) managedSettings() config.ManagedSettings {
	return config.ManagedSettings{
		Paths: c.Paths, Excludes: c.Excludes, ExcludeFiles: c.ExcludeFiles,
		ExcludeIfPresent: c.ExcludeIfPresent, ExcludeLargerThan: c.ExcludeLargerThan,
		ExcludeCaches: c.ExcludeCaches, Schedule: c.Schedule, NoScan: c.NoScan,
		ReadConcurrency: c.ReadConcurrency, Nice: c.Nice, IONiceClass: c.IONiceClass,
	}
}

// SetRemoteManagement turns server-side management of this agent on or off.
func (a *Agent) SetRemoteManagement(on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.RemoteManagement = on
	return a.persist()
}

// CheckIn tells the server this agent exists, what it is configured to do and
// which settings push it last applied, and acts on the reply: new settings
// and "back up now", both only when remote management is on. Daemons call it
// every minute, one-shot runs once before backing up. It is how a headless
// machine is managed without anything connecting in to it. It reports whether
// it started a backup, so the caller does not start a second one alongside.
func (a *Agent) CheckIn(ctx context.Context, allowBackup bool) (startedBackup bool) {
	base := a.ServerBaseURL()
	if base == "" {
		return false
	}
	client := &http.Client{Timeout: 10 * time.Second}
	send := func() serverReply {
		cfg := a.GetConfig()
		live := a.Live()
		return a.reportFull(client, base, &live, nil, map[string]any{
			"checkin":         true,
			"managed":         cfg.RemoteManagement,
			"applied_version": cfg.ManagedVersion,
			"settings":        cfg.managedSettings(),
			"agent_version":   version.Version,
		})
	}
	reply := send()
	if reply.Config != nil {
		if err := a.applyManaged(reply.Config.Version, reply.Config.SettingsJSON, reply.Config.MAC); err != nil {
			a.log.Warn("settings from the server were not applied", "err", err)
		} else {
			send() // tell the server right away, so its dashboard stops showing "pending"
		}
	}
	if reply.Command == "backup" && allowBackup && a.GetConfig().RemoteManagement {
		if _, running := a.LastRun(); running {
			a.log.Info("backup requested from the server dashboard, but one is already running")
			return false
		}
		a.log.Info("backup requested from the server dashboard")
		go func() {
			if err := a.Backup(ctx); err != nil {
				a.log.Error("requested backup failed", "err", err)
			}
		}()
		return true
	}
	return false
}

// applyManaged installs a settings push after checking that this agent opted
// in, that the push is signed with this client's repository password, and that
// it is newer than the last one applied.
func (a *Agent) applyManaged(ver int, settingsJSON, mac string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.cfg.RemoteManagement {
		return fmt.Errorf("remote management is off on this machine (enable with: hoard-agent manage on)")
	}
	if ver <= a.cfg.ManagedVersion {
		return nil // already applied
	}
	key := os.Getenv("RESTIC_PASSWORD")
	if a.cfg.PasswordFile != "" {
		b, err := os.ReadFile(a.cfg.PasswordFile)
		if err != nil {
			return fmt.Errorf("read password file: %w", err)
		}
		key = trimNewline(string(b))
	}
	if !config.VerifyManaged(key, a.cfg.Host, ver, settingsJSON, mac) {
		return fmt.Errorf("signature check failed; ignoring the push")
	}
	var m config.ManagedSettings
	if err := json.Unmarshal([]byte(settingsJSON), &m); err != nil {
		return fmt.Errorf("unreadable settings: %w", err)
	}
	if err := m.Validate(); err != nil {
		return err
	}
	c := &a.cfg
	c.Paths, c.Excludes, c.ExcludeFiles = cleanList(m.Paths), cleanList(m.Excludes), cleanList(m.ExcludeFiles)
	c.ExcludeIfPresent, c.ExcludeLargerThan, c.ExcludeCaches = cleanList(m.ExcludeIfPresent), m.ExcludeLargerThan, m.ExcludeCaches
	c.Schedule, c.NoScan, c.ReadConcurrency = m.Schedule, m.NoScan, m.ReadConcurrency
	c.Nice, c.IONiceClass = m.Nice, m.IONiceClass
	c.ManagedVersion = ver
	a.log.Info("applied settings from the server dashboard", "version", ver, "paths", len(c.Paths), "schedule", c.Schedule)
	return a.persist()
}

// serverReply is what the server answers to a report.
type serverReply struct {
	Command string `json:"command"`
	Config  *struct {
		Version      int    `json:"version"`
		SettingsJSON string `json:"settings_json"`
		MAC          string `json:"mac"`
	} `json:"config"`
}

// report POSTs one live snapshot (and, on the final report, the completed
// backup's outcome) to the server and returns any queued command.
func (a *Agent) report(client *http.Client, base string, live *Live, outcome *reportOutcome) string {
	return a.reportFull(client, base, live, outcome, nil).Command
}

// reportFull is report with extra payload fields and the whole reply.
func (a *Agent) reportFull(client *http.Client, base string, live *Live, outcome *reportOutcome, extra map[string]any) serverReply {
	payload := map[string]any{"host": a.Host(), "live": live}
	if outcome != nil {
		payload["last_result"] = outcome
	}
	for k, v := range extra {
		payload[k] = v
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, base+"/api/report", bytes.NewReader(body))
	if err != nil {
		return serverReply{}
	}
	req.Header.Set("Content-Type", "application/json")
	// A client with its own repository logs in to the report API with the same
	// user and password that its repository URL carries.
	if u, err := url.Parse(strings.TrimPrefix(a.GetConfig().Repository, "rest:")); err == nil && u.User != nil {
		pw, _ := u.User.Password()
		req.SetBasicAuth(u.User.Username(), pw)
	}
	resp, err := client.Do(req)
	if err != nil {
		return serverReply{}
	}
	defer resp.Body.Close()
	var out serverReply
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
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
	c.Schedule = trimSpace(c.Schedule)
	if c.IONiceClass != "" && c.IONiceClass != "idle" && c.IONiceClass != "best-effort" {
		return fmt.Errorf("ionice_class %q must be idle or best-effort", c.IONiceClass)
	}
	if c.Schedule != "" {
		if _, err := nextRun(c.Schedule, time.Time{}, time.Time{}, time.Now()); err != nil {
			return err
		}
	}
	// Preserve fields not exposed for edit if the caller left them blank (the GUI
	// posts only the fields it renders, so Host/ServerURL/Tags would otherwise be
	// wiped on every save). Paths/Excludes ARE editable, so an empty value there
	// is an intentional clear and is kept as sent.
	if c.Host == "" {
		c.Host = a.cfg.Host
	}
	if c.ServerURL == "" {
		c.ServerURL = a.cfg.ServerURL
	}
	c.Paths = cleanList(c.Paths)
	c.Excludes = cleanList(c.Excludes)
	c.Tags = cleanList(c.Tags)
	if len(c.Tags) == 0 {
		c.Tags = a.cfg.Tags
	}
	a.cfg = c
	return a.persist()
}

// Enroll redeems a token against the hoard server and writes the returned repo
// URL, password (to a 0600 file next to the config), and server URL into config.
func (a *Agent) Enroll(ctx context.Context, serverURL, token string) error {
	// The host name lets a server running private repositories create this
	// machine's own repository and login; older servers ignore it.
	body, _ := json.Marshal(map[string]string{"token": token, "host": a.GetConfig().Host})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(serverURL, "/")+"/api/enroll/redeem", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// A server creating this machine's own repository replies only once the
	// new login is live, which can take most of a minute.
	cl := &http.Client{Timeout: 90 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("enroll rejected: %s", e.Error)
	}
	var out struct {
		Repository string `json:"repository"`
		Password   string `json:"password"`
		ServerURL  string `json:"server_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if out.Repository == "" || out.Password == "" {
		return fmt.Errorf("enroll: incomplete response from server")
	}
	dir := filepath.Dir(a.cfgPath)
	if dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	pwPath := filepath.Join(dir, "enrolled-password")
	if err := os.WriteFile(pwPath, []byte(out.Password), 0o600); err != nil {
		return err
	}
	c := a.GetConfig()
	c.Repository = out.Repository
	c.PasswordFile = pwPath
	c.ServerURL = out.ServerURL
	return a.SetConfig(c)
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
	cl := restic.New(a.resticBin, config.Repo{
		Repository: a.cfg.Repository, Password: pw,
		LimitUploadKiBps: a.cfg.LimitUploadKiBps, LimitDownloadKiBps: a.cfg.LimitDownloadKiBps,
	})
	if a.canRetryLock() {
		cl.WithRetryLock(lockWait)
	}
	cl.WithBackupArgs(a.cfg.backupArgs()).WithWrapper(a.cfg.wrapper(exec.LookPath, a.log))
	return cl, nil
}

// backupArgs maps the exclude and tuning settings to `restic backup` flags.
func (c Config) backupArgs() []string {
	var args []string
	for _, f := range c.ExcludeFiles {
		args = append(args, "--exclude-file", f)
	}
	if c.ExcludeCaches {
		args = append(args, "--exclude-caches")
	}
	for _, n := range c.ExcludeIfPresent {
		args = append(args, "--exclude-if-present", n)
	}
	if c.ExcludeLargerThan != "" {
		args = append(args, "--exclude-larger-than", c.ExcludeLargerThan)
	}
	if c.ReadConcurrency > 0 {
		args = append(args, "--read-concurrency", fmt.Sprint(c.ReadConcurrency))
	}
	if c.NoScan {
		args = append(args, "--no-scan")
	}
	return args
}

// wrapper builds the nice/ionice prefix for the backup process. A tool that is
// not installed is skipped with a warning instead of failing the backup.
func (c Config) wrapper(look func(string) (string, error), log *slog.Logger) []string {
	var w []string
	add := func(tool string, args ...string) {
		p, err := look(tool)
		if err != nil {
			log.Warn("priority setting ignored: tool not found", "tool", tool)
			return
		}
		w = append(append(w, p), args...)
	}
	if c.Nice > 0 {
		add("nice", "-n", fmt.Sprint(min(c.Nice, 19)))
	}
	switch c.IONiceClass {
	case "idle":
		add("ionice", "-c", "3")
	case "best-effort":
		add("ionice", "-c", "2", "-n", "7")
	}
	return w
}

// DryRun reports what a backup would do right now, without writing anything
// to the repository: use it to check exclude rules before a first large run.
func (a *Agent) DryRun(ctx context.Context) (*restic.BackupResult, error) {
	cl, err := a.resticClient()
	if err != nil {
		return nil, err
	}
	cfg := a.GetConfig()
	cl.WithBackupArgs(append(cfg.backupArgs(), "--dry-run"))
	res, _, err := cl.Backup(ctx, cfg.Paths, cfg.Excludes, cfg.Host, cfg.Tags, restic.BackupHooks{})
	return res, err
}

// lockWait is how long a backup waits for the server's prune or check to
// release the repository before it counts as failed.
const lockWait = "30m"

// canRetryLock reports whether the local restic understands --retry-lock
// (0.16+). Checked once; an unknown version is treated as too old, because an
// unsupported flag would fail every backup.
func (a *Agent) canRetryLock() bool {
	a.verOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		maj, min, err := restic.Version(ctx, a.resticBin)
		a.retryLock = err == nil && (maj > 0 || min >= 16)
	})
	return a.retryLock
}

// Backup runs one backup now. It is safe to call concurrently; a second call
// while one is running returns an error.
func (a *Agent) Backup(ctx context.Context) error { return a.BackupPin(ctx, "") }

// BackupPin runs one backup and, when note is non-empty, pins the snapshot:
// it is tagged so the server's retention keeps it until someone unpins it
// there. Use before a risky operation ("before worktree cleanup").
func (a *Agent) BackupPin(ctx context.Context, note string) error {
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
	a.saveLastStart(time.Now())

	reported := make(chan struct{})
	defer func() {
		a.mu.Lock()
		a.running = false
		a.paused = false
		a.progress = nil // clear live progress once the run ends
		a.proc = nil
		a.cancel = nil
		a.mu.Unlock()
		cancel()
		<-reported // the final outcome report has been sent (bounded by its 5s HTTP timeout)
	}()

	start := time.Now()
	rr := RunResult{StartedAt: start, Kind: "backup"}

	// Stream live state to the server (and pick up commands); also covers
	// pre-flight failures below.
	go func() { defer close(reported); a.reportLoop(runCtx) }()

	cl, err := a.resticClient()
	if err != nil {
		rr.EndedAt = time.Now()
		rr.OK = false
		rr.Message = err.Error()
		a.log.Error("backup failed", "err", err)
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
	tags := cfg.Tags
	if note = trimSpace(strings.ReplaceAll(note, ",", " ")); note != "" { // restic splits tags on commas
		tags = append(append([]string(nil), tags...), restic.TagPin, "note:"+note)
	}
	summary, out, err := cl.Backup(runCtx, cfg.Paths, cfg.Excludes, cfg.Host, tags, hooks)
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
		if summary.PeakRSSKiB > 0 {
			rr.Message += fmt.Sprintf(" (%s, peak memory %s)",
				rr.EndedAt.Sub(rr.StartedAt).Round(time.Second), humanBytes(uint64(summary.PeakRSSKiB)*1024))
		}
		if summary.WarningCount > 0 {
			rr.Warnings = summary.Warnings
			rr.Message += fmt.Sprintf(", %d files could not be read", summary.WarningCount)
		}
	} else {
		rr.Message = "backup completed"
	}
	a.log.Info("backup ok", "msg", rr.Message)
	a.storeRun(rr)
	return nil
}

// Restore restores snapID (optionally only subpath) to target. mode "inplace"
// restores to "/" overwriting originals (if-changed); anything else restores
// under target. Reuses the single-op lane (Kind=restore), so pause/cancel and
// the live panel work exactly as for backups.
func (a *Agent) Restore(ctx context.Context, snapID, subpath, target, mode string) error {
	runCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		cancel()
		return fmt.Errorf("a backup or restore is already running")
	}
	a.running = true
	a.paused = false
	a.kind = "restore"
	a.cancel = cancel
	a.startedAt = time.Now()
	a.pausedAccum = 0
	a.pauseStart = time.Time{}
	a.activity = nil
	a.progress = nil
	a.mu.Unlock()

	reported := make(chan struct{})
	defer func() {
		a.mu.Lock()
		a.running = false
		a.paused = false
		a.kind = ""
		a.progress = nil
		a.proc = nil
		a.cancel = nil
		a.mu.Unlock()
		cancel()
		<-reported // see Backup
	}()
	go func() { defer close(reported); a.reportLoop(runCtx) }()

	overwrite := "always"
	if mode == "inplace" {
		target = "/"
		overwrite = "if-changed"
	}

	rr := RunResult{StartedAt: time.Now(), Kind: "restore"}
	cl, err := a.resticClient()
	if err != nil {
		rr.EndedAt = time.Now()
		rr.Message = err.Error()
		a.storeRun(rr)
		return err
	}

	hooks := restic.RestoreHooks{
		OnProgress: func(p restic.RestoreProgress) {
			a.mu.Lock()
			a.progress = &restic.Progress{
				PercentDone: p.PercentDone, FilesDone: p.FilesRestored, TotalFiles: p.TotalFiles,
				BytesDone: p.BytesRestored, TotalBytes: p.TotalBytes,
			}
			a.mu.Unlock()
		},
		OnActivity: func(action, item string) {
			a.mu.Lock()
			a.appendActivityLocked(action + "  " + item)
			a.mu.Unlock()
		},
		OnStart: func(p *os.Process) { a.mu.Lock(); a.proc = p; a.mu.Unlock() },
	}
	res, out, err := cl.Restore(runCtx, snapID, subpath, target, overwrite, false, hooks)
	rr.EndedAt = time.Now()
	rr.Output = out
	if err != nil {
		rr.OK = false
		if runCtx.Err() == context.Canceled {
			rr.Message = "cancelled"
		} else {
			rr.Message = err.Error()
		}
		a.storeRun(rr)
		return err
	}
	rr.OK = true
	if res != nil {
		rr.Message = fmt.Sprintf("restored %d files, %s to %s", res.FilesRestored, humanBytes(res.BytesRestored), target)
	} else {
		rr.Message = "restore completed"
	}
	a.storeRun(rr)
	return nil
}

// Find searches this host's snapshots for pattern (a file-name glob such as
// "wt-perf" or "*.jsonl") and returns each match's history.
func (a *Agent) Find(ctx context.Context, pattern string) ([]restic.Found, error) {
	cl, err := a.resticClient()
	if err != nil {
		return nil, err
	}
	return cl.FindTimeline(ctx, a.GetConfig().Host, pattern)
}

// RestorePath brings back one file or directory from the newest snapshot that
// contains it (taken before `before`, if set). By default it restores beside
// the original, into "<path>.restored-<timestamp>" (or `to`): a directory's
// contents go straight into that folder, a file goes inside it. inplace
// restores over the original location instead. It returns where the data went.
func (a *Agent) RestorePath(ctx context.Context, p string, before time.Time, to string, inplace bool) (string, error) {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute: %s", p)
	}
	cl, err := a.resticClient()
	if err != nil {
		return "", err
	}
	snap, typ, err := cl.LastContaining(ctx, a.GetConfig().Host, p, before)
	if err != nil {
		return "", err
	}
	a.log.Info("restoring", "path", p, "snapshot", snap.ShortID, "taken", snap.Time.Format(time.RFC3339))
	if inplace {
		return p, a.Restore(ctx, snap.ID, p, "", "inplace")
	}
	if to == "" {
		to = p + ".restored-" + time.Now().Format("20060102-150405")
	}
	// "<id>:<dir>" makes restic treat dir as the root, so the data lands
	// directly in `to` instead of under to/home/user/....
	if typ == "dir" {
		return to, a.Restore(ctx, snap.ID+":"+p, "", to, "")
	}
	return to, a.Restore(ctx, snap.ID+":"+filepath.Dir(p), "/"+filepath.Base(p), to, "")
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

func (a *Agent) storeRun(rr RunResult) {
	a.mu.Lock()
	a.lastRun = rr
	enabled, bin := a.cfg.NotifyEnabled(), a.notifyBin
	a.mu.Unlock()
	if enabled && bin != "" {
		if args, ok := notifyArgs(rr); ok {
			go a.sendNotify(bin, args)
		}
	}
}

// sendNotify runs the configured notify-send binary with args. Errors are
// logged but never returned — a failed desktop notification must never
// affect the backup result.
func (a *Agent) sendNotify(bin string, args []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, bin, args...).Run(); err != nil {
		a.log.Debug("desktop notification failed", "err", err)
	}
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

func (a *Agent) statePath() string { return filepath.Join(filepath.Dir(a.cfgPath), "state.json") }

type agentState struct {
	LastBackupStart time.Time `json:"last_backup_start"`
}

// loadLastStart reads state.json; a missing or unreadable file means "never".
func (a *Agent) loadLastStart() {
	raw, err := os.ReadFile(a.statePath())
	if err != nil {
		return
	}
	var s agentState
	if json.Unmarshal(raw, &s) == nil {
		a.lastStart = s.LastBackupStart
	}
}

// saveLastStart records when a backup started, in memory and in state.json.
// A write failure only costs catch-up after a restart, so it is logged.
func (a *Agent) saveLastStart(t time.Time) {
	a.mu.Lock()
	a.lastStart = t
	a.mu.Unlock()
	raw, _ := json.Marshal(agentState{LastBackupStart: t})
	_ = os.MkdirAll(filepath.Dir(a.statePath()), 0o700)
	tmp := a.statePath() + ".tmp"
	err := os.WriteFile(tmp, raw, 0o600)
	if err == nil {
		err = os.Rename(tmp, a.statePath())
	}
	if err != nil {
		a.log.Warn("could not save agent state", "err", err)
	}
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

// NotifyEnabled reports whether desktop notifications are on (default on).
func (c Config) NotifyEnabled() bool { return c.NotifyDesktop == nil || *c.NotifyDesktop }

// notifyArgs builds notify-send arguments for a finished run, and reports whether
// to send. Real backups only (not restores, not user-cancels); failures — including
// pre-flight — send a critical toast.
func notifyArgs(rr RunResult) (args []string, ok bool) {
	if rr.Kind != "backup" {
		return nil, false
	}
	if !rr.OK && rr.Message == "cancelled" {
		return nil, false
	}
	title, urgency := "Backup complete", "normal"
	if !rr.OK {
		title, urgency = "Backup failed", "critical"
	}
	return []string{"-a", "hoard", "-u", urgency, "hoard: " + title, rr.Message}, true
}
