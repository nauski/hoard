// Package restic is a thin wrapper around the restic CLI. It shells out rather
// than embedding restic-as-a-library on purpose: the CLI is the stable,
// well-tested surface, its --json output is easy to consume, and we never have
// to track restic's internal API. Each call gets its repo credentials via env
// so nothing sensitive lands on argv.
package restic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nauski/hoard/internal/config"
)

// Client runs restic commands against a single repository.
type Client struct {
	bin  string
	repo config.Repo
}

// New returns a Client for the given repo. bin defaults to "restic" if empty.
func New(bin string, repo config.Repo) *Client {
	if bin == "" {
		bin = "restic"
	}
	return &Client{bin: bin, repo: repo}
}

func (c *Client) env() []string {
	env := []string{
		"RESTIC_REPOSITORY=" + c.repo.Repository,
		"RESTIC_PASSWORD=" + c.repo.Password,
	}
	if c.repo.S3AccessKeyID != "" {
		env = append(env,
			"AWS_ACCESS_KEY_ID="+c.repo.S3AccessKeyID,
			"AWS_SECRET_ACCESS_KEY="+c.repo.S3SecretAccessKey,
		)
	}
	return env
}

// globalArgs returns restic persistent flags (bandwidth caps) to prepend before
// the subcommand. Empty when no limits are configured.
func (c *Client) globalArgs() []string {
	var g []string
	if c.repo.LimitUploadKiBps > 0 {
		g = append(g, "--limit-upload", strconv.Itoa(c.repo.LimitUploadKiBps))
	}
	if c.repo.LimitDownloadKiBps > 0 {
		g = append(g, "--limit-download", strconv.Itoa(c.repo.LimitDownloadKiBps))
	}
	return g
}

// run executes restic with the given args and returns combined stdout.
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin, append(c.globalArgs(), args...)...)
	cmd.Env = append(cmd.Environ(), c.env()...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.Bytes(), fmt.Errorf("restic %s: %w: %s", args[0], err, msg)
	}
	return out.Bytes(), nil
}

// Snapshot is a single restic snapshot (subset of fields we surface).
type Snapshot struct {
	ID       string           `json:"id"`
	ShortID  string           `json:"short_id"`
	Time     time.Time        `json:"time"`
	Hostname string           `json:"hostname"`
	Username string           `json:"username"`
	Paths    []string         `json:"paths"`
	Tags     []string         `json:"tags"`
	Summary  *SnapshotSummary `json:"summary,omitempty"`
}

// SnapshotSummary is the backup summary restic embeds in each snapshot (0.17+).
// TotalBytesProcessed is the version's logical size (what was backed up);
// DataAdded is what that backup cost in the repo after deduplication.
type SnapshotSummary struct {
	TotalBytesProcessed uint64 `json:"total_bytes_processed"`
	DataAdded           uint64 `json:"data_added"`
	TotalFilesProcessed int    `json:"total_files_processed"`
}

// Snapshots lists snapshots in the repo, newest first. Uses --no-lock so it
// keeps working while a prune/rewrite holds the exclusive lock (a delete in
// progress must not make the browser read fail and blank the list).
func (c *Client) Snapshots(ctx context.Context) ([]Snapshot, error) {
	out, err := c.run(ctx, "snapshots", "--json", "--no-lock")
	if err != nil {
		return nil, err
	}
	var snaps []Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, fmt.Errorf("parse snapshots: %w", err)
	}
	return snaps, nil
}

// Copy copies all snapshots from src into this (destination) repo. Requires
// restic >= 0.14. Both repos' credentials are passed through env.
func (c *Client) CopyFrom(ctx context.Context, src config.Repo) (string, error) {
	args := []string{
		"copy",
		"--from-repo", src.Repository,
	}
	cmd := exec.CommandContext(ctx, c.bin, append(c.globalArgs(), args...)...)
	env := append(cmd.Environ(), c.env()...)
	env = append(env, "RESTIC_FROM_PASSWORD="+src.Password)
	if src.S3AccessKeyID != "" {
		// Only one repo can use AWS_* env; if the source is also S3 this needs
		// per-repo config. For hot(local)->cold(e2) the source is local, so the
		// destination (this client) owns the AWS_* vars set in env().
		env = append(env, "RESTIC_FROM_REPOSITORY="+src.Repository)
	}
	cmd.Env = env
	out, err := combinedRun(cmd)
	return out, err
}

