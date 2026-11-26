// Package config loads hoard's runtime configuration from a JSON file with
// environment-variable overrides. Secrets (repo passwords, S3 credentials) may
// be supplied via env so they never have to live in the config file on disk.
package config

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Config is the full runtime configuration for the hoard server.
type Config struct {
	// ListenAddr is the host:port the API + dashboard bind to.
	ListenAddr string `json:"listen_addr"`

	// Hot is the local repository clients push into via the restic REST server.
	Hot Repo `json:"hot"`

	// Cold is the legacy single offsite repository. Read for migration into
	// Offsite, never written back.
	Cold Repo `json:"cold,omitzero"`

	// Offsite are the labelled offsite storages hot snapshots are mirrored to.
	// It is always written, as [] when empty: a missing key means a fresh
	// install (env may seed a storage), an empty list means all were removed.
	Offsite []Offsite `json:"offsite"`

	// RetiredOffsite are ids of removed storages, never handed out again.
	RetiredOffsite []string `json:"retired_offsite,omitempty"`

	// Routing maps a host to the storage ids it is mirrored to. A host without
	// an entry goes to every Default storage; an empty list means none.
	Routing map[string][]string `json:"routing,omitempty"`

	// Schedule controls when the recurring jobs run.
	Schedule Schedule `json:"schedule"`

	// Retention is applied to the cold repo after each mirror, and to the hot
	// repo before it when Retention.ApplyToHot is set.
	Retention Retention `json:"retention"`

	// Mirror is the legacy hot-only host list. Read for migration into Routing,
	// never written back.
	Mirror Mirror `json:"mirror,omitzero"`

	// Alert is where failure/staleness notifications are sent.
	Alert Alert `json:"alert"`

	// Anomaly sets when a shrinking backup counts as a mass deletion.
	Anomaly Anomaly `json:"anomaly"`

	// StatePath is where run history is persisted across restarts.
	StatePath string `json:"state_path"`

	// RecoveryKitAck records that the user confirmed saving their recovery kit
	// (dismisses the dashboard reminder). Purely a UI nudge; no security effect.
	RecoveryKitAck bool `json:"recovery_kit_ack"`

	// SMTP is the optional email alert channel.
	SMTP SMTP `json:"smtp"`

	// DashboardPassword protects the dashboard and its API (HTTP Basic auth,
	// any user name). Empty leaves them open to anyone who can reach the port.
	DashboardPassword string `json:"dashboard_password,omitempty"`

	// PrivateRepos is set from HOARD_REST_PRIVATE at every start (it must
	// agree with how the entrypoint launched rest-server, so it is never
	// persisted). When on, each enrolled client gets its own repository and
	// its own login, and cannot reach any other client's backups.
	PrivateRepos bool `json:"-"`

	// Clients are the machines enrolled with their own repository.
	Clients map[string]Client `json:"clients,omitempty"`
}

// Client is one enrolled machine's credentials. Its repository lives at
// ReposDir()/<name> on the server's disk.
type Client struct {
	// Password is the restic password of this client's repository.
	Password string `json:"password"`
	// HTTPPassword is the client's rest-server (and report API) login; the
	// user name is the client name.
	HTTPPassword string `json:"http_password"`
	// Managed is the settings the dashboard wants this client to run with;
	// nil until someone saves them. ManagedVersion counts those saves, so the
	// client can tell a new push from one it already applied.
	Managed        *ManagedSettings `json:"managed,omitempty"`
	ManagedVersion int              `json:"managed_version,omitempty"`
	// ManagedAt is when those settings were last saved. Only backups taken
	// after it are checked against the required paths.
	ManagedAt time.Time `json:"managed_at,omitzero"`
}

// ReposDir is where per-client repositories live: next to the shared hot repo.
func (c *Config) ReposDir() string { return filepath.Join(filepath.Dir(c.Hot.Repository), "repos") }

// HtpasswdPath is the rest-server login file hoardd maintains in private mode.
func (c *Config) HtpasswdPath() string {
	return filepath.Join(filepath.Dir(c.Hot.Repository), "htpasswd")
}

// ClientRepo returns the on-disk repository of an enrolled client.
func (c *Config) ClientRepo(name string) Repo {
	return Repo{Repository: filepath.Join(c.ReposDir(), name), Password: c.Clients[name].Password}
}

var clientName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidClientName reports whether s can be a client name: it becomes a
// directory name and an HTTP user name.
func ValidClientName(s string) bool { return clientName.MatchString(s) }

