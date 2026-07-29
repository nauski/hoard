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
	"os/exec"
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

// run executes restic with the given args and returns combined stdout.
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...)
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
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Username string    `json:"username"`
	Paths    []string  `json:"paths"`
	Tags     []string  `json:"tags"`
}

// Snapshots lists snapshots in the repo, newest first.
func (c *Client) Snapshots(ctx context.Context) ([]Snapshot, error) {
	out, err := c.run(ctx, "snapshots", "--json")
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
	cmd := exec.CommandContext(ctx, c.bin, args...)
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

// Backup backs up the given paths to this repo. excludes are restic --exclude
// patterns; host overrides the snapshot hostname; tags are applied to the
// snapshot. It parses the final summary message from restic's JSON output.
func (c *Client) Backup(ctx context.Context, paths, excludes []string, host string, tags []string) (*BackupResult, string, error) {
	if len(paths) == 0 {
		return nil, "", fmt.Errorf("no paths configured to back up")
	}
	args := []string{"backup", "--json"}
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

	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Env = append(cmd.Environ(), c.env()...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	// restic streams one JSON object per line; the last "summary" carries results.
	res := parseBackupSummary(out.String())
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		return res, tail(out.String(), 2000), fmt.Errorf("restic backup: %w: %s", err, msg)
	}
	return res, tail(out.String(), 2000), nil
}

func parseBackupSummary(stdout string) *BackupResult {
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var res *BackupResult
	for sc.Scan() {
		line := sc.Bytes()
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal(line, &probe) != nil || probe.MessageType != "summary" {
			continue
		}
		var s BackupResult
		if json.Unmarshal(line, &s) == nil {
			res = &s
		}
	}
	return res
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
