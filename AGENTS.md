# AGENTS.md

Guidance for AI coding agents (and humans) working in this repository.

## Commit messages: Conventional Commits (required)

**Every commit message and every pull request title must follow
[Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/).** Releases are
versioned automatically from them (semantic versioning via release-please), so a wrong
type means a wrong version.

```
<type>[optional scope][!]: <description>

[optional body]

[optional footer(s)]
```

| Type | Use for | Version bump |
|---|---|---|
| `feat` | a user-visible feature | minor (patch→minor while < 1.0.0) |
| `fix` | a bug fix | patch |
| `perf` | a performance improvement | patch |
| `feat!` / `fix!` / footer `BREAKING CHANGE:` | an incompatible change (config keys, API, CLI flags) | major (minor while < 1.0.0) |
| `docs`, `test`, `refactor`, `style`, `build`, `ci`, `chore`, `revert` | everything else | none |

Examples:

- `feat(web): add config editor with live reload`
- `fix(qbt): accept 204 from /auth/login on qBittorrent 5.x`
- `feat(config)!: rename daemon.interval to daemon.poll_interval`
- `ci: bump docker/build-push-action to v7`

Rules:

- Imperative, lower-case description, no trailing period, ≤ 72 characters.
- Useful scopes: `qbt`, `replicator`, `config`, `web`, `cli`, `docker`, `ci`, `deps`.
- PRs are squash-merged with the PR title as the commit message; CI (`PR title`) rejects
  non-conventional titles.
- Never hand-edit `CHANGELOG.md`, `.release-please-manifest.json` or create version tags;
  release-please owns them. Merge its `chore(release): x.y.z` PR to release.

## Project overview

`qbt-replicator` replicates completed torrents from a source qBittorrent instance to other
instances that share the same storage, so one torrent seeds from several VPN tunnels.
It is a Go 1.27 single binary: cobra CLI + stdlib `net/http` web UI/JSON API, sharing one
service layer. See `README.md` for usage and `docs/PLAN.md` for the design.

```
cmd/qbt-replicator/        entrypoint
internal/config/           viper YAML config, validation, starter file, secret masking
internal/qbt/              qBittorrent WebAPI client (4.x and 5.x dialects)
internal/qbt/fake/         in-memory fake qBittorrent used by tests
internal/replicator/       snapshot, pure planner, executor, engine/daemon, metrics
internal/store/            SQLite persistence: events, run history, planner memory, outbox
internal/notify/           notifications: Telegram/webhook senders, outbox dispatcher
internal/app/              Service façade used by BOTH cli and web (+ config reload)
internal/client/           typed JSON API client (CLI remote mode --host)
internal/backend/          operations shared by CLI and MCP: local service or remote client
internal/cli/              cobra commands (thin adapters)
internal/web/              handlers, templates, static assets (embedded)
```

## Commands

```sh
make test     # go test -race -cover ./...
make lint     # golangci-lint (v2 config in .golangci.yml)
make build    # bin/qbt-replicator
make snapshot # goreleaser snapshot build into dist/
```

Before pushing: `gofmt -l .` must be empty, `go mod tidy` must not change files,
`go vet ./...`, `golangci-lint run ./...` and `go test -race ./...` must pass.

## Invariants (do not break)

- **Never delete torrent data.** `qbt.Client.Remove` always sends `deleteFiles=false`;
  storage is shared, deleting files on one instance destroys them for all.
- Replicas are added stopped, get the source's file renames/priorities before they start,
  and carry a 1 B/s download limit. They must never download into shared storage.
- Torrents tagged with `daemon.replica_tag` are owned by the replicator and are never
  treated as sources (prevents replication loops).
- No orphan removal while any instance is unreachable.
- API keys never manage API keys, never change `server.auth` and never see secrets; key
  management stays in the web UI (outside `/api/`) and the local CLI.
- New user-facing operations go into `internal/app.Service` first, then get both a CLI
  command and an HTTP handler; keep those adapters thin. CLI commands go through the
  `backend.Backend` interface (`internal/backend`) so they also work remotely: add the
  method to `internal/client` too.
- The planner (`replicator.Compute`) stays a pure function; add table tests for new cases.
- Test against `internal/qbt/fake` (both dialects) instead of real servers.
- Schema changes are new entries appended to `store.migrations`; never edit a released
  migration. Keep the SQL portable (a Postgres store may follow).
- Every config key is documented in `docs/configuration.md`; add new keys there, to
  `config.example.yaml` and, if user-facing, to the Config form (`internal/web`).
