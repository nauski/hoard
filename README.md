# hoard

A small, self-hosted **central backup system** that fixes the things IDrive's
Linux client gets wrong: no API, no dashboard, silent failure, and no way to see
or clean up what you've backed up. hoard is two tiny Go binaries — a **server**
that lives next to your storage and an **agent** that runs on each machine — both
with an embedded web UI, sitting in front of [restic](https://restic.net) and
[IDrive e2](https://www.idrive.com/e2/) (S3-compatible object storage).

Machines back up to the server; the server consolidates everything into a local
(**hot**) repository, mirrors it offsite to IDrive e2 (**cold**) on a schedule,
verifies integrity, enforces retention, lets you **browse and clean up** what's
stored, and **alerts you the moment a client goes stale or a job fails** — so a
backup can never quietly rot for months.

![Server dashboard](docs/images/hoard-server-dashboard.png)

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

- **Two web dashboards** — a server dashboard for the whole fleet and a per-machine
  agent GUI. No config files to hand-edit for day-to-day use.
- **Live running-backup view** — watch a backup as it runs: progress bar with
  percentage, files/bytes, elapsed, and ETA, plus a **terminal-style feed of every
  file** as it's processed. The server aggregates this **across all clients** in
  one place.
- **Pause, resume, cancel** — control any running backup from the agent *or* the
  server, even though agents bind localhost-only (control rides the client's
  status reports back to it).
- **Graphical folder picker** — choose what to back up by browsing your filesystem,
  not by typing absolute paths.
- **Backup browser** — pick a client → a version → browse its file tree → download
  a file, or delete it. See exactly what's taking up space.
- **Delete that actually frees space** — remove a file from one version or from
  **all versions**, applied across *both* hot and e2, with a type-to-confirm guard
  on the irreversible option.
- **Staleness & failure alerts** — a webhook fires the moment a client stops
  backing up or a job fails, so silent rot is impossible.
- **Scheduled mirror / retention / integrity check** — nightly offsite copy,
  `restic forget --prune`, and periodic `restic check` with data re-reads.

## The backup browser

Open the server dashboard, pick a **client**, then a **version**, and walk its
file tree. Download any file, or delete one — from just that version, or from
**every** version to reclaim the space for good.

![Backup browser](docs/images/hoard-server-browser.png)

Because your data lives in two repos, deleting "from all versions" runs
`restic rewrite --exclude` + `prune` on **both** the hot repo and e2 — otherwise
the next mirror would copy the file straight back, or e2 would keep holding it.
The irreversible option makes you type the filename to confirm:

![Delete from all versions](docs/images/hoard-delete-modal.png)

## The agent

Each machine runs `hoard-agent`, a local web GUI (default `http://127.0.0.1:7420`).
Pick folders with the browser, set a daily time, hit **Back up now**, and watch
live progress. The same backup browser is here too, scoped to this machine — and
its deletes are delegated to the server so they hit e2 as well.

![Agent dashboard](docs/images/hoard-agent-dashboard.png)

## Watching & controlling live backups

While a backup runs, the agent shows a live panel: progress with a real elapsed
time and ETA, **pause / resume / cancel** controls, and a terminal-style feed of
every file being processed (colour-coded new / changed / unchanged).

![Agent running a backup](docs/images/hoard-running-agent.png)

The server rolls this up for the **whole fleet** — one panel per client currently
backing up, each with the same live feed and controls. Pause or cancel a remote
client's backup right from here. Since agents bind localhost-only, control travels
back on the status reports each agent already sends the server every second.

![Server watching all clients](docs/images/hoard-running-server.png)

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

**Option B: plain restic.** The agent is optional — any restic works:

```sh
export RESTIC_REPOSITORY="rest:http://server:8000/hot"
export RESTIC_PASSWORD="your-hot-repo-password"
restic backup /home/me --host "$(hostname)"
```

Either way, hoard handles everything downstream (offsite mirror, retention,
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
- `schedule.stale_after` — e.g. `"26h"`; a client with no newer snapshot is flagged
  and (if `alert.on_stale`) triggers a one-shot alert on the transition to stale.
- `retention.keep_*` — passed straight to `restic forget --prune` on the cold repo.
- `alert.webhook_url` — a JSON POST target. The payload carries `title`, `content`,
  and `text` keys, covering ntfy, Discord-compatible relays, and most others.

Secrets can always be supplied by env (`HOARD_*` for the server, `HOARD_AGENT_*`
for the agent), which takes precedence over the file.

## HTTP API

**Server (`hoardd`)**

| Method + path | Purpose |
|---|---|
| `GET /api/status` | Running job, per-client freshness, last result per job. |
| `GET /api/snapshots` | Live `restic snapshots` from the hot repo. |
| `GET /api/history` | Recent job results (mirror/check/prune/purge). |
| `POST /api/actions/mirror` | Trigger a hot→cold mirror now (202, or 409 if busy). |
| `POST /api/actions/check` | Trigger an integrity check now. |
| `GET /api/ls?id=&path=` | List one directory level inside a snapshot. |
| `GET /api/download?id=&path=` | Stream a file from a snapshot. |
| `POST /api/purge` | Remove a path from one version (`version`) or all versions of a `host`, across hot + cold. |
| `POST /api/delete-version` | Delete one whole snapshot (hot + e2 twin). |
| `GET /healthz` | Liveness. |

**Agent (`hoard-agent`)** — `GET/POST /api/config`, `GET /api/status` (with live
progress), `POST /api/backup`, `GET /api/browse` (folder picker), plus `ls` /
`download` (local) and `purge` / `delete-version` (delegated to the server).

Only one restic operation runs at a time on the server; triggers return `409`
while busy.

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
data. The agent's browse endpoint also lists the local filesystem as the running
user. Keep these bound to your LAN/localhost. Before exposing anything, add a
reverse proxy with auth and give rest-server a `.htpasswd`
(`HOARD_REST_OPTS=""` + a mounted credentials file, then
`rest:http://user:pass@server:8000/hot`).

## Status

The core loop (push → mirror → prune → check → freshness/alerts → dashboard),
the agent (GUI, live progress, folder picker), and the backup browser
(browse / download / delete one or all versions across both repos) are
implemented and verified end-to-end against a live TrueNAS + IDrive e2 setup.

Roadmap: authentication, per-client retention, Prometheus `/metrics`, and a
NixOS module for running the server on non-container hosts.
