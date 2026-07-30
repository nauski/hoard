// Package config loads hoard's runtime configuration from a JSON file with
// environment-variable overrides. Secrets (repo passwords, S3 credentials) may
// be supplied via env so they never have to live in the config file on disk.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config is the full runtime configuration for the hoard server.
type Config struct {
	// ListenAddr is the host:port the API + dashboard bind to.
	ListenAddr string `json:"listen_addr"`

	// Hot is the local repository clients push into via the restic REST server.
	Hot Repo `json:"hot"`

	// Cold is the offsite IDrive e2 (S3) repository the hot repo is mirrored to.
	Cold Repo `json:"cold"`

	// Schedule controls when the recurring jobs run.
	Schedule Schedule `json:"schedule"`

	// Retention is applied to the cold repo after each mirror.
	Retention Retention `json:"retention"`

	// Alert is where failure/staleness notifications are sent.
	Alert Alert `json:"alert"`

	// StatePath is where run history is persisted across restarts.
	StatePath string `json:"state_path"`
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
}

// Retention maps to `restic forget` flags applied to the cold repo.
type Retention struct {
	Last    int `json:"keep_last"`
	Daily   int `json:"keep_daily"`
	Weekly  int `json:"keep_weekly"`
	Monthly int `json:"keep_monthly"`
	Yearly  int `json:"keep_yearly"`
}

// Alert configures outbound failure notifications via a generic webhook
// (Discord/Slack/ntfy/etc. — anything that accepts a JSON POST).
type Alert struct {
	WebhookURL string `json:"webhook_url"`
	// OnStale sends an alert when a client repo crosses StaleAfter.
	OnStale bool `json:"on_stale"`
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
	c.applyEnv()
	if err := c.validate(); err != nil {
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
	if c.Cold.Repository == "" {
		c.Cold.Repository = os.Getenv("HOARD_COLD_REPOSITORY")
	}
	if c.Cold.Password == "" {
		c.Cold.Password = os.Getenv("HOARD_COLD_PASSWORD")
	}
	if c.Cold.S3AccessKeyID == "" {
		c.Cold.S3AccessKeyID = os.Getenv("HOARD_COLD_S3_ACCESS_KEY_ID")
	}
	if c.Cold.S3SecretAccessKey == "" {
		c.Cold.S3SecretAccessKey = os.Getenv("HOARD_COLD_S3_SECRET_ACCESS_KEY")
	}
	if c.Alert.WebhookURL == "" {
		c.Alert.WebhookURL = os.Getenv("HOARD_ALERT_WEBHOOK_URL")
	}
}

func (c *Config) validate() error {
	if c.Hot.Repository == "" {
		return fmt.Errorf("hot.repository is required (the local repo clients push to)")
	}
	if c.Cold.Repository == "" {
		return fmt.Errorf("cold.repository is required (the e2/S3 offsite repo)")
	}
	return nil
}
