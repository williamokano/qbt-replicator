# qbt-replicator — Implementation Plan

## Context

Several qBittorrent instances run on one NAS, each behind its own ProtonVPN tunnel (~1.05 Gbps cap per tunnel, per-TCP limits too). All instances mount the **same storage at the same paths**. Downloading happens on one instance; once a torrent hits 100% it should be "exported" and added to the other instances pointing at the same files, so the torrent seeds from 3+ IPs and uses more of the uplink.

Deliverable: a Go **1.27.3** program (`qbt-replicator`) that is both a **CLI** (cobra) and a **daemon + web UI** (stdlib `net/http`) sharing one service layer. Config is YAML (viper), default `/config/config.yaml` in Docker, overridable for local runs. GitHub Actions for PR checks and for publishing images to GHCR from `main`.

The repo (`williamokano/qbt-replicator`) is currently empty. Step 0 of implementation: copy this plan to `docs/PLAN.md` in the repo, commit and push on `claude/affectionate-newton-r8m8jw`.

---

## Problems identified (and how the design handles them)

| # | Problem | Handling |
|---|---------|----------|
| 1 | **Deleting files from a replica deletes them for everyone** (shared FS). | Replicator *never* calls delete with `deleteFiles=true`. Hard-coded, not configurable. |
| 2 | ***arr apps (Sonarr/Radarr) remove the torrent from the master after ratio/import** → replicas keep seeding a torrent whose files vanish → `missingFiles` errors. | "Orphan cleanup": a replica whose hash no longer exists as a non-replica on any *healthy* instance is removed (files kept) after 2 consecutive confirming ticks. Disabled if any instance is unreachable that tick. |
| 3 | **Master skipped some files** (priority 0). Replica added with skip-check would think it has everything; without skip-check it would *download* missing files into the shared dir. | Add replica **stopped**, copy file priorities from source (`torrents/filePrio`), then start. |
| 4 | **Files/folders renamed on master** → replica's paths don't match the disk. | After add (still stopped) compare file lists, apply `renameFile`/`renameFolder` to match source. |
| 5 | **Content layout / subfolder settings differ between instances**. | Always add with `contentLayout=Original`; savepath = source `save_path`; rename step (#4) fixes residual differences. |
| 6 | **Torrent "100%" while still moving** (temp/incomplete dir → save dir, autoTMM category move, `checkingUP`). | Eligible only when `progress==1`, state ∈ {uploading, stalledUP, queuedUP, pausedUP/stoppedUP, forcedUP}, not `moving`/`checking*`, and `completion_on` older than `settle_delay` (default 2m). |
| 7 | **Master later relocates files** (category change with autoTMM, manual move). | Detect replica `save_path` ≠ mapped source `save_path` → remove replica (keep files) and re-add at new path. Replicas are added with `autoTMM=false` so they never move files themselves. |
| 8 | **Different mount points per container** despite same storage. | Optional per-instance `path_mappings` (instance path ↔ canonical path). Identity by default. |
| 9 | **skip_checking is a trust decision**. | Default `skip_checking: true` (source is verified complete, avoids 3× disk reads). Option to recheck instead. After start, if replica `progress < 1` → stop it and emit an error event (prevents replicas downloading). |
| 10 | **Private trackers**: same passkey announcing from 3 IPs can be flagged/banned. | Rule filter `include_private` defaults to **false**. Loud warning in docs. Per-tracker allow-list available. |
| 11 | **qBittorrent API differences**: `torrents/export` needs qBT ≥ 4.5; v5 (WebAPI ≥ 2.11) renamed `pause/resume`→`stop/start`, `paused`→`stopped`, `pausedUP`→`stoppedUP`. | Client detects `app/webapiVersion` on login and picks endpoints/params. Fallback for old versions: read `<hash>.torrent` from a mounted `BT_backup` dir (`export_fallback_dir`, optional). |
| 12 | **Replication loops** in "all-to-all" mode (replica treated as a source). | Replicas carry an ownership tag (`replica_tag`, default `qbtr-replica`); tagged torrents are never sources. Stateless: tags are the source of truth, no DB. |
| 13 | **Auth quirks**: SID cookie expiry, IP ban after failed logins, Host/Referer CSRF checks. | Re-login on 403 once; exponential backoff on login failure (never hammer); always send `Referer`/`Origin` = base URL. |
| 14 | **Unreachable instance** looks like "torrent missing". | Snapshot marks instance unhealthy; unhealthy instances are skipped as source/target and block all removals for that tick. |
| 15 | **Port forwarding**: seeding is far more effective with a forwarded port per tunnel. | UI/CLI show each instance's `connection_status` (connected/firewalled), listen port and up speed. Docs mention gluetun/ProtonVPN NAT-PMP. |
| 16 | **Duplicate add** returns `Fails.` | Treat "already present" as success; always check target by hash first. |
| 17 | The web UI can add/remove torrents. | Optional basic auth; POST-only mutations with same-origin check; config shown redacted. |

---

## Architecture

```
cmd/qbt-replicator/main.go          -> cli.Execute()
internal/version/                   -> Version/Commit/Date (ldflags)
internal/config/                    -> viper load, env expansion, defaults, Validate()
internal/qbt/                       -> qBittorrent WebAPI v2 client (+ fake server for tests)
internal/replicator/
    snapshot.go                     -> fetch state of all instances concurrently
    planner.go                      -> PURE: (config, snapshot, memory) -> []Action
    executor.go                     -> runs actions (add/remove/readd/stop) w/ concurrency, dry-run
    daemon.go                       -> ticker loop, ctx cancel, trigger channel
internal/events/                    -> thread-safe ring buffer of events (activity log)
internal/app/service.go             -> FAÇADE used by BOTH cli and web
internal/cli/                       -> cobra commands (thin, call app.Service)
internal/web/                       -> mux, handlers (HTML + JSON), embed templates/static
```

**Shared commands principle:** every operation is a method on `app.Service`; cobra commands and HTTP handlers are thin adapters. Methods:
`Status(ctx)`, `Instances(ctx)`, `CheckInstances(ctx)`, `Torrents(ctx, filter)` (matrix hash × instance), `Plan(ctx)` (what would happen), `SyncOnce(ctx, dryRun)`, `Replicate(ctx, hash, from, to[], opts)`, `Unreplicate(ctx, hash, from[])`, `Events(limit)`, `ConfigRedacted()`.

### qbt client (`internal/qbt`)
Plain `net/http` + `cookiejar`, timeout per request, context everywhere. Endpoints:
`auth/login`, `app/version`, `app/webapiVersion`, `transfer/info`, `torrents/info` (filter/hashes/category/tag), `torrents/properties`, `torrents/files`, `torrents/trackers` (private flag fallback; `private` field exists in info on newer versions), `torrents/export`, `torrents/add` (multipart: `torrents`, `savepath`, `category`, `tags`, `stopped`|`paused`, `skip_checking`, `contentLayout`, `autoTMM=false`, ratio/seeding limits), `torrents/delete` (`deleteFiles=false` forced inside client — no parameter exposed), `torrents/start|resume`, `torrents/stop|pause`, `torrents/filePrio`, `torrents/renameFile`, `torrents/renameFolder`, `torrents/recheck`, `torrents/createCategory`, `torrents/categories`, `torrents/setShareLimits`.
`internal/qbt/fake` — `httptest` in-memory qBT emulating these endpoints (both v4 and v5 dialects) for tests.

### Reconcile loop (per tick, default 30s; also triggerable from CLI/UI)
1. **Snapshot** all instances concurrently (`torrents/info`, `transfer/info`); record health.
2. **Plan** (pure function, unit-tested heavily):
   - For each rule → each healthy source instance → each non-replica torrent matching the rule filter and eligible (#6) → for each healthy target lacking the hash → `Add`.
   - For each replica: no healthy non-replica owner anywhere (and all instances healthy) → `Remove` after 2 consecutive ticks (memory map in planner input) (#2).
   - Replica save_path mismatch → `Readd` (#7). Replica `missingFiles`/`error` with source present → `Recheck` once, then event.
   - Optional `sync_share_limits`: mirror source ratio/seeding-time limits.
3. **Execute** with bounded concurrency (`daemon.concurrency`, default 2), cache exported `.torrent` bytes per hash for the tick. Add flow: export → add stopped (+category create if needed) → poll until hash appears (≤30s) → filePrio sync → renames → share limits → start (unless rule says stay stopped) → verify progress==1 else stop + error event.
4. Emit events (info/warn/error) into ring buffer + slog.

---

## Configuration

Default path `/config/config.yaml`. Resolution order: `--config/-c` flag → `QBTR_CONFIG` env → `/config/config.yaml` → `./config.yaml` (when not in Docker). Viper with env prefix `QBTR_` for scalar keys (`QBTR_SERVER_LISTEN`, `QBTR_DAEMON_INTERVAL`, `QBTR_LOG_LEVEL`…). Secrets: `${VAR}` expansion on file content before parsing, plus `password_file:` support (Docker secrets). `config.example.yaml` committed.

```yaml
log:
  level: info            # debug|info|warn|error
  format: text           # text|json
server:
  listen: ":8080"
  base_path: "/"         # for reverse proxies
  auth: { username: "", password: "" }   # empty = disabled
daemon:
  enabled: true
  interval: 30s
  concurrency: 2
  dry_run: false
  replica_tag: qbtr-replica
instances:
  - name: qbt-1
    url: http://gluetun-1:8080
    username: admin
    password: ${QBT1_PASSWORD}
    # password_file: /run/secrets/qbt1
    insecure_skip_verify: false
    path_mappings: []    # - { instance: /downloads, canonical: /data/downloads }
    export_fallback_dir: ""   # mounted BT_backup for qBT < 4.5
  - name: qbt-2
    url: http://gluetun-2:8080
    username: admin
    password: ${QBT2_PASSWORD}
  - name: qbt-3
    url: http://gluetun-3:8080
    username: admin
    password: ${QBT3_PASSWORD}
rules:
  - name: master-to-all
    sources: [qbt-1]      # or ["*"] = any instance (mesh)
    targets: ["*"]        # "*" = all except the source
    settle_delay: 2m
    filter:
      categories: []      # empty = any
      tags: []
      exclude_tags: [no-replicate]
      trackers: []        # substring match on tracker host
      include_private: false
      min_size: 0         # e.g. "1GiB"
      min_ratio: 0
    add:
      category: inherit   # inherit | "" | explicit name
      extra_tags: []
      skip_checking: true
      start: true
    remove_orphans: true
    follow_relocation: true
    sync_share_limits: false
```
`config.Validate()` checks: unique names, URLs parse, rule sources/targets reference known instances, interval ≥ 5s, `replica_tag` non-empty.

---

## CLI (cobra)

Global flags: `--config/-c`, `--log-level`, `--output/-o table|json`.

| Command | Purpose |
|---|---|
| `serve` | Run daemon + web server (default Docker CMD). `--no-daemon` for UI only. |
| `sync [--dry-run]` | One reconcile pass and exit (cron-friendly). |
| `plan` | Show actions that would be taken (alias of dry-run with detail). |
| `status` | Instances health, connection status, speeds, replica counts. |
| `instances list` / `instances check` | Config + connectivity/auth/version check. |
| `torrents list [--instance X] [--filter completed]` | Matrix of torrents × instances. |
| `replicate <hash> [--from X] [--to Y,Z] [--force]` | Manual replication (ignores rule filters with `--force`). |
| `unreplicate <hash> [--on Y]` | Remove replica(s), files kept. Refuses non-replica torrents. |
| `config validate` / `config show` | Validate / print redacted effective config. |
| `healthcheck [--url]` | Hits `/healthz`; used by Docker HEALTHCHECK (distroless has no curl). |
| `version` | Version/commit/date. |

---

## Web (stdlib `net/http`, Go 1.22+ method patterns, `html/template` + `embed`)

HTML (simple, server-rendered, `<meta refresh>` toggle, minimal embedded CSS, no JS framework):
- `GET /` dashboard: instance cards (health, version, connection status, listen port, up/down speed), totals, last tick, "Sync now" button.
- `GET /torrents`: table torrent × instance (✓ source / ✓ replica / – missing / ⚠ error) with Replicate / Unreplicate buttons; filter by category/search.
- `GET /plan`: pending actions preview.
- `GET /events`: activity log.
- `GET /config`: redacted config.
- `POST /actions/sync|replicate|unreplicate` → redirect back with flash.

JSON API (same service methods): `GET /api/v1/status|instances|torrents|plan|events`, `POST /api/v1/sync|replicate|unreplicate`.
Ops: `GET /healthz` (process up), `GET /readyz` (≥1 instance reachable), `GET /metrics` (Prometheus via `client_golang`: per-instance up, up/down speed, torrents, replicas, actions total by type/result, tick duration).
Middleware: request logging (slog), recover, optional basic auth (constant-time compare; `/healthz` exempt), same-origin check on POST. Graceful shutdown on SIGINT/SIGTERM.

---

## Docker

Multi-stage `Dockerfile`:
- build: `golang:1.27.3-alpine`, `CGO_ENABLED=0`, `-trimpath -ldflags "-s -w -X …version…"`, `TARGETOS/TARGETARCH` for buildx.
- runtime: `gcr.io/distroless/static-debian12:nonroot`, `VOLUME /config`, `EXPOSE 8080`, `ENV QBTR_CONFIG=/config/config.yaml`, `HEALTHCHECK CMD ["/qbt-replicator","healthcheck"]`, `ENTRYPOINT ["/qbt-replicator"]`, `CMD ["serve"]`.
- No file access needed to the media share (all via API) unless `export_fallback_dir` is used.
- `docker-compose.example.yml` showing it next to 3 gluetun+qbittorrent pairs on the same network.

---

## GitHub Actions

- `.github/workflows/ci.yml` — on `pull_request` and `push` to `main`: `actions/setup-go` (`go-version-file: go.mod`), `gofmt` check, `go vet`, `golangci-lint` (action), `go test -race -cover ./...`, `go build`, docker buildx build (linux/amd64, no push) to validate Dockerfile.
- `.github/workflows/release.yml` — on `push` to `main` and tags `v*`: permissions `contents: write, packages: write`; QEMU + buildx; `docker/login-action` to `ghcr.io` with `GITHUB_TOKEN`; `docker/metadata-action` tags: `latest` + `sha-<short>` on main, `X.Y.Z`/`X.Y`/`X` on semver tags; platforms `linux/amd64,linux/arm64`; GHA cache; OCI labels. On `v*` tags also create a GitHub Release (`softprops/action-gh-release`) with changelog-from-commits.
- `.github/dependabot.yml` for gomod, github-actions, docker.
- `.golangci.yml` with sane linters (errcheck, govet, staticcheck, revive, gosec, bodyclose, noctx).

---

## Files to create

`go.mod` (`go 1.27`, `toolchain go1.27.3`; deps: cobra, viper, prometheus/client_golang), `cmd/qbt-replicator/main.go`, `internal/{version,config,qbt,qbt/fake,replicator,events,app,cli,web}/…`, `internal/web/templates/*.html`, `internal/web/static/style.css`, `Dockerfile`, `.dockerignore`, `config.example.yaml`, `docker-compose.example.yml`, `Makefile` (build/test/lint/docker/run), `README.md` (overview, quick start, config reference, config path override rules, CLI reference, private tracker warning, port forwarding note, troubleshooting), `docs/PLAN.md`, `.github/…` as above, `.gitignore`.

## Implementation phases (each ends green: build + tests + lint)

0. Save plan to `docs/PLAN.md`; scaffold module, Makefile, `.gitignore`, CI workflow.
1. `config` package + tests (defaults, env expansion, password_file, validation, path resolution).
2. `qbt` client + `qbt/fake` + tests for both API dialects, re-login, Fails./duplicate handling.
3. `replicator`: snapshot, pure planner (table-driven tests for every problem in the table above), executor (against fake), daemon loop.
4. `events` + `app.Service`.
5. `cli` commands.
6. `web` (HTML + JSON + middleware + metrics) with `httptest` handler tests.
7. Dockerfile, compose example, release workflow, dependabot, golangci config.
8. README/docs, `config.example.yaml`.

## Verification

- `go test -race ./...` — planner tables cover: eligibility states, settle delay, skipped files, renames, relocation, orphan grace + unhealthy-instance block, private filter, mesh loop prevention, v4 vs v5 params; executor runs end-to-end against 3 `fake` qBT servers sharing one simulated filesystem.
- `golangci-lint run`, `gofmt -l .` empty.
- `docker build .` then `docker run -v $PWD/example:/config … qbt-replicator config validate` and `serve`; `curl /healthz`, `/api/v1/status`.
- Optional manual E2E: `docker-compose.example.yml` with 3 `linuxserver/qbittorrent` containers sharing a volume; add a small public torrent (e.g. a Linux ISO) to qbt-1, watch it appear on qbt-2/3 seeding with the same path; delete from qbt-1 (keep files) and verify replicas are removed and files untouched.
- CI workflow green on the PR; release workflow verified by pushing to `main` and pulling `ghcr.io/williamokano/qbt-replicator:latest`.

## Future ideas (not in first implementation)
Webhook notifications (ntfy/Discord) on errors; per-target capacity limits (max replicas / skip targets with low free disk via `sync/maindata` `free_space_on_disk`); "upload-aware" placement (replicate only torrents with active leechers / to least-loaded tunnel); hot config reload (viper `WatchConfig`); `sync/maindata` incremental polling for large libraries; CLI `--remote` mode talking to a running daemon's JSON API.