// Htpasswd renders the rest-server login file for the enrolled clients. The
// {SHA} form needs no dependency; the passwords are long random strings, so
// the lack of a salt costs nothing.
func (c *Config) Htpasswd() string {
	names := make([]string, 0, len(c.Clients))
	for n := range c.Clients {
		names = append(names, n)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, n := range names {
		sum := sha1.Sum([]byte(c.Clients[n].HTTPPassword))
		fmt.Fprintf(&b, "%s:{SHA}%s\n", n, base64.StdEncoding.EncodeToString(sum[:]))
	}
	return b.String()
}

// WriteHtpasswd writes the login file atomically. rest-server picks up
// changes by itself within about half a minute.
func (c *Config) WriteHtpasswd() error {
	p := c.HtpasswdPath()
	if err := os.WriteFile(p+".tmp", []byte(c.Htpasswd()), 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// Repo describes a single restic repository and the credentials to open it.
type Repo struct {
	// Repository is the restic repository URL, e.g. "/data/hot" or
	// "s3:https://<region>.idrivee2.com/<bucket>".
	Repository string `json:"repository"`
	// Password is the restic repository password (RESTIC_PASSWORD).
	Password string `json:"password"`
	// S3AccessKeyID / S3SecretAccessKey authenticate to e2; empty for local repos.
	S3AccessKeyID     string `json:"s3_access_key_id"`
	S3SecretAccessKey string `json:"s3_secret_access_key"`
	// LimitUploadKiBps / LimitDownloadKiBps cap restic transfer speed (KiB/s);
	// 0 = unlimited.
	LimitUploadKiBps   int `json:"limit_upload_kibps"`
	LimitDownloadKiBps int `json:"limit_download_kibps"`
}

// Schedule holds cron-free "daily at HH:MM" times for each recurring job.
// Empty string disables that job.
type Schedule struct {
	// Mirror copies new snapshots hot -> cold (e.g. "02:30").
	Mirror string `json:"mirror"`
	// Check runs `restic check` on the cold repo (e.g. "04:00", typically weekly-ish).
	Check string `json:"check"`
	// Verify runs the restore fire-drill at this "HH:MM" (empty = disabled).
	Verify string `json:"verify"`
	// CheckWeekday, if set (0=Sun..6=Sat), restricts Check to that weekday.
	CheckWeekday *int `json:"check_weekday"`
	// StaleAfter marks a client repo stale if no new snapshot within this window.
	StaleAfter Duration `json:"stale_after"`
	// StaleAfterHosts overrides StaleAfter per hostname (e.g. an hourly host: "3h").
	StaleAfterHosts map[string]Duration `json:"stale_after_hosts,omitempty"`
}

// StaleFor returns the staleness window for host.
func (s Schedule) StaleFor(host string) time.Duration {
	if d, ok := s.StaleAfterHosts[host]; ok {
		return d.Std()
	}
	return s.StaleAfter.Std()
}

// Mirror is the legacy mirror section, kept only so old configs migrate.
type Mirror struct {
	ExcludeHosts []string `json:"exclude_hosts,omitempty"`
}

// Retention maps to `restic forget` flags. It is applied per host: Hosts
// overrides the top-level policy for the named hostnames.
type Retention struct {
	Last    int `json:"keep_last"`
	Hourly  int `json:"keep_hourly"`
	Daily   int `json:"keep_daily"`
	Weekly  int `json:"keep_weekly"`
	Monthly int `json:"keep_monthly"`
	Yearly  int `json:"keep_yearly"`
	// Within keeps every snapshot newer than this restic duration ("48h", "7d",
	// "1y6m"), relative to the host's latest snapshot.
	Within string `json:"keep_within,omitempty"`
	// ApplyToHot also applies retention (forget + prune) to the hot repo before
	// each mirror. Off by default: the hot repo then keeps every snapshot.
	// Only read on the top-level policy.
	ApplyToHot bool `json:"apply_to_hot"`
	// Hosts holds per-hostname policies that replace the top-level one. Only
	// read on the top-level policy.
	Hosts map[string]Retention `json:"hosts,omitempty"`
}

// For returns the policy that applies to host.
func (r Retention) For(host string) Retention {
	if h, ok := r.Hosts[host]; ok {
		return h
	}
	return r
}

var resticDuration = regexp.MustCompile(`^(\d+[ymdh])+$`)

// Validate checks the fields restic would otherwise reject mid-job.
func (r Retention) Validate() error {
	if r.Within != "" && !resticDuration.MatchString(r.Within) {
		return fmt.Errorf("keep_within %q is not a restic duration like 48h, 7d or 1y6m", r.Within)
	}
	for h, p := range r.Hosts {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("retention for host %s: %w", h, err)
		}
	}
	return nil
}

// Anomaly flags a backup whose file count fell sharply against the previous
// snapshot of the same host (a bad rm -rf, a sync wiping a tree, an unmounted
// disk). The previous snapshot is then held so retention cannot drop it.
type Anomaly struct {
	// Files triggers when at least this many files vanished (0 = off).
	Files int `json:"files"`
	// Percent triggers when at least this share of the files vanished (0 = off).
	Percent int `json:"percent"`
	// Hosts holds per-hostname thresholds that replace the top-level ones, e.g.
	// a low file count for a host where losing one worktree (a few thousand
	// files out of a million) must already alert. Only read on the top level.
	Hosts map[string]Anomaly `json:"hosts,omitempty"`
}

// For returns the thresholds that apply to host.
func (a Anomaly) For(host string) Anomaly {
	if h, ok := a.Hosts[host]; ok {
		return h
	}
	return a
}

// Triggered reports whether going from before to after files is an anomaly.
// It compares net counts, so a rename of a large tree does not trigger.
func (a Anomaly) Triggered(before, after int) bool {
	drop := before - after
	if drop <= 0 {
		return false
	}
	return (a.Files > 0 && drop >= a.Files) || (a.Percent > 0 && drop*100 >= a.Percent*before)
}

// Alert configures outbound failure notifications via a generic webhook
// (Discord/Slack/ntfy/etc. — anything that accepts a JSON POST).
type Alert struct {
	WebhookURL string `json:"webhook_url"`
	// OnStale sends an alert when a client repo crosses StaleAfter.
	OnStale bool `json:"on_stale"`
	// OnFailure sends an alert when a client's backups fail repeatedly.
	OnFailure bool `json:"on_failure"`
	// FailureThreshold is the number of consecutive failed backups before alerting.
	FailureThreshold int `json:"failure_threshold"`
}

// SMTP configures the optional email alert channel. Password is an app password
// (a secret); it is persisted to the 0600 config file and shown write-only in the
// UI. Empty Host/From/To disables email.
type SMTP struct {
	Host     string `json:"host"` // e.g. smtp.gmail.com
	Port     string `json:"port"` // e.g. 587 (blank -> 587)
	Username string `json:"username"`
	Password string `json:"password"` // app password (secret)
	From     string `json:"from"`
	To       string `json:"to"`
}

// Duration is a JSON-friendly time.Duration ("24h", "30m").
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Std returns the standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Load reads config from path, applies env overrides, and fills defaults.
func Load(path string) (*Config, error) {
	c := &Config{
		ListenAddr: ":8080",
		StatePath:  "/data/hoard-state.json",
		Schedule:   Schedule{StaleAfter: Duration(26 * time.Hour)},
		Retention:  Retention{Daily: 7, Weekly: 4, Monthly: 6},
		Alert:      Alert{OnFailure: true, FailureThreshold: 3},
		Anomaly:    Anomaly{Files: 10000, Percent: 50},
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if c.Offsite == nil && c.Cold.Repository == "" && c.Cold != (Repo{}) {
		c.Cold.Repository = os.Getenv("HOARD_COLD_REPOSITORY") // keep the file's other cold fields
	}
	c.migrateOffsite()
	c.applyEnv()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// applyEnv SEEDS empty fields from the environment (a fresh TrueNAS deploy can
// supply the initial secrets / storage config there) but does not override
// values already present in the config file. The config file — editable from
// the Settings panel — is the source of truth, so a change made in the GUI
// isn't reverted on restart. ListenAddr always follows env if set (it's a
// deploy concern, not a GUI setting).
func (c *Config) applyEnv() {
	if v := os.Getenv("HOARD_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if c.Hot.Password == "" {
		c.Hot.Password = os.Getenv("HOARD_HOT_PASSWORD")
	}
	if c.Offsite == nil {
		// Key absent from the file: brand-new env-only deployment, seed the
		// first storage. (A saved empty list is non-nil and stays empty.)
		if repo := os.Getenv("HOARD_COLD_REPOSITORY"); repo != "" {
			o := Offsite{Label: "Offsite", Default: true, Repo: Repo{Repository: repo}}
			o.ID = NewOffsiteID(o.Label, nil)
			if strings.Contains(repo, "idrivee2") {
				o.ID, o.Label = "e2", "IDrive e2"
			}
			c.Offsite = []Offsite{o}
		}
	}
	// The secrets belong to the legacy e2 storage or the one seeded above.
	for i := range c.Offsite {
		o := &c.Offsite[i]
		if o.ID != "e2" && o.ID != "offsite" {
			continue
		}
		if o.Password == "" {
			o.Password = os.Getenv("HOARD_COLD_PASSWORD")
		}
		if o.S3AccessKeyID == "" {
			o.S3AccessKeyID = os.Getenv("HOARD_COLD_S3_ACCESS_KEY_ID")
		}
		if o.S3SecretAccessKey == "" {
			o.S3SecretAccessKey = os.Getenv("HOARD_COLD_S3_SECRET_ACCESS_KEY")
		}
	}
	if c.Alert.WebhookURL == "" {
		c.Alert.WebhookURL = os.Getenv("HOARD_ALERT_WEBHOOK_URL")
	}
	if c.DashboardPassword == "" {
		c.DashboardPassword = os.Getenv("HOARD_DASHBOARD_PASSWORD")
	}
	c.PrivateRepos = os.Getenv("HOARD_REST_PRIVATE") == "true"
}

// Validate checks the config for the errors that would stop Load.
func (c *Config) Validate() error {
	if c.Hot.Repository == "" {
		return fmt.Errorf("hot.repository is required (the local repo clients push to)")
	}
	ids, labels := map[string]bool{}, map[string]bool{}
	for _, o := range c.Offsite {
		if o.ID == "" || ids[o.ID] {
			return fmt.Errorf("offsite storage id %q is empty or duplicated", o.ID)
		}
		if o.Label == "" || labels[o.Label] {
			return fmt.Errorf("offsite storage label %q is empty or duplicated", o.Label)
		}
		ids[o.ID], labels[o.Label] = true, true
	}
	return nil
}