// BackupResult summarizes a completed backup (parsed from restic --json).
type BackupResult struct {
	SnapshotID      string `json:"snapshot_id"`
	FilesNew        int    `json:"files_new"`
	FilesChanged    int    `json:"files_changed"`
	DataAddedPacked uint64 `json:"data_added_packed"`
	TotalFiles      int    `json:"total_files_processed"`
	TotalBytes      uint64 `json:"total_bytes_processed"`
}

// Progress is a live snapshot of an in-flight backup, parsed from restic's
// --json "status" messages. It carries everything needed to render a progress
// bar with an ETA and the file currently being read.
type Progress struct {
	PercentDone      float64  `json:"percent_done"` // 0..1
	FilesDone        int      `json:"files_done"`
	TotalFiles       int      `json:"total_files"`
	BytesDone        uint64   `json:"bytes_done"`
	TotalBytes       uint64   `json:"total_bytes"`
	SecondsElapsed   int      `json:"seconds_elapsed"`
	SecondsRemaining int      `json:"seconds_remaining"` // restic's ETA
	CurrentFiles     []string `json:"current_files"`
}

// BackupHooks are optional callbacks fired during a backup so callers can show
// live status, stream per-file activity, and control the process.
type BackupHooks struct {
	// OnProgress fires for each "status" message (a few times/sec).
	OnProgress func(Progress)
	// OnActivity fires for each per-file "verbose_status" message with the
	// restic action ("new"/"changed"/"unchanged"/…) and the file path.
	OnActivity func(action, item string)
	// OnStart fires once with the restic process, so the caller can suspend
	// (SIGSTOP/SIGCONT) or otherwise signal it.
	OnStart func(*os.Process)
}

