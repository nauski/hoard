# hoard

A small, self-hosted **central backup server** that fixes the thing IDrive's
Linux client gets wrong: no API, no dashboard, and silent failure. hoard is a
single Go binary + web dashboard that sits in front of [restic](https://restic.net)
and [IDrive e2](https://www.idrive.com/e2/) (S3-compatible object storage).

Local machines push their backups to hoard with plain restic. hoard consolidates
them into a local (**hot**) repository, mirrors that offsite to IDrive e2 (**cold**)
on a schedule, verifies integrity, enforces retention, and **alerts you the moment
a client goes stale or a job fails** — so a backup can never quietly rot for months.

> **Why not just use the IDrive client?** Classic IDrive personal backup has no
> public API — the only interface is their proprietary Perl/Python `.bin`, which
> is exactly the fragile, un-automatable thing this replaces. IDrive **e2** exposes
> a standard S3 endpoint, so we drive it with restic (encryption, dedup, and
> `restic check` integrity verification) instead of reverse-engineering a
> proprietary protocol whose server side can change without notice.

## Architecture

```
 desktop restic ─┐
 laptop restic  ─┼─ push ─▶  ┌───────────── hoard (TrueNAS Docker app) ─────────────┐
 other clients  ─┘           │  rest-server  →  HOT repo on pool  (/data/hot)         │
                             │        │                                               │
                             │   hoardd control-plane:  API + dashboard + scheduler   │
                             │        │                                               │
                             │        └── restic copy ─▶  COLD repo on IDrive e2 (S3)  │
                             └───────────────────────────────────────────────────────┘
```

- **Hot repo** — a restic repository on your TrueNAS pool. `rest-server` (bundled)
  gives clients an HTTP push target: `http://truenas:8000/hot`.
- **Cold repo** — a second, independent restic repo living in an IDrive e2 bucket.
  hoard runs `restic copy` hot→cold on a schedule; both repos are separately
  `restic check`-able, giving you two verifiable tiers (a 3-2-1-shaped setup).
- **Control plane (`hoardd`)** — serves the JSON API and dashboard, runs the
  scheduler (mirror / check / prune / freshness), and sends alerts.

## Components

| Part | What it is |
|------|------------|
| `hoardd` | The Go control-plane binary (API, dashboard, scheduler, alerts). |
| `rest-server` | Upstream restic REST server; the client push target. Bundled in the image. |
| `restic` | The backup engine. Bundled in the image; also what clients run. |

## Quick start (Docker / TrueNAS)

1. **Create an IDrive e2 bucket** and an access key. Note the S3 endpoint, e.g.
   `s3:https://x0x0.va.idrivee2-1.com/<bucket>`.

2. **Write `config.json`** (see `config.example.json`). Keep secrets out of the
   file and pass them as env vars instead.

3. **Run it** (compose shown; on TrueNAS use the *Custom App* / *Install via YAML*):

   ```yaml
   services:
     hoard:
       image: nauski/hoard:latest      # or build: .
       restart: unless-stopped
       ports:
         - "8080:8080"                 # dashboard + API
         - "8000:8000"                 # restic push target for clients
       volumes:
         - /mnt/Premium/backups/hoard:/data
       environment:
         HOARD_HOT_PASSWORD: "your-hot-repo-password"
         HOARD_COLD_REPOSITORY: "s3:https://x0x0.va.idrivee2-1.com/your-bucket"
         HOARD_COLD_PASSWORD: "your-cold-repo-password"
         HOARD_COLD_S3_ACCESS_KEY_ID: "e2-access-key-id"
         HOARD_COLD_S3_SECRET_ACCESS_KEY: "e2-secret-key"
         HOARD_ALERT_WEBHOOK_URL: "https://ntfy.sh/your-topic"
   ```

   Mount your `config.json` at `/data/hoard.json` (or bake defaults and rely on env).

4. **Initialize the repos once** (creates hot + cold if missing):

   ```sh
   docker exec -it hoard hoardd -config /data/hoard.json -init
   ```

5. **Open the dashboard** at `http://truenas:8080`.

## Pointing a client at hoard

On each machine you want backed up, restic just targets the REST server:

```sh
export RESTIC_REPOSITORY="rest:http://truenas:8000/hot"
export RESTIC_PASSWORD="your-hot-repo-password"

restic backup /home/me --host "$(hostname)"
```

Schedule that with a systemd timer / cron. hoard handles everything downstream
(offsite mirror, retention, verification) and shows each host's freshness on the
dashboard. If a host stops checking in past `stale_after`, you get an alert.

> The bundled rest-server starts with `--no-auth` for simplicity on a trusted
> LAN. For anything exposed more widely, set `HOARD_REST_OPTS=""`, mount a
> `.htpasswd`, and give each client `rest:http://user:pass@truenas:8000/hot`.

## Configuration

All fields are in `config.example.json`. Highlights:

- `schedule.mirror` / `schedule.check` — `"HH:MM"` local times (empty = disabled).
- `schedule.check_weekday` — `0`–`6` (Sun–Sat) to make the integrity check weekly.
- `schedule.stale_after` — e.g. `"26h"`; a client with no newer snapshot is flagged
  and (if `alert.on_stale`) triggers a one-shot alert on the transition to stale.
- `retention.keep_*` — passed straight to `restic forget --prune` on the cold repo.
- `alert.webhook_url` — a JSON POST target. The payload carries `title`, `content`,
  and `text` keys, which covers ntfy, Discord-compatible relays, and most others.

Secrets can always be supplied by env (`HOARD_*`, see `internal/config`), which
takes precedence over the file.

## HTTP API

| Method + path | Purpose |
|---|---|
| `GET /api/status` | Current running job, per-client freshness, last result per job. |
| `GET /api/snapshots` | Live `restic snapshots` from the hot repo. |
| `GET /api/history` | Recent job results (mirror/check/prune). |
| `POST /api/actions/mirror` | Trigger a hot→cold mirror now (202, or 409 if busy). |
| `POST /api/actions/check` | Trigger an integrity check now. |
| `GET /healthz` | Liveness. |

Only one restic operation runs at a time; triggers return `409` while busy.

## Build & develop

```sh
CGO_ENABLED=0 go build ./cmd/hoardd     # single static binary; web UI is embedded
go vet ./...
docker build -t nauski/hoard:latest .
```

The dashboard is a single `cmd/hoardd/web/index.html`, embedded via `go:embed` —
no build step, no node_modules.

## Status

v0 — the core loop (push → mirror → prune → check → freshness/alerts → dashboard)
is implemented and smoke-tested end-to-end against local repos. Roadmap:
per-client retention, `.htpasswd` provisioning from the UI, Prometheus `/metrics`,
and a NixOS module for non-container hosts.
