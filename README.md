# hoard

A small, self-hosted **central backup system** that fixes the things IDrive's
Linux client gets wrong: no API, no dashboard, silent failure, and no way to see
or clean up what you've backed up. hoard is two tiny Go binaries — a **server**
that lives next to your storage and an **agent** that runs on each machine — both
with an embedded web UI, sitting in front of [restic](https://restic.net) and
[IDrive e2](https://www.idrive.com/e2/) (S3-compatible object storage).

Machines back up to the server; the server consolidates everything into a local
(**hot**) repository, mirrors it offsite to IDrive e2 (**cold**) on a schedule,
verifies both integrity **and that backups actually restore**, enforces retention,
lets you **browse, restore, and clean up** what's stored, and **alerts you — by
webhook or email — the moment a client goes stale or a job fails** — so a backup
can never quietly rot for months.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/hoard-server-dashboard-dark.png">
  <img alt="Server dashboard" src="docs/images/hoard-server-dashboard-light.png">
</picture>

> The screenshots below follow your GitHub theme — dark screenshots in dark
> mode, light in light mode. Both dashboards have the same 🌙/☀️ toggle built in.

> **Why not just use the IDrive client?** Classic IDrive personal backup has no
> public API — the only interface is their proprietary Perl/Python `.bin`, the
> fragile, un-automatable thing this replaces. IDrive **e2** exposes a standard
> S3 endpoint, so we drive it with restic (encryption, dedup, and `restic check`
> integrity verification) instead of reverse-engineering a proprietary protocol
> whose server side can change without notice.

## Architecture

```
  ┌─ hoard-agent (desktop) ─┐        ┌───────────── hoard server (TrueNAS app) ─────────────┐
  │  web GUI: pick paths,   │        │  rest-server  →  HOT repo on pool (/data/hot)         │
  │  schedule, browse,      │─push──▶│        │                                              │
  │  live progress          │        │   hoardd:  API + dashboard + scheduler + browser      │
  └─────────────────────────┘        │        │                                              │
  ┌─ hoard-agent (laptop) ──┐        │        └── restic copy ─▶  COLD repo on IDrive e2 (S3) │
  │  … or plain restic      │─push──▶│                                                       │
  └─────────────────────────┘        └───────────────────────────────────────────────────────┘
```

- **Hot repo** — a restic repository on your NAS pool. `rest-server` (bundled)
  gives clients an HTTP push target: `rest:http://server:8000/hot`.
- **Cold repo** — a second, independent restic repo in an IDrive e2 bucket. The
  server runs `restic copy` hot→cold on a schedule; both repos are separately
  `restic check`-able, giving you two verifiable tiers (a 3-2-1-shaped setup).
- **Server (`hoardd`)** — serves the API + dashboard, runs the scheduler
  (mirror / check / prune / freshness), the backup browser, and alerts.
- **Agent (`hoard-agent`)** — per-machine web GUI to choose what to back up, run
  backups (with live progress), and browse/restore/clean up this machine's
  backups. Delegates deletions to the server (the only side that can reach e2).

## Components

| Part | What it is |
|------|------------|
| `hoardd` | Server binary: API, dashboard, scheduler, backup browser, alerts. |
| `hoard-agent` | Client binary: web GUI to select paths, back up, and browse/clean up. |
| `rest-server` | Upstream restic REST server; the client push target. Bundled in the server image. |
| `restic` | The backup engine. Bundled; also what the agent shells out to. |

## Features

- **Two web dashboards** — a tabbed server dashboard for the whole fleet and a
  per-machine agent GUI, each with contextual **ⓘ help tooltips** on the jargon.
  No config files to hand-edit for day-to-day use.
- **Restore, not just download** — restore a whole snapshot, a chosen folder, or a
  single file to a destination you pick (or in place), with the same live progress
  + terminal feed as a backup. Getting data *back* is the whole point.
- **Verified restores ("fire drill")** — on a schedule the server restores a
  *sampled* file, hashes it, and confirms the bytes actually come back; a headline
  badge shows how long ago that last succeeded. `restic check` proves structure —
  this proves recovery.
- **Recovery kit** — one-click download of a plain-text file with your repo URLs,
  passwords, and copy-paste `restic restore` commands, so you can recover with
  stock restic even if hoard is gone. A first-run gate nudges you to save it.
- **Live running-backup view** — progress bar, files/bytes, elapsed, ETA, and a
  terminal feed of every file as it's processed, **aggregated across all clients**
  on the server.
- **Live server jobs + cancel** — the offsite mirror, integrity check, and prune
  each show a real packs-based progress bar, elapsed, ETA, a streaming terminal,
  and a **Cancel** button (a cancelled copy is safe — restic copy is resumable).
- **Pause, resume, cancel** any running client backup from the agent *or* the
  server, even though agents bind localhost-only (control rides the client's
  status reports back to it).
- **Graphical folder picker** — choose what to back up by browsing, not by typing
  absolute paths.
- **Backup browser** — client → version → file tree → download, restore, or delete;
  **diff** any version against the previous to see exactly what changed.
- **Delete that actually frees space** — remove a file from one version or from
  **all versions**, across *both* hot and e2, with a type-to-confirm guard — and a
  **retention preview** that dry-runs exactly which snapshots a policy would forget
  before you apply it.
- **Staleness & failure alerts** — webhook **or email (SMTP)**, when a client goes
  stale *or* its backups fail N times in a row (configurable threshold), plus a
  manual retry.
- **Bandwidth caps** — throttle transfer speed (KiB/s) on both the agent's push and
  the server's offsite mirror, so a first sync doesn't saturate your uplink.
- **Storage forecast** — projects where your repo size is heading from its recent
  growth trend ("~58 GiB by Oct 29").
- **Savings & freshness at a glance** — "120 GB logical → 78 GB stored, 35% saved",
  and per-client green/amber/red freshness chips.
- **One-command client enrollment** — the server mints a short-lived, single-use
  token; `hoard-agent -enroll <token> -enroll-server <url>` auto-configures a new
  machine (repo URL + password), no hand-managed secrets.
- **Desktop notifications** — the agent pops a native toast when a backup completes
  or fails, so you don't have to open the GUI.
- **Scheduled mirror / retention / integrity check** — nightly offsite copy,
  `restic forget --prune`, and periodic `restic check` with data re-reads.

## The backup browser

Open the server dashboard, pick a **client**, then a **version**, and walk its
file tree. Download any file, **restore** a file/folder/whole snapshot to a chosen
destination, **diff** a version against the previous one, or delete a file — from
just that version, or from **every** version to reclaim the space for good.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/hoard-server-browser-dark.png">
  <img alt="Backup browser" src="docs/images/hoard-server-browser-light.png">
</picture>

Because your data lives in two repos, deleting "from all versions" runs
`restic rewrite --exclude` + `prune` on **both** the hot repo and e2 — otherwise
the next mirror would copy the file straight back, or e2 would keep holding it.
The irreversible option makes you type the filename to confirm:

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/hoard-delete-modal-dark.png">
  <img alt="Delete from all versions" src="docs/images/hoard-delete-modal-light.png">
</picture>

## The agent

Each machine runs `hoard-agent`, a local web GUI (default `http://127.0.0.1:7420`).
Pick folders with the browser, set a daily time (`21:00`) or an interval (`1h`; a
run missed while the machine was asleep or off starts as soon as it is back), hit **Back up now**, and watch
live progress. The same backup browser is here too, scoped to this machine — and
its deletes are delegated to the server so they hit e2 as well.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/hoard-agent-dashboard-dark.png">
  <img alt="Agent dashboard" src="docs/images/hoard-agent-dashboard-light.png">
</picture>

## Watching & controlling live backups

While a backup runs, the agent shows a live panel: progress with a real elapsed
time and ETA, **pause / resume / cancel** controls, and a terminal-style feed of
every file being processed (colour-coded new / changed / unchanged).

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/hoard-running-agent-dark.png">
  <img alt="Agent running a backup" src="docs/images/hoard-running-agent-light.png">
</picture>

The server rolls this up for the **whole fleet** — one panel per client currently
backing up, each with the same live feed and controls. Pause or cancel a remote
client's backup right from here. Since agents bind localhost-only, control travels
back on the status reports each agent already sends the server every second.

The server's **own** jobs get the same visibility: kick off a mirror to e2 or an
integrity check and you get a live **packs-based progress bar, elapsed, ETA, a
streaming restic terminal, and a Cancel button** — no more staring at a bare
spinner wondering whether a 30 GB offsite copy is halfway done or stuck.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/hoard-running-server-dark.png">
  <img alt="Server watching all clients" src="docs/images/hoard-running-server-light.png">
</picture>

## Quick start — server (Docker / TrueNAS)

1. **Create an IDrive e2 bucket** and an access key. Note the S3 endpoint, e.g.
   `s3:https://s3.<region>.idrivee2.com/<bucket>`.

2. **Write `config.json`** (see `config.example.json`). Keep secrets out of the
   file and pass them as env vars instead.

3. **Run it** (compose shown; on TrueNAS use the *Custom App* form / *Install via YAML*):

   ```yaml
   services:
     hoard:
       image: nauski/hoard:latest      # or build: .
       restart: unless-stopped
       ports:
         - "8080:8080"                 # dashboard + API
         - "8000:8000"                 # restic push target for clients
       volumes:
         - /mnt/pool/backups/hoard:/data
       environment:
         HOARD_HOT_PASSWORD: "your-hot-repo-password"
         HOARD_COLD_REPOSITORY: "s3:https://s3.<region>.idrivee2.com/your-bucket"
         HOARD_COLD_PASSWORD: "your-cold-repo-password"
         HOARD_COLD_S3_ACCESS_KEY_ID: "e2-access-key-id"
         HOARD_COLD_S3_SECRET_ACCESS_KEY: "e2-secret-key"
         HOARD_ALERT_WEBHOOK_URL: "https://ntfy.sh/your-topic"
   ```

   Mount your `config.json` at `/data/hoard.json` (or rely on env + defaults).

4. **Initialize the repos once** (creates hot + cold if missing):

   ```sh
   docker exec -it hoard hoardd -config /data/hoard.json -init
   ```

5. **Open the dashboard** at `http://server:8080`.

## Quick start — client

**Option A: the agent (recommended).** Run `hoard-agent`, point it at the server,
and use the GUI. On NixOS, this repo ships a home-manager module:

```nix
# flake inputs: hoard.url = "git+ssh://…/hoard.git";  (or github:…)
# home.nix imports: inputs.hoard.homeManagerModules.hoard-agent
services.hoard-agent = {
  enable = true;
  repository   = "rest:http://server:8000/hot";
  passwordFile = "/run/secrets/hoard_hot_password";  # from sops-nix / agenix
};
```

The GUI opens at `http://127.0.0.1:7420`; the server URL and password are pinned
declaratively, while paths/excludes/schedule live in the GUI.

> **One-command enrollment (no sops dance).** On the server dashboard, click
> **Generate enrollment token**, then on the new machine run the command it shows:
> `hoard-agent -enroll <token> -enroll-server http://server:8080`. The agent
> redeems the single-use, 15-minute token, writes its repo URL + password, and is
> ready to back up — handy for a quick or non-declarative host. (Convenience on a
> trusted LAN, not an auth boundary — see the security note.)

**Option B: plain restic.** The agent is optional — any restic works:

```sh
export RESTIC_REPOSITORY="rest:http://server:8000/hot"
export RESTIC_PASSWORD="your-hot-repo-password"
restic backup /home/me --host "$(hostname)"
```

**Option C: headless server (systemd timer).** On a machine with no desktop,
enroll once, list what to back up, and let a timer run one-shot backups. They
report to the server like the GUI agent, so live progress, pause and cancel
work from the dashboard.

```sh
hoard-agent -enroll <token> -enroll-server http://server:8080
$EDITOR ~/.config/hoard-agent/config.json     # set "paths" (and "excludes")
hoard-agent backup                            # first run, by hand
cp contrib/systemd/hoard-agent-backup.* ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now hoard-agent-backup.timer
sudo loginctl enable-linger "$USER"
```

Also from the shell, using the same saved config:

```sh
hoard-agent backup -pin "before worktree cleanup"   # keep this snapshot past retention
hoard-agent find wt-perf                            # which backups have it, when it vanished
hoard-agent restore -path ~/non-work/wt-perf        # newest backup that has it -> wt-perf.restored-<time>
hoard-agent restore -path ~/notes.md -before "2026-11-14 14:00" -to /tmp/out
```

`find` matches file names (`wt-perf`, `*.jsonl`) across every backup of this
host, so it can take minutes on a large home. `restore` never writes over the
original unless you pass `-inplace`.

**Excludes and resource caps (agent `config.json`).** These are file-only
settings; saving in the GUI keeps them.

```json
{
  "exclude_files": ["/home/me/.config/backup-ignore.txt"],
  "exclude_caches": true,
  "exclude_if_present": [".nobackup"],
  "exclude_larger_than": "1G",
  "nice": 19,
  "ionice_class": "idle",
  "read_concurrency": 1,
  "no_scan": true
}
```

- An exclude file holds one restic pattern per line. A bare name (`node_modules`)
  matches at any depth; a leading `/` anchors at the filesystem root, not at the
  backup path (unlike gitignore); `!` re-includes, e.g. `node_modules` then
  `!/home/me/.claude/plugins/**/node_modules`.
- `hoard-agent backup -dry-run` prints how many files and bytes a backup would
  include after excludes, without writing anything. Run it before a first large
  backup.
- `nice` / `ionice_class` (`idle` or `best-effort`) lower the backup's priority;
  they matter in daemon mode (the timer unit already sets both). `no_scan` skips
  the pre-scan, saving one walk of the tree per run; progress then has no
  percentage.
- Each run's log line and GUI entry include wall time and restic's peak memory.

**restic version on the client.** Backups work with an old distro-packaged
restic, but restoring from the agent needs restic 0.17 or newer (the agent warns
at start if it is older). The mass-deletion alert does not depend on the client's
version: when a snapshot lacks the size summary that 0.17+ clients embed, the
server measures it with its own restic.

`hoard-agent backup` exits 0 when a snapshot was saved, including when some
files could not be read because they were changing (restic exit code 3; the
count is in the log line and the files are listed in the GUI), and 1 on failure.

Any way you choose, hoard handles everything downstream (offsite mirror, retention,
verification) and shows each host's freshness on the dashboard. If a host stops
checking in past `stale_after`, you get an alert.

> The bundled rest-server starts with `--no-auth` for a trusted LAN, and the
> dashboards/APIs are unauthenticated too. Anyone who can reach these ports can
> browse and **delete** backups — keep them on your LAN, or put auth in front
> before exposing them. See the security note below.

## Configuration

All fields are in `config.example.json`. Highlights:

- `schedule.mirror` / `schedule.check` — `"HH:MM"` local times (empty = disabled).
- `schedule.check_weekday` — `0`–`6` (Sun–Sat) to make the integrity check weekly.
- `schedule.verify` — `"HH:MM"` for the restore fire-drill (empty = disabled).
- `schedule.stale_after` — e.g. `"26h"`; a client with no newer snapshot is flagged
  and (if `alert.on_stale`) triggers a one-shot alert on the transition to stale.
- `schedule.stale_after_hosts` — per-host overrides, e.g. `{"hourly-vm": "3h"}`.
- `retention.keep_*` — `keep_last/hourly/daily/weekly/monthly/yearly` and
  `keep_within` (`"48h"`, `"7d"`), passed to `restic forget` host by host, then one
  `prune`. The dashboard's **retention preview** dry-runs the effect before you save.
- `retention.hosts` — per-host policies that replace the default for the named
  hosts, e.g. `{"hourly-vm": {"keep_within": "48h", "keep_daily": 14}}`.
- `retention.apply_to_hot` — off by default: retention then runs only on the
  offsite repo and the hot repo keeps every snapshot. Turn it on to also apply
  retention to the hot repo before each mirror; the first run after that removes
  whatever old snapshots have accumulated there (preview shows them first). With
  it off, snapshots the offsite repo has forgotten but hot still holds are copied
  again at the next mirror, so turn it on if you want offsite retention to stick.
- `mirror.exclude_hosts` — hosts that are never copied offsite ("hot only" on the
  dashboard, and listed in the recovery kit). Snapshots of such a host that were
  mirrored before you excluded it stay offsite until retention or a delete
  removes them.
- `cold.limit_upload_kibps` / `limit_download_kibps` — transfer caps in KiB/s for
  the offsite mirror (`0` = unlimited); each agent has the same knobs for its push.
- `anomaly.files` / `anomaly.percent` — mass-deletion alert (defaults `10000` /
  `50`, `0` = off). When a host's newest backup has that many fewer files than the
  one before it (or that share of them), hoard alerts, flags the client on the
  dashboard, and tags the previous snapshot `hold`. Snapshots tagged `hold` or
  `pin` (from "Back up and pin" / `hoard-agent backup -pin`) are kept by every
  retention policy until released in the browser. `anomaly.hosts` sets
  thresholds per host, e.g. `{"hourly-vm": {"files": 2000, "percent": 50}}` when
  one deleted project there is only a few thousand files out of a million; each
  alert holds one more snapshot until you release it, so a low threshold on a
  host with routine large deletions means regular alerts and holds. Tagging gives a snapshot a new
  ID, so a held snapshot of a mirrored host is copied offsite once more.
- `alert.webhook_url` — a JSON POST target. The payload carries `title`, `content`,
  and `text` keys, covering ntfy, Discord-compatible relays, and most others.
- `alert.on_failure` / `alert.failure_threshold` — also alert when a client's
  backups fail this many times in a row.
- `smtp.host` / `port` / `username` / `from` / `to` — email alert channel alongside
  the webhook (the SMTP password is write-only: set via env or the GUI, never read
  back).

Secrets can always be supplied by env (`HOARD_*` for the server, `HOARD_AGENT_*`
for the agent), which takes precedence over the file. Repo passwords and the SMTP
password are **write-only** in the dashboards — the API returns only whether each
is set, never the value.

## HTTP API

**Server (`hoardd`)**

| Method + path | Purpose |
|---|---|
| `GET /api/status` | Running job + **live server-job view** (progress/tail), per-client freshness, verify badge, last result per job. |
| `GET /api/stats` | Repo sizes — logical vs stored (dedup/compression savings). |
| `GET /api/forecast` | Projected repo-size growth from recorded samples. |
| `GET /api/snapshots` | Live `restic snapshots` from the hot repo. |
| `GET /api/find?host=&q=` | Search one host's backups by file name; per match, the last version that has it and when it vanished. |
| `POST /api/untag` | Remove the `pin` or `hold` tag from a snapshot (`{id, tag}`). |
| `POST /api/anomaly/dismiss` | Clear a client's mass-deletion flag (`{host}`). |
| `GET /api/history` | Recent job results (mirror/check/prune/purge/…). |
| `GET /api/ls?id=&path=` | List one directory level inside a snapshot. |
| `GET /api/diff?id=&parent=` | What changed between a version and the previous. |
| `GET /api/download?id=&path=` | Stream a file from a snapshot. |
| `GET /api/recovery-kit` | Download the plain-text recovery kit. |
| `POST /api/restore` | Restore a snapshot / path to a destination. |
| `POST /api/actions/mirror` · `check` | Trigger a mirror or integrity check now (202, or 409 if busy). |
| `POST /api/actions/cancel` | Cancel the running server job (409 if idle). |
| `POST /api/purge` | Remove a path from one version or all versions of a `host`, across hot + cold. |
| `POST /api/delete-version` | Delete one whole snapshot (hot + e2 twin). |
| `GET/POST /api/config` (+ `/config/{retention-preview,test-cold,init-cold,test-email,ack-kit}`) | Read/update settings; preview retention; test/init offsite; send a test email; ack the recovery kit. |
| `POST /api/enroll/mint` · `redeem` | Mint / redeem a single-use client enrollment token. |
| `POST /api/clients/control` · `GET /api/running` | Pause/resume/cancel a client's backup; live cross-client running view. |
| `GET /healthz` | Liveness. |

**Agent (`hoard-agent`)** — `GET/POST /api/config`, `GET /api/status` (with live
progress), `POST /api/backup`, `POST /api/restore`, `GET /api/browse` (folder
picker), plus `ls` / `download` (local) and `purge` / `delete-version` (delegated
to the server). A native desktop toast fires on backup completion/failure, and
`hoard-agent -enroll <token> -enroll-server <url>` redeems an enrollment token to
self-configure.

Only one restic operation runs at a time on the server; triggers return `409`
while busy, and a running mirror/check/prune can be stopped with
`POST /api/actions/cancel`.

## Build & develop

```sh
CGO_ENABLED=0 go build ./...      # two static binaries; web UIs are embedded
go vet ./...
docker build -t nauski/hoard:latest .   # server image (bundles restic + rest-server)
nix build .#hoardd .#hoard-agent        # or via the flake
```

Each dashboard is a single embedded `web/index.html` (`go:embed`) — no build
step, no node_modules. No external Go modules, so the flake's `vendorHash` is
`null`.

## Security note

hoard is built for a trusted LAN. The rest-server (`:8000`), the server dashboard
(`:8080`), and the agent GUI (`:7420`) are **unauthenticated**, and the browse /
download / delete endpoints expose backup contents and can permanently destroy
data. The recovery-kit and enrollment-redeem endpoints hand out the repo password
to anyone who can reach the open API — enrollment tokens add single-use + expiry
on top, but on the open API they're a convenience, not an auth boundary. The
agent's browse endpoint also lists the local filesystem as the running user. Keep
these bound to your LAN/localhost. Before exposing anything, add a
reverse proxy with auth and give rest-server a `.htpasswd`
(`HOARD_REST_OPTS=""` + a mounted credentials file, then
`rest:http://user:pass@server:8000/hot`).

**Append-only.** Set `HOARD_REST_APPEND_ONLY=true` on the server container and
rest-server runs with `--append-only`: clients can add backups, find and restore,
but cannot delete or overwrite anything in the repo, so a mistake or compromise
on a client cannot destroy its history. Retention, deletes and unpin/release
still work from the server dashboard, because hoardd writes to the hot repo on
disk. This limits what port `:8000` can do; the dashboard on `:8080` can still
delete, so it needs the same care as before.

**Locks.** A backup that meets the server's prune or check waits up to 30
minutes for the repository instead of failing (needs restic 0.16+ on the
client), and server jobs wait up to 10 minutes for a running backup. Before each
nightly run the server clears stale locks left by a crashed backup.

## Status

The core loop (push → mirror → prune → check → freshness/alerts → dashboard), the
agent (GUI, live progress, folder picker, desktop notifications), the backup
browser (browse / diff / download / **restore** / delete one or all versions
across both repos), **restore fire-drill verification**, the **recovery kit**,
webhook **and email** alerting with a failure threshold, **bandwidth caps**,
**storage forecast**, **one-command client enrollment**, and **live + cancelable
server jobs** are all implemented and verified end-to-end against a live TrueNAS +
IDrive e2 setup.

Roadmap: authentication, Prometheus `/metrics`, and a
NixOS module for running the server on non-container hosts.