// Backup backs up the given paths to this repo. excludes are restic --exclude
// patterns; host overrides the snapshot hostname; tags are applied to the
// snapshot. --verbose is passed so every file emits a verbose_status event.
// Cancel by cancelling ctx. It parses and returns the final summary.
func (c *Client) Backup(ctx context.Context, paths, excludes []string, host string, tags []string, hooks BackupHooks) (*BackupResult, string, error) {
	if len(paths) == 0 {
		return nil, "", fmt.Errorf("no paths configured to back up")
	}
	args := []string{"backup", "--json", "--verbose"}
	if host != "" {
		args = append(args, "--host", host)
	}
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	for _, e := range excludes {
		args = append(args, "--exclude", e)
	}
	args = append(args, paths...)

	cmd := exec.CommandContext(ctx, c.bin, append(c.globalArgs(), args...)...)
	cmd.Env = append(cmd.Environ(), c.env()...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	if hooks.OnStart != nil && cmd.Process != nil {
		hooks.OnStart(cmd.Process)
	}

	// Stream stdout line by line: "status" → progress, "verbose_status" → the
	// per-file activity feed, "summary" → the result. Only a short tail of raw
	// output is retained for diagnostics.
	var result *BackupResult
	var tailLines []string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		switch probe.MessageType {
		case "status":
			if hooks.OnProgress != nil {
				var p Progress
				if json.Unmarshal(line, &p) == nil {
					hooks.OnProgress(p)
				}
			}
		case "verbose_status":
			if hooks.OnActivity != nil {
				var v struct {
					Action string `json:"action"`
					Item   string `json:"item"`
				}
				if json.Unmarshal(line, &v) == nil && v.Item != "" {
					hooks.OnActivity(v.Action, v.Item)
				}
			}
		case "error":
			if hooks.OnActivity != nil {
				var e struct {
					Item  string `json:"item"`
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal(line, &e) == nil {
					hooks.OnActivity("error", e.Item+": "+e.Error.Message)
				}
			}
			tailLines = appendTail(tailLines, string(line))
		case "summary":
			var s BackupResult
			if json.Unmarshal(line, &s) == nil {
				result = &s
			}
			tailLines = appendTail(tailLines, string(line))
		}
	}

	waitErr := cmd.Wait()
	out := strings.Join(tailLines, "\n")
	if waitErr != nil {
		msg := strings.TrimSpace(errb.String())
		return result, out, fmt.Errorf("restic backup: %w: %s", waitErr, msg)
	}
	return result, out, nil
}

// RestoreProgress mirrors restic restore's --json "status" message.
type RestoreProgress struct {
	PercentDone    float64 `json:"percent_done"`
	FilesRestored  int     `json:"files_restored"`
	TotalFiles     int     `json:"total_files"`
	BytesRestored  uint64  `json:"bytes_restored"`
	TotalBytes     uint64  `json:"total_bytes"`
	SecondsElapsed int     `json:"seconds_elapsed"`
}

// RestoreResult is restic restore's final --json "summary".
type RestoreResult struct {
	TotalFiles    int    `json:"total_files"`
	FilesRestored int    `json:"files_restored"`
	TotalBytes    uint64 `json:"total_bytes"`
	BytesRestored uint64 `json:"bytes_restored"`
}

// RestoreHooks are optional callbacks fired during a restore (same pattern as
// BackupHooks) so callers can show live status and control the process.
type RestoreHooks struct {
	OnProgress func(RestoreProgress)
	OnActivity func(action, item string)
	OnStart    func(*os.Process)
}

// Restore restores snapID (optionally only subpath) into target. subpath, if
// set, is applied via restic's --include, so it matches either a single file
// or a directory subtree (the "id:subpath" spec form only supports
// directories and hard-errors on a file path, so we don't use it). overwrite
// is one of always|if-changed|if-newer|never. If verify is set, restic
// re-reads restored files and checks their content against the repo. Cancel
// via ctx.
func (c *Client) Restore(ctx context.Context, snapID, subpath, target, overwrite string, verify bool, hooks RestoreHooks) (*RestoreResult, string, error) {
	if target == "" {
		return nil, "", fmt.Errorf("no restore target")
	}
	if overwrite == "" {
		overwrite = "always"
	}
	// restic restore needs --verbose=2 (unlike backup, where a single
	// --verbose suffices) to emit per-file verbose_status events.
	args := []string{"restore", snapID, "--target", target, "--overwrite", overwrite, "--json", "--verbose=2"}
	if subpath != "" {
		args = append(args, "--include", subpath)
	}
	if verify {
		args = append(args, "--verify")
	}

	cmd := exec.CommandContext(ctx, c.bin, append(c.globalArgs(), args...)...)
	cmd.Env = append(cmd.Environ(), c.env()...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	if hooks.OnStart != nil && cmd.Process != nil {
		hooks.OnStart(cmd.Process)
	}

	var result *RestoreResult
	var tailLines []string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		switch probe.MessageType {
		case "status":
			if hooks.OnProgress != nil {
				var p RestoreProgress
				if json.Unmarshal(line, &p) == nil {
					hooks.OnProgress(p)
				}
			}
		case "verbose_status":
			if hooks.OnActivity != nil {
				var v struct {
					Action string `json:"action"`
					Item   string `json:"item"`
				}
				if json.Unmarshal(line, &v) == nil && v.Item != "" {
					hooks.OnActivity(v.Action, v.Item)
				}
			}
		case "summary":
			var s RestoreResult
			if json.Unmarshal(line, &s) == nil {
				result = &s
			}
			tailLines = appendTail(tailLines, string(line))
		case "error":
			tailLines = appendTail(tailLines, string(line))
		}
	}

	waitErr := cmd.Wait()
	out := strings.Join(tailLines, "\n")
	if waitErr != nil {
		msg := strings.TrimSpace(errb.String())
		return result, out, fmt.Errorf("restic restore: %w: %s", waitErr, msg)
	}
	return result, out, nil
}

// appendTail keeps the most recent ~20 non-status lines for diagnostics.
func appendTail(lines []string, s string) []string {
	lines = append(lines, s)
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return lines
}

// Check verifies repository integrity. readData also re-reads a subset of pack
// files (slower, stronger); pass "" to skip, or "5%" for a sampled read.
func (c *Client) Check(ctx context.Context, readDataSubset string) (string, error) {
	args := []string{"check"}
	if readDataSubset != "" {
		args = append(args, "--read-data-subset", readDataSubset)
	}
	out, err := c.run(ctx, args...)
	return string(out), err
}

// ForgetPrune applies a retention policy and prunes unreferenced data.
func (c *Client) ForgetPrune(ctx context.Context, r config.Retention) (string, error) {
	args := []string{"forget", "--prune"}
	if r.Last > 0 {
		args = append(args, "--keep-last", itoa(r.Last))
	}
	if r.Daily > 0 {
		args = append(args, "--keep-daily", itoa(r.Daily))
	}
	if r.Weekly > 0 {
		args = append(args, "--keep-weekly", itoa(r.Weekly))
	}
	if r.Monthly > 0 {
		args = append(args, "--keep-monthly", itoa(r.Monthly))
	}
	if r.Yearly > 0 {
		args = append(args, "--keep-yearly", itoa(r.Yearly))
	}
	out, err := c.run(ctx, args...)
	return string(out), err
}

// Stats returns the repo's restore-size stats as raw JSON for the dashboard.
type Stats struct {
	TotalSize      uint64 `json:"total_size"`
	TotalFileCount uint64 `json:"total_file_count"`
	SnapshotsCount int    `json:"snapshots_count"`
}

// Stats returns repository size stats (restore-size mode).
func (c *Client) Stats(ctx context.Context) (*Stats, error) {
	out, err := c.run(ctx, "stats", "--json", "--mode", "raw-data")
	if err != nil {
		return nil, err
	}
	var s Stats
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, fmt.Errorf("parse stats: %w", err)
	}
	return &s, nil
}

