# Hoard — Backlog

Prioritized list of what to build next. Ordered so each tier builds on components
that already exist (the live-progress/terminal panel, the backup browser, the
`config.Store`, the restic CLI wrapper).

## Current state (for context)

- **Server** (`hoardd`, on TrueNAS): dashboard with live "running backups" (terminal
  + pause/cancel across all clients), clients table with sizes, actions
  (mirror/check), GUI-editable settings incl. offsite storage + GUI repo init,
  backup browser (client → versions → file tree → download / delete /
  delete-from-all-versions), recent jobs, repo totals, light/dark.
- **Agent** (`hoard-agent`, per-user service): settings, folder picker, live backup
  panel (progress/elapsed/ETA/terminal/pause/cancel), backup browser with version
  sizes, auto-refreshing version list, light/dark.
- **Architecture**: hot repo on NAS (restic rest-server) ← clients push; cold repo
  on IDrive e2 (S3) ← `restic copy` mirror on a schedule; retention/prune after
  mirror; webhook alerts. restic-native throughout (no lock-in).

Recommended build order: **restore flow → restore verification → recovery kit →
auto-retry/suspend**. Those four take hoard from "a nice backup uploader" to "a
system I'd bet my data on."

---

## P0 — Trust: make it restore-and-verify, not write-only

The single real gap. Today you can browse and download individual files, but
there is no whole-folder / whole-snapshot restore, and nothing proves the backups
actually come back. For a backup tool this is the point.

- [ ] **Restore flow**
  - **What**: pick a version (or a folder/path within it), pick a target
    destination, and run `restic restore`. Reuse the existing live-progress +
    terminal panel for feedback. Agent restores to a local path; server can
    restore to a share/`/data` path.
  - **Why**: getting data back easily is the entire purpose; right now recovery is
    manual, file-by-file.
  - **Notes**: add `Restore(ctx, snapID, includePath, target, hooks)` to
    `internal/restic/restic.go` (mirror the `Backup` streaming/hooks shape so
    progress/terminal work for free). New handlers `POST /api/restore` on both
    agent and server; new browser action "Restore…". Guard against restoring over
    existing files (restic `--overwrite` policy — default to a safe mode + explicit
    opt-in). Confirm destination has room.

- [ ] **Automated restore verification ("fire drill")**
  - **What**: on a schedule, restore a *sampled* file from the latest snapshot to a
    temp dir, hash it, confirm it matches, then delete the temp copy. Surface a
    headline badge: **"Last verified restore: 2h ago ✓"** (red if overdue/failed).
  - **Why**: `restic check` proves repo *structure*; this proves you can actually
    get *bytes* back. Turns "I hope it works" into a measured metric. Rare in
    self-hosted backup UIs — a real differentiator.
  - **Notes**: scheduler job on the server (it can reach both repos). Pick a random
    file via `restic ls`, `restic dump` to temp, compare. Store result in
    `state.Store` and show on the dashboard header + per-client. Alert on failure.

---

## P1 — Close real risks

- [ ] **Recovery kit (anti–lock-out)**
  - **What**: one-click "Download recovery kit" — a plain file containing the repo
    URL, repo password, and copy-paste `restic restore` instructions. First-run
    gate on storage settings: "I've saved my recovery kit somewhere safe ☑".
  - **Why**: losing the repo password = total, unrecoverable loss — restic's
    catastrophic failure mode. Also a strong trust/no-lock-in story: recover with
    stock restic from any machine even if hoard is gone.
  - **Notes**: server-side generate on demand (creds already in `config.Store`).
    Never email/transmit it — download only. Consider a "password set on <date>,
    kit downloaded?" indicator in Settings.

- [ ] **Auto-retry + suspend awareness**
  - **What**: if a backup run fails, auto-retry once (backoff) before marking it
    failed. Optionally a systemd `sleep.target` hook so suspend cleanly
    pauses/resumes the running backup instead of risking a failed run.
  - **Why**: closes the "did my long overnight backup silently die?" gap. restic
    already resumes via dedup; this makes the agent self-healing without a manual
    re-click.
  - **Notes**: retry logic in `agent.Backup` (don't retry on context-cancel = user
    cancel). Sleep hook: a small `systemd-sleep` script or `inhibit`/signal that
    calls the agent's existing pause/resume.

- [ ] **Bandwidth throttle for the offsite mirror**
  - **What**: editable upload cap for `restic copy` (`--limit-upload`) in server
    Settings.
  - **Why**: a 200 GB first mirror saturates a home uplink for hours; capping (or
    "nights only") keeps the network usable.
  - **Notes**: add to `config.Schedule`/a new bandwidth field; thread into the
    scheduler's copy invocation.

- [ ] **Email alerts (SMTP)**
  - **What**: SMTP alert channel alongside the existing webhook.
  - **Why**: "this client hasn't backed up in 3 days" should actually reach you.
  - **Notes**: extend `config.Alert` + the `Notify` path in `internal/api`.

---

## P2 — UX polish (high value / low cost)

- [ ] **Client freshness chips** — promote "last successful backup: 3h ago" to a
  colored status chip per client (green/amber/red vs `StaleAfter`). Makes the
  dashboard answer "is everything protected?" at a glance.
- [ ] **Version diff** — in the browser, "what changed since the previous version"
  via `restic diff` (added/removed/changed). Makes the version list meaningful
  instead of bare timestamps.
- [ ] **Retention preview** — before saving a retention policy, dry-run and show
  "this would forget N snapshots (these)". No blind destructive changes.
  (`restic forget --dry-run`.)
- [ ] **Dedup/savings stat** — one line: "120 GB logical → 78 GB stored, 35% saved".
  Data is already available from `restic stats` modes.

---

## P3 — Novel ideas

- [x] **Client enrollment tokens** — DONE (server 0.25). Server mints a one-time,
  15-min token (`POST /api/enroll/mint`); a new agent redeems it
  (`hoard-agent -enroll <token> -enroll-server <url>`) to auto-configure repo URL +
  shared password + dashboard URL. Convenience layer on the LAN-trust model (redeem
  returns the shared password, same exposure as the existing open recovery-kit —
  not a boundary against a hostile LAN device). Per-client scoped passwords were
  deferred (would need `restic key` lifecycle).
- [x] **Storage forecast** — DONE (server 0.24). Daily `restic stats` size samples →
  least-squares projection ("Cold: 42 GiB now · +1.8 GiB/week · ~58 GiB by <date>").
  Size only, no cost estimate (pricing drifts).
- [x] **Native desktop notifications** — DONE (agent). `notify-send` toast on backup
  complete/failed via the Nix service (libnotify + session-bus wiring); Settings
  toggle, default on. Quiet "backup complete / failed" toast from
  the per-user agent service, so you don't need to open the GUI.

---

## Notes / conventions

- Everything stays restic-native: prefer wrapping a restic subcommand in
  `internal/restic` over bespoke logic, so the recovery-kit escape hatch always
  holds.
- New long-running operations should reuse `BackupHooks`-style streaming so the
  live-progress + terminal UI works without new plumbing.
- Destructive or irreversible actions get a dry-run/preview or an explicit confirm.
