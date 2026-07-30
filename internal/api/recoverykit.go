package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nauski/hoard/internal/config"
)

// resticVersion is the restic release bundled in the hoard image; the recovery
// kit tells the user which stock restic to fetch for a from-scratch restore.
const resticVersion = "0.18.0"

// buildRecoveryKit renders a plain-text, download-only recovery kit from the
// current config. It deliberately includes the repo passwords and S3 secret in
// cleartext — the whole point is to let the user restore with stock restic even
// if hoard (or the NAS) is gone. Pure function so it is unit-testable.
func buildRecoveryKit(c *config.Config, resticVer string, now time.Time) string {
	var b strings.Builder
	val := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "— not configured —"
		}
		return s
	}

	fmt.Fprintf(&b, "HOARD RECOVERY KIT\n")
	fmt.Fprintf(&b, "generated: %s\n\n", now.Format(time.RFC1123))
	b.WriteString("!! SENSITIVE — contains your repository passwords and S3 secret in\n")
	b.WriteString("!! cleartext. Store it in a password manager. Never share it.\n")
	b.WriteString("!! With these, anyone can read (or delete) your backups.\n\n")
	b.WriteString("These are stock-restic instructions: they need no hoard, just the\n")
	fmt.Fprintf(&b, "restic %s binary (https://github.com/restic/restic/releases).\n\n", resticVer)

	b.WriteString("==================================================================\n")
	b.WriteString("PRIMARY — OFFSITE (cold / S3). Use this if the NAS is gone.\n")
	b.WriteString("==================================================================\n")
	fmt.Fprintf(&b, "Repository          : %s\n", val(c.Cold.Repository))
	fmt.Fprintf(&b, "Repo password       : %s\n", val(c.Cold.Password))
	fmt.Fprintf(&b, "S3 Access Key ID    : %s\n", val(c.Cold.S3AccessKeyID))
	fmt.Fprintf(&b, "S3 Secret Access Key: %s\n\n", val(c.Cold.S3SecretAccessKey))
	b.WriteString("Restore (paste into a shell with restic installed):\n")
	if strings.TrimSpace(c.Cold.Repository) == "" {
		b.WriteString("  (offsite repo not configured — nothing to restore from here)\n\n")
	} else {
		fmt.Fprintf(&b, "  export RESTIC_REPOSITORY='%s'\n", c.Cold.Repository)
		fmt.Fprintf(&b, "  export RESTIC_PASSWORD='%s'\n", c.Cold.Password)
		fmt.Fprintf(&b, "  export AWS_ACCESS_KEY_ID='%s'\n", c.Cold.S3AccessKeyID)
		fmt.Fprintf(&b, "  export AWS_SECRET_ACCESS_KEY='%s'\n", c.Cold.S3SecretAccessKey)
		b.WriteString("  restic snapshots                      # list what's there\n")
		b.WriteString("  restic restore latest --target ./restored\n\n")
	}

	b.WriteString("==================================================================\n")
	b.WriteString("SECONDARY — LOCAL (hot / NAS rest-server). Only if the NAS dataset\n")
	b.WriteString("survived; normally you recover from the offsite copy above.\n")
	b.WriteString("==================================================================\n")
	fmt.Fprintf(&b, "Repository          : %s\n", val(c.Hot.Repository))
	fmt.Fprintf(&b, "Repo password       : %s\n\n", val(c.Hot.Password))
	b.WriteString("Restore:\n")
	if strings.TrimSpace(c.Hot.Repository) == "" {
		b.WriteString("  (local repo not configured — nothing to restore from here)\n")
	} else {
		fmt.Fprintf(&b, "  export RESTIC_REPOSITORY='%s'\n", c.Hot.Repository)
		fmt.Fprintf(&b, "  export RESTIC_PASSWORD='%s'\n", c.Hot.Password)
		b.WriteString("  restic snapshots\n")
		b.WriteString("  restic restore latest --target ./restored\n")
	}

	return b.String()
}

// handleRecoveryKit streams the recovery kit as a download. The body is NEVER
// logged (it contains secrets), and nothing is accepted via query string.
func (s *Server) handleRecoveryKit(w http.ResponseWriter, r *http.Request) {
	kit := buildRecoveryKit(s.cfg.Load(), resticVersion, time.Now())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="hoard-recovery-kit.txt"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(kit))
}

// handleAckKit records that the user saved their recovery kit, dismissing the
// dashboard reminder. Idempotent.
func (s *Server) handleAckKit(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Update(func(c *config.Config) { c.RecoveryKitAck = true }); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.configView())
}