// LsEntry is one file or directory inside a snapshot.
type LsEntry struct {
	Name  string    `json:"name"`
	Path  string    `json:"path"`
	Type  string    `json:"type"` // "dir" or "file"
	Size  uint64    `json:"size"`
	MTime time.Time `json:"mtime"`
}

// Ls lists the immediate children of dir within snapshot snapID (one level, not
// recursive). dir defaults to "/". Directories sort before files, then by name.
func (c *Client) Ls(ctx context.Context, snapID, dir string) ([]LsEntry, error) {
	if dir == "" {
		dir = "/"
	}
	dir = path.Clean(dir)
	out, err := c.run(ctx, "ls", snapID, dir, "--json", "--no-lock")
	if err != nil {
		return nil, err
	}
	var entries []LsEntry
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		var n struct {
			MessageType string    `json:"message_type"`
			Name        string    `json:"name"`
			Type        string    `json:"type"`
			Path        string    `json:"path"`
			Size        uint64    `json:"size"`
			MTime       time.Time `json:"mtime"`
		}
		if json.Unmarshal(sc.Bytes(), &n) != nil || n.MessageType != "node" {
			continue
		}
		// Keep only direct children of dir.
		if path.Dir(n.Path) != dir {
			continue
		}
		entries = append(entries, LsEntry{Name: n.Name, Path: n.Path, Type: n.Type, Size: n.Size, MTime: n.MTime})
	}
	sort.Slice(entries, func(i, j int) bool {
		if (entries[i].Type == "dir") != (entries[j].Type == "dir") {
			return entries[i].Type == "dir" // dirs first
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

// ListFiles returns every regular file in snapshot snapID (recursive), with
// path + size. Used by the restore-verification sampler. Uses --no-lock.
func (c *Client) ListFiles(ctx context.Context, snapID string) ([]LsEntry, error) {
	out, err := c.run(ctx, "ls", snapID, "--json", "--no-lock")
	if err != nil {
		return nil, err
	}
	var files []LsEntry
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		var n struct {
			MessageType string    `json:"message_type"`
			Name        string    `json:"name"`
			Type        string    `json:"type"`
			Path        string    `json:"path"`
			Size        uint64    `json:"size"`
			MTime       time.Time `json:"mtime"`
		}
		if json.Unmarshal(sc.Bytes(), &n) != nil || n.MessageType != "node" || n.Type != "file" {
			continue
		}
		files = append(files, LsEntry{Name: n.Name, Path: n.Path, Type: n.Type, Size: n.Size, MTime: n.MTime})
	}
	return files, nil
}

// Dump streams the contents of a single file (filePath) from snapshot snapID to
// w, for downloads. For a directory restic would emit a tar; we only expose it
// for files in the UI.
func (c *Client) Dump(ctx context.Context, snapID, filePath string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, c.bin, append(c.globalArgs(), "dump", snapID, filePath)...)
	cmd.Env = append(cmd.Environ(), c.env()...)
	var errb bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restic dump: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// Prune removes data no longer referenced by any snapshot, reclaiming space.
func (c *Client) Prune(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "prune")
	return string(out), err
}

// RewriteExcludePath removes targetPath (a file or directory, with everything
// under it) from every snapshot of host, rewriting them in place (--forget).
// It does NOT prune; call Prune afterwards to reclaim space. If host is empty
// all snapshots are rewritten.
func (c *Client) RewriteExcludePath(ctx context.Context, host, targetPath string) (string, error) {
	args := []string{"rewrite", "--forget"}
	if host != "" {
		args = append(args, "--host", host)
	}
	// Match the path itself and everything beneath it.
	args = append(args, "--exclude", targetPath, "--exclude", strings.TrimRight(targetPath, "/")+"/**")
	out, err := c.run(ctx, args...)
	return string(out), err
}

// RewriteExcludePathSnap removes targetPath from a single snapshot (by ID),
// rewriting it in place (--forget). Does not prune.
func (c *Client) RewriteExcludePathSnap(ctx context.Context, snapID, targetPath string) (string, error) {
	args := []string{"rewrite", snapID, "--forget",
		"--exclude", targetPath, "--exclude", strings.TrimRight(targetPath, "/") + "/**"}
	out, err := c.run(ctx, args...)
	return string(out), err
}

// ForgetSnapshot deletes a single snapshot by ID. It does not prune.
func (c *Client) ForgetSnapshot(ctx context.Context, snapID string) (string, error) {
	out, err := c.run(ctx, "forget", snapID)
	return string(out), err
}

// Unlock removes stale locks (from processes that died, e.g. a killed backup)
// so a subsequent rewrite/prune can acquire the repository. It uses restic's
// default behavior, which only clears stale locks — a lock held by a live
// process (an in-flight backup) is left alone.
func (c *Client) Unlock(ctx context.Context) error {
	_, err := c.run(ctx, "unlock")
	return err
}

// EnsureInit initializes the repo if it does not yet exist. It is safe to call
// on an already-initialized repo (returns nil).
func (c *Client) EnsureInit(ctx context.Context) error {
	if _, err := c.run(ctx, "cat", "config"); err == nil {
		return nil // already initialized
	}
	_, err := c.run(ctx, "init")
	return err
}

func combinedRun(cmd *exec.Cmd) (string, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	// Return the tail of output so callers can log/store it.
	return tail(buf.String(), 4000), err
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Keep whole trailing lines.
	sc := bufio.NewScanner(strings.NewReader(s[len(s)-n:]))
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) > 1 {
		lines = lines[1:] // drop the first, likely-truncated line
	}
	return strings.Join(lines, "\n")
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }
