package api

import (
	"fmt"
	"net/http"
	"slices"
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

	if len(c.Offsite) == 0 {
		b.WriteString("==================================================================\n")
		b.WriteString("PRIMARY — OFFSITE. Use this if the NAS is gone.\n")
		b.WriteString("==================================================================\n")
		b.WriteString("  (offsite storage not configured — nothing to restore from here)\n\n")
	}
	for _, o := range c.Offsite {
		b.WriteString("==================================================================\n")
		fmt.Fprintf(&b, "PRIMARY — OFFSITE: %s. Use this if the NAS is gone.\n", o.Label)
		b.WriteString("==================================================================\n")
		fmt.Fprintf(&b, "Repository          : %s\n", val(o.Repository))
		fmt.Fprintf(&b, "Repo password       : %s\n", val(o.Password))
		if o.S3AccessKeyID != "" || o.S3SecretAccessKey != "" {
			fmt.Fprintf(&b, "S3 Access Key ID    : %s\n", val(o.S3AccessKeyID))
			fmt.Fprintf(&b, "S3 Secret Access Key: %s\n", val(o.S3SecretAccessKey))
		}
		b.WriteString("\nRestore (paste into a shell with restic installed):\n")
		if strings.TrimSpace(o.Repository) == "" {
			b.WriteString("  (repository not configured — nothing to restore from here)\n\n")
			continue
		}
		fmt.Fprintf(&b, "  export RESTIC_PASSWORD='%s'\n", o.Password)
		if o.S3AccessKeyID != "" || o.S3SecretAccessKey != "" {
			fmt.Fprintf(&b, "  export AWS_ACCESS_KEY_ID='%s'\n", o.S3AccessKeyID)
			fmt.Fprintf(&b, "  export AWS_SECRET_ACCESS_KEY='%s'\n", o.S3SecretAccessKey)
		}
		fmt.Fprintf(&b, "  restic -r '%s' snapshots              # list what's there\n", o.Repository)
		fmt.Fprintf(&b, "  restic -r '%s' restore latest --target ./restored\n\n", o.Repository)
	}
	for _, o := range c.Offsite {
		hosts := hostsFor(c, o.ID)
		line := "none listed"
		if len(hosts) > 0 {
			line = strings.Join(hosts, ", ")
		}
		if o.Default {
			line += " (and every other host that follows the defaults)"
		}
		fmt.Fprintf(&b, "Hosts copied to %s: %s\n", o.Label, line)
	}
	if len(c.Offsite) > 0 {
		b.WriteString("\n")
	}
	hotOnly := hotOnlyHosts(c)
	if len(c.Offsite) > 0 && !slices.ContainsFunc(c.Offsite, func(o config.Offsite) bool { return o.Default }) {
		b.WriteString("No storage is a default, so a host without its own routing is not copied offsite either.\n")
	}
	if len(hotOnly) > 0 {
		fmt.Fprintf(&b, "NOT OFFSITE: these hosts have no offsite copy and are never mirrored,\nso they can only be restored from the local repo below: %s\n\n",
			strings.Join(hotOnly, ", "))
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

	if len(c.Clients) > 0 {
		b.WriteString("\nPER-CLIENT REPOSITORIES (each enrolled machine has its own repo on the\nNAS, with its own password; see Hosts copied to above for which\noffsite storages hold which host, and NOT OFFSITE for those with none):\n\n")
		for _, n := range clientNames(c) {
			fmt.Fprintf(&b, "  %-20s %s\n  %-20s password: %s\n", n, c.ClientRepo(n).Repository, "", c.Clients[n].Password)
		}
	}

	return b.String()
}

// knownHosts are the hosts the config names: enrolled clients and routed hosts.
func knownHosts(c *config.Config) []string {
	hosts := clientNames(c)
	for h := range c.Routing {
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	slices.Sort(hosts)
	return hosts
}

// hostsFor lists the known hosts copied to storage id (the server's routing;
// a client's own extra choices are not in the config).
func hostsFor(c *config.Config, id string) []string {
	var out []string
	for _, h := range knownHosts(c) {
		if slices.Contains(c.ServerOffsiteFor(h), id) {
			out = append(out, h)
		}
	}
	return out
}

// hotOnlyHosts lists the known hosts that no offsite storage receives.
func hotOnlyHosts(c *config.Config) []string {
	var out []string
	for _, h := range knownHosts(c) {
		if len(c.ServerOffsiteFor(h)) == 0 {
			out = append(out, h)
		}
	}
	return out
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
