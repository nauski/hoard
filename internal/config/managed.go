package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"time"
)

// ManagedSettings is everything the server dashboard may set on a client that
// has opted in to remote management. It is deliberately a short list: what to
// back up, when, and how gently. Where backups go (repository, password file,
// server address, host name) is never part of it, so a push from the server
// cannot redirect a client's backups or make it trust another server.
type ManagedSettings struct {
	Paths             []string `json:"paths"`
	Excludes          []string `json:"excludes"`
	ExcludeFiles      []string `json:"exclude_files"`
	ExcludeIfPresent  []string `json:"exclude_if_present"`
	ExcludeLargerThan string   `json:"exclude_larger_than"`
	ExcludeCaches     bool     `json:"exclude_caches"`
	Schedule          string   `json:"schedule"`
	NoScan            bool     `json:"no_scan"`
	ReadConcurrency   int      `json:"read_concurrency"`
	Nice              int      `json:"nice"`
	IONiceClass       string   `json:"ionice_class"`
}

// Validate rejects settings an agent could not run with.
func (m ManagedSettings) Validate() error {
	for _, p := range m.Paths {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("path %q is not absolute", p)
		}
	}
	if m.Schedule != "" {
		d, derr := time.ParseDuration(m.Schedule)
		_, terr := time.Parse("15:04", m.Schedule)
		if (derr != nil && terr != nil) || (derr == nil && d < time.Minute) {
			return fmt.Errorf("schedule %q is neither HH:MM nor an interval of at least 1m", m.Schedule)
		}
	}
	if m.IONiceClass != "" && m.IONiceClass != "idle" && m.IONiceClass != "best-effort" {
		return fmt.Errorf("ionice_class %q must be idle or best-effort", m.IONiceClass)
	}
	if m.Nice < 0 || m.Nice > 19 || m.ReadConcurrency < 0 {
		return fmt.Errorf("nice must be 0-19 and read_concurrency must not be negative")
	}
	return nil
}

// SignManaged authenticates a settings push from server to client. The key is
// the client's repository password: both sides hold it, and after enrollment
// it never crosses the network, so someone who can read or alter traffic on
// the LAN still cannot forge a push. host and version are bound in, so a push
// cannot be replayed to another client or re-sent after a newer one.
func SignManaged(key, host string, version int, settingsJSON string) string {
	mac := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(mac, "hoard-managed-v1\n%s\n%d\n%s", host, version, settingsJSON)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyManaged checks a push's signature in constant time.
func VerifyManaged(key, host string, version int, settingsJSON, mac string) bool {
	want := SignManaged(key, host, version, settingsJSON)
	return key != "" && hmac.Equal([]byte(want), []byte(mac))
}
