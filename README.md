# qbt-replicator

Seed the same torrent from several qBittorrent instances at once.

If you run several qBittorrent instances that **share the same storage at the same
paths**, each behind its own VPN tunnel (e.g. ProtonVPN, where one tunnel tops out
around 1 Gbps), `qbt-replicator` watches them and, once a torrent is **100% complete**
on a source instance, exports its `.torrent` and adds it to the other instances
pointing at the **same files**. The torrent then seeds from several IPs/tunnels without
downloading anything twice.

It is a single Go binary that is both a **CLI** and a **daemon with a small web UI and
JSON API**; every command and every UI button goes through the same service layer.

```
            download                         seed            seed            seed
 Internet ──────────────► qbt-1 (tunnel A) ──────►   qbt-2 (tunnel B) ─►   qbt-3 (tunnel C) ─►
                              │  100%                    ▲                    ▲
                              └── export .torrent ── qbt-replicator ─ add at same path, files kept
                                                       (shared /data mount, nothing is copied)
```

## Features

- Rule-based replication: source(s) → target(s), or `"*"` for any-to-all (mesh).
- Filters: categories, tags, excluded tags, tracker, min size, min ratio, private flag.
- Waits until the torrent is really settled (seeding state, not moving/checking, plus a
  configurable `settle_delay`) before replicating.
- Mirrors **skipped files** (priority 0) and **renamed files/folders** onto replicas
  before they start, so a replica never looks for (or downloads) the wrong files.
- **Never deletes data.** Every removal uses `deleteFiles=false`; it is not configurable.
- Replicas get a 1 B/s download limit: they are seed-only and can't meaningfully
  download into shared storage.
- Orphan cleanup: when the source torrent disappears (e.g. Sonarr/Radarr removed it),
  replicas are removed (files kept) after N confirming passes, and **never** while any
  instance is unreachable.
- Follows relocations: if the source moves its files, replicas are re-added at the new path.
- Optional share-limit sync (ratio / seeding time) from source to replicas.
- Username/password or **API key** authentication (qBittorrent 5.2+).
- Works with qBittorrent **4.x and 5.x** (detects WebAPI dialect: `pause/resume` vs
  `stop/start`, `paused` vs `stopped`, 5.x status codes). Tested against 4.6.7 and 5.3.0.
- Web UI: dashboard (health, VPN port **connected/firewalled**, speeds), torrent ×
  instance matrix with Replicate/Unreplicate buttons, pending plan, history, activity log, config.
- History of every action in an embedded SQLite database (no extra service to run).
- **Notifications** to **Telegram** (your own bot) or any **webhook** (JSON): replicas
  added/removed, failures, instances going down/up, and an optional daily summary. Bursts
  are batched into one message, repeated failures are sent once, and delivery is retried.
- JSON API with read-only / full-access **API keys** and per-key rate limits, `/healthz`,
  `/readyz`, Prometheus `/metrics`, optional basic auth.
- Dry-run mode everywhere.
- **MCP server** for AI assistants (HTTP at `/mcp`, or stdio), tools default to dry runs.

## Quick start (Docker)

```sh
mkdir -p ./qbt-replicator
curl -o ./qbt-replicator/config.yaml \
  https://raw.githubusercontent.com/williamokano/qbt-replicator/main/config.example.yaml
# edit ./qbt-replicator/config.yaml

docker run -d --name qbt-replicator \
  -v "$PWD/qbt-replicator:/config" \
  -e QBT1_PASSWORD=... -e QBT2_PASSWORD=... -e QBT3_PASSWORD=... \
  -p 8085:8080 \
  ghcr.io/williamokano/qbt-replicator:latest
```

Open http://localhost:8085. See [`docker-compose.example.yml`](docker-compose.example.yml)
for a full stack with three gluetun tunnels.

### Portainer

[`docker-compose.portainer.yml`](docker-compose.portainer.yml) needs no config file:

1. **Stacks → Add stack**, paste the file (or use *Repository* with compose path
   `docker-compose.portainer.yml`), optionally set `QBTR_PORT` (default `8085`), deploy.
2. On first start qbt-replicator writes a minimal `/config/config.yaml` into the named
   volume and runs in **setup mode** (no instances, nothing happens).
3. Open `http://<nas>:8085/config`, add your instances and a rule, click **Save & apply**.

To keep WebUI passwords out of the file, add them as stack environment variables (and
uncomment them in the `environment:` section), then write `password: ${QBT1_PASSWORD}` in
the editor. To reach qBittorrent by container name, join the stack to their network (see
the commented `networks:` section); otherwise use the NAS IP and published WebUI ports.

### Config editor

The **Config** page has a form for daemon settings, instances and rules, and an
**Advanced (YAML)** tab (CodeMirror: highlighting, Tab indents) that edits the file directly. On save it validates the config, keeps
the previous file as `config.yaml.bak`, writes the new one atomically and **applies it
without a restart** (only `server.listen`, `server.base_path` and `log` need one). Literal
passwords are shown as `********`; leave them as-is to keep the stored value. The same is
available over the API: `GET /api/v1/config/raw`, `PUT /api/v1/config/raw` (YAML body).
Protect the UI with `server.auth` if it is reachable by others: it can change the config.

If no config file exists when `serve` starts, a starter file is created (setup mode).
`qbt-replicator config init` writes the same starter file.

## Running outside Docker

```sh
go install github.com/williamokano/qbt-replicator/cmd/qbt-replicator@latest
cp config.example.yaml config.yaml   # edit it
qbt-replicator --config ./config.yaml serve
```

### Where the config is read from

First match wins:

1. `--config` / `-c` flag
2. `$QBTR_CONFIG`
3. `/config/config.yaml` (the Docker default)
4. `./config.yaml`

Inside the YAML, `${VAR}` in values is expanded from the environment (after parsing, so
special characters in the variable are safe), and instances can use
`password_file:` (Docker secrets). Scalar settings can be overridden with
`QBTR_<SECTION>_<KEY>` environment variables, e.g. `QBTR_LOG_LEVEL=debug`,
`QBTR_DAEMON_DRY_RUN=true`, `QBTR_SERVER_LISTEN=:9090`.

## Configuration

Configure everything from the web UI: the **Config** page has a form, and an **Advanced (YAML)**
editor with syntax highlighting. Or edit `config.yaml` by hand. **Every setting, how rules
decide, and rule recipes (by category, name regex, save path, tracker, tags, size…) are in
[`docs/configuration.md`](docs/configuration.md).** [`config.example.yaml`](config.example.yaml)
is a commented example.

## CLI

```
qbt-replicator serve [--no-daemon] [--listen :8080]   # daemon + web UI (Docker default)
qbt-replicator sync [--dry-run]                       # one pass and exit (cron friendly)
qbt-replicator plan                                   # what the next pass would do
qbt-replicator status                                 # health, connection status, speeds
qbt-replicator instances list | check | test <name>
qbt-replicator torrents list [--instance X] [--search S] [--category C] [--completed]
qbt-replicator replicate <hash> [--from X] [--to Y,Z] [--force] [--dry-run]   # default targets: the rules for the source
qbt-replicator unreplicate <hash> [--on Y] [--exclude] [--dry-run]   # refuses non-replicas; --exclude keeps rules from re-adding it
qbt-replicator include <hash>                         # undo --exclude: rules replicate it again
qbt-replicator history [search] [--hash H] [--instance X] [--type add] [--result error] [--since 24h]
qbt-replicator events [--limit 50] [--level warn] [--hash H]   # activity log
qbt-replicator apikey create <name> [--scope full] | list | revoke <id|name>
qbt-replicator mcp [--read-only]                      # MCP server over stdio (local, or --host)
qbt-replicator notify list | test <name>              # notification destinations, send a test message
qbt-replicator config init | validate | show          # init writes a starter file; show redacts secrets
qbt-replicator healthcheck [--url ...]                # used by Docker HEALTHCHECK
qbt-replicator version
```

Global flags: `-c/--config`, `--log-level`, `-o/--output table|json`, `--host`, `--api-key`.

Inside Docker: `docker exec qbt-replicator /qbt-replicator plan`.

**Remote mode.** From any machine, point the CLI at a running qbt-replicator with an
[API key](docs/configuration.md#api-keys); no config file or database is needed there:

```sh
export QBTR_HOST=https://replicator.lan QBTR_API_KEY=qbtr_...   # or --host / --api-key
qbt-replicator status
qbt-replicator history --since 24h
qbt-replicator replicate 3f2a... --dry-run    # changes need a full-access key
```

Every command works remotely except `serve`, `apikey`, `config validate` and `config init`,
which need the local files. `--host https://user:pass@host` uses the web login instead of a
key. Rate-limited requests (`429`) with a short `Retry-After` are retried once.

## AI assistants (MCP)

qbt-replicator is an [MCP](https://modelcontextprotocol.io) server, so assistants such as
Claude can answer "why isn't this torrent on qbt-3?" or "what was replicated today?" and,
with a full-access key, replicate or remove replicas for you. It needs an
[API key](docs/configuration.md#api-keys); a read-only key only gets the read tools.

**Over HTTP** (the running server, endpoint `/mcp`), e.g. Claude Code:

```sh
claude mcp add --transport http qbt-replicator https://replicator.lan/mcp \
  --header "Authorization: Bearer qbtr_..."
```

**Over stdio** (Claude Desktop and other clients that start a command): the binary
forwards to the server, nothing else is needed on that machine:

```json
{
  "mcpServers": {
    "qbt-replicator": {
      "command": "qbt-replicator",
      "args": ["mcp", "--host", "https://replicator.lan"],
      "env": { "QBTR_API_KEY": "qbtr_..." }
    }
  }
}
```

| Tool | Does |
|---|---|
| `get_status` | Instance health, VPN port connected/firewalled, speeds, replicas, last pass |
| `search_torrents` | Fuzzy search by name or hash prefix; where each torrent is source or replica |
| `get_torrent` | One torrent: where it is, planned actions, **why it is skipped**, its history and activity |
| `get_plan` | What the next pass will do and why torrents are skipped |
| `get_history` / `get_activity` | Executed actions / activity log with filters (`since: 24h`, instance, type, result) |
| `get_config` | Instances, rules, daemon and notification settings, secrets masked |
| `sync`, `replicate`, `unreplicate`, `include` | Full-access keys only. **`dry_run` defaults to true**: the assistant shows what would happen and has to ask again with `dry_run: false` |
| `test_instance`, `test_notification` | Full-access keys only. Connection and notification tests |

Torrents can be named by hash, hash prefix or name; an ambiguous name returns the
candidates. Every tool call counts against the key's [rate limit](docs/configuration.md#api-keys)
(reads or changes). Configuration changes, API keys and secrets are not available over MCP.

## HTTP endpoints

| Method | Path | |
|---|---|---|
| GET | `/`, `/torrents`, `/plan`, `/history[?hash=H]`, `/events`, `/config` | HTML UI (`?refresh=15` auto-refreshes) |
| POST | `/config` | config editor save (form field `content`) |
| GET / PUT | `/api/v1/config/raw` | read (secrets masked) / replace the YAML config |
| POST | `/api/v1/instances/test` `{"name"}` or `{"url","username","password",…}` | test a saved or unsaved instance's connection and credentials |
| GET | `/api/v1/notifications` | notifiers with pending/sent/failed counts |
| POST | `/api/v1/notifications/test` `{"name"}` or `{"type","url",…}` | send a test notification |
| POST | `/actions/sync`, `/actions/replicate`, `/actions/unreplicate` | UI form actions |
| GET | `/api/v1/status[?fresh=1]`, `/api/v1/torrents`, `/api/v1/plan`, `/api/v1/events?limit=N&before=ID&level=&instance=&hash=`, `/api/v1/history?limit=N&before=ID&hash=&search=&instance=&type=&result=&since=24h|RFC3339`, `/api/v1/config` | JSON |
| POST | `/api/v1/sync` `{"dry_run":bool}` | run a pass, returns the report |
| POST | `/api/v1/replicate` `{"hash","from","to":[],"force","dry_run"}` | manual replication |
| POST | `/api/v1/unreplicate` `{"hash","on":[],"exclude","dry_run"}` | remove replicas (files kept); `exclude` tags the source so rules skip it |
| POST | `/api/v1/include` `{"hash","dry_run"}` | remove the exclude tag again |
| GET | `/api/v1/whoami` | how the request authenticated: `{auth, name, scope}` |
| POST | `/mcp` | MCP (streamable HTTP, stateless); needs an API key or the login |
| GET | `/healthz`, `/readyz`, `/metrics` | ops |

Cross-origin POSTs are rejected (CSRF protection). Scripts authenticate with an
[API key](docs/configuration.md#api-keys) (`Authorization: Bearer qbtr_…`), read-only or
full access, rate limited per key.

## How a replica is added

1. Source torrent is complete, in a seeding state, not moving/checking, and older than `settle_delay`.
2. `.torrent` is exported from the source (`/torrents/export`).
3. It is added to the target **stopped**, at the source's save path, with
   `contentLayout=Original`, `autoTMM=false`, the replica tag, and a 1 B/s download limit.
4. File priorities (skipped files) and file names are copied from the source.
5. With `skip_checking: true` the replica is started and must report 100% within 30s,
   otherwise it is stopped and an error is logged. With `skip_checking: false` it is
   rechecked while stopped and only started if it is 100%.

## Caveats and things to know

- **Private trackers.** Seeding the same torrent with one passkey from several IPs can
  get you flagged or banned. Private torrents are skipped unless `include_private: true`.
- **Port forwarding matters.** A firewalled (no forwarded port) instance seeds far less.
  The dashboard shows each instance's connection status; enable port forwarding in gluetun.
- **Same paths.** All instances must see the data at the same path, or you must configure
  `path_mappings`. qbt-replicator never reads or writes your data; it only talks to the WebUIs.
- **Deleting with files.** If you delete a torrent *with files* on any instance, the data is
  gone for all of them; that's the nature of shared storage. The replicator itself never does this.
- **Renaming on a replica** renames the real file on disk for everyone. Rename on the source.
- **Disk I/O.** `skip_checking: false` reads the whole torrent once per target.
- **Stopped replicas show 0%.** qBittorrent only applies "skip hash check" once the torrent
  runs, so replicas added with `start: false` show 0% until started.
- History, the activity log and planner state live in an embedded SQLite database,
  `/config/qbt-replicator.db` by default (see [`database`](docs/configuration.md#database)).

## Development

```sh
make test      # go test -race ./...
make lint      # golangci-lint (v2)
make build     # bin/qbt-replicator
make docker    # local image
```

Tests run against an in-memory fake qBittorrent (`internal/qbt/fake`) that emulates both
the 4.x and 5.x API dialects. See [`docs/PLAN.md`](docs/PLAN.md) for the design.

### Releasing

Versions are computed from [Conventional Commits](https://www.conventionalcommits.org/)
(see [`AGENTS.md`](AGENTS.md)):

1. Merge PRs to `main` with conventional titles (`feat: …`, `fix: …`, `feat!: …`). PRs are
   squash-merged and the `PR title` check enforces the format.
2. [release-please](https://github.com/googleapis/release-please) keeps an open
   `chore(release): x.y.z` PR with the next semantic version and `CHANGELOG.md`.
   While below 1.0.0, `feat` bumps the minor version and breaking changes bump the minor too.
3. Merging that PR tags `vX.Y.Z`, creates the GitHub release, and the same workflow run
   publishes binaries/archives with [GoReleaser](https://goreleaser.com) and the
   `ghcr.io/williamokano/qbt-replicator:X.Y.Z` / `X.Y` / `X` images.

Every push to `main` still publishes `:latest` and `:sha-<commit>`. To (re)publish
artifacts for an existing tag, run the **Release** workflow manually with its `tag` input.
`make snapshot` runs GoReleaser locally without publishing.

CI (`.github/workflows/ci.yml`) runs gofmt, `go mod tidy`, vet, golangci-lint, race tests and
a Docker build on every pull request. `.github/workflows/release.yml` runs release-please,
GoReleaser and the multi-arch image build (see *Releasing*).
