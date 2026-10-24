# Configuration reference

qbt-replicator reads one YAML file. You can edit it in the web UI (**Config** page: a form,
plus an **Advanced (YAML)** editor) or by hand. Saving in the UI validates the file, keeps the
previous version as `config.yaml.bak` and applies the change immediately. Only
`server.listen`, `server.base_path` and `log` need a restart.

- [Where the file lives](#where-the-file-lives)
- [Environment variables and secrets](#environment-variables-and-secrets)
- [`log`](#log) · [`server`](#server) · [`daemon`](#daemon) · [`instances`](#instances) · [`rules`](#rules)
- [How a rule decides what to replicate](#how-a-rule-decides-what-to-replicate)
- [Rule recipes](#rule-recipes)
- [Full example](#full-example)

## Where the file lives

First match wins:

1. `--config` / `-c` flag
2. `$QBTR_CONFIG`
3. `/config/config.yaml` (Docker default)
4. `./config.yaml`

If `serve` finds no file, it writes a minimal starter config there and runs in **setup
mode**: no instances, nothing happens, and the UI asks you to finish the configuration.
`qbt-replicator config init` writes the same starter file. `qbt-replicator config validate`
checks a file without starting anything.

## Environment variables and secrets

- **`${VAR}` in values** is replaced with the environment variable `VAR` when the file is
  loaded, e.g. `password: ${QBT1_PASSWORD}`. Expansion happens inside the parsed value, so
  passwords may contain any character (`"`, `#`, `:`, `$`, …).
- **`password_file:`** reads an instance password from a file (Docker secrets).
- **`QBTR_<SECTION>_<KEY>`** overrides a scalar setting, e.g. `QBTR_LOG_LEVEL=debug`,
  `QBTR_DAEMON_DRY_RUN=true`, `QBTR_DAEMON_INTERVAL=2s`, `QBTR_SERVER_LISTEN=:9090`.
  Overrides win over the file, and the Config page lists the active ones. Lists such as
  instances and rules can't be overridden this way.
- In the UI, literal passwords are never shown: the form leaves them empty ("unchanged") and
  the YAML editor shows `********`. Leave them as-is to keep the stored value.

Durations use Go syntax: `500ms`, `5s`, `2m`, `1h30m`. Sizes accept `1024`, `500MB`, `1.5GiB`.

## `log`

| Key | Default | Description |
|---|---|---|
| `log.level` | `info` | `debug`, `info`, `warn` or `error`. |
| `log.format` | `text` | `text` or `json` (for log collectors). |

## `server`

| Key | Default | Description |
|---|---|---|
| `server.listen` | `:8080` | Address of the web UI / API inside the container. Restart required. |
| `server.base_path` | `/` | Mount path behind a reverse proxy, e.g. `/replicator`. Restart required. |
| `server.auth.username` | empty | Enables HTTP basic auth when both username and password are set. `/healthz` and `/readyz` stay open. |
| `server.auth.password` | empty | |

The UI can change the configuration and remove torrents from instances. Set `server.auth`
if anyone else can reach it.

## `daemon`

| Key | Default | Description |
|---|---|---|
| `daemon.enabled` | `true` | Run the reconcile loop in `serve`. `serve --no-daemon` also disables it. |
| `daemon.interval` | `5s` | **Poll interval**: how often all instances are read and rules evaluated. Minimum `1s`. Each pass makes a few cheap API calls per instance (`torrents/info`, `transfer/info`), so `1s`–`5s` is fine on a LAN. |
| `daemon.concurrency` | `2` | Actions (add/remove/…) executed in parallel. |
| `daemon.dry_run` | `false` | Plan and log actions without changing anything. |
| `daemon.replica_tag` | `qbtr-replica` | Tag put on every torrent qbt-replicator adds. Tagged torrents are never sources, which prevents loops. Changing it makes existing replicas look like normal torrents. |
| `daemon.exclude_tag` | `qbtr-exclude` | Source torrents with this tag are never replicated by any rule. Set by **Remove & exclude** in the UI, `unreplicate --exclude` and the API. |
| `daemon.orphan_grace_ticks` | `2` | A replica whose source torrent is gone (e.g. Sonarr removed it) is removed only after it was seen orphaned on this many consecutive passes… |
| `daemon.orphan_grace_period` | `1m` | …**and** for at least this long, so a fast poll interval can't remove replicas hastily. Files are always kept, and nothing is removed while any instance is unreachable. |
| `daemon.state_file` | `state.json` next to the config | Persists orphan confirmations across restarts. `none` disables it. |

## `instances`

At least two, all seeing the downloads **at the same path** (or use `path_mappings`).
Use the **Test** button next to each instance on the Config page (or
`qbt-replicator instances test <name>`) to check the URL and credentials before saving. It
explains what's wrong: wrong password, connection refused, host not found, TLS, banned IP…

| Key | Default | Description |
|---|---|---|
| `name` | required | Unique name used in rules, e.g. `qbt-1`. `*` is reserved. |
| `url` | required | WebUI base URL as seen from the qbt-replicator container, e.g. `http://gluetun-1:8080` or `http://192.168.1.10:8080`. |
| `username` / `password` | empty | WebUI credentials. Leave both empty if the instance whitelists qbt-replicator's subnet ("Bypass authentication for clients in whitelisted IP subnets"). |
| `password_file` | | Read the password from this file instead. |
| `insecure_skip_verify` | `false` | Accept self-signed HTTPS certificates. |
| `timeout` | `15s` | Per-request timeout. |
| `path_mappings` | none | Only if containers mount the share at different paths. List of `{instance: /path/in/this/container, canonical: /shared/name}`; the longest prefix wins. |
| `export_fallback_dir` | | Only for qBittorrent < 4.5 (no `/torrents/export`): a mounted copy of that instance's `BT_backup` directory. |

## `rules`

A rule replicates **completed** torrents from its `sources` to its `targets`. Rules are
desired state: while a torrent matches a rule, each pass re-adds a missing replica. To keep a
single torrent off, use **Remove & exclude** or an `exclude_tags` filter.

| Key | Default | Description |
|---|---|---|
| `name` | `rule-N` | Shown in the UI and logs. |
| `sources` | required | Instances where torrents complete, or `["*"]` for any instance (mesh). Replicas are never sources. |
| `targets` | `["*"]` | Where to add replicas. `*` means every instance except the source. |
| `settle_delay` | `2m` | Wait this long after completion, so qBittorrent or Sonarr/Radarr can finish moving and renaming files first. `0s` replicates immediately. |
| `filter.categories` | any | Only torrents in one of these categories. |
| `filter.tags` | any | Only torrents with **at least one** of these tags. |
| `filter.exclude_tags` | none | Skip torrents with **any** of these tags. |
| `filter.trackers` | any | Only torrents whose current tracker URL contains one of these substrings (case-insensitive). |
| `filter.name_regex` | any | Only torrents whose **name** matches this [Go regular expression](https://pkg.go.dev/regexp/syntax). Prefix `(?i)` for case-insensitive. |
| `filter.exclude_name_regex` | none | Skip torrents whose name matches this regular expression. |
| `filter.save_paths` | any | Only torrents whose save path is under one of these prefixes, e.g. `/data/tv`. |
| `filter.min_size` | `0` | Skip torrents smaller than this, e.g. `1GiB`. |
| `filter.min_ratio` | `0` | Only torrents whose ratio on the source is at least this. |
| `filter.include_private` | **`false`** | Private-tracker torrents are skipped unless `true`. Many private trackers forbid seeding one account from several IPs, so check the rules first. |
| `add.category` | `inherit` | Category of the replica: `inherit` (same as the source), `""` (none) or a fixed name. Missing categories are created. |
| `add.extra_tags` | `[]` | Tags added to replicas besides the replica tag. |
| `add.skip_checking` | `true` | Trust the source (100% verified) and skip the hash check. `false` rechecks each replica, reading the data once per target. |
| `add.start` | `true` | Start replicas after adding. `false` leaves them stopped (they show 0% until started). |
| `remove_orphans` | `true` | Remove replicas whose source torrent no longer exists (files kept). |
| `follow_relocation` | `true` | If the source moves its files, re-add replicas at the new path. |
| `sync_share_limits` | `false` | Copy ratio / seeding-time limits from the source to its replicas. |

All filters of a rule must match: they combine with AND, and each list matches any of its
values (OR). When several rules match the same torrent, their targets add up.

### How a rule decides what to replicate

A torrent is replicated from source to target when all of these hold:

1. it is on a rule source and is **not** a replica, and doesn't carry `daemon.exclude_tag`;
2. it is 100% complete, in a seeding state (not checking or moving), and completed more than `settle_delay` ago;
3. it passes every filter of the rule;
4. the target is reachable and doesn't have the torrent yet.

The **Plan** page (and `qbt-replicator plan`) lists every completed torrent that is *not*
replicated, with the reason, e.g. *private torrent: set filter.include_private: true …* or
*completed 45s ago, waiting for settle_delay 2m*. **Replicate** with **force** on the Torrents
page ignores the filters for one torrent.

## Rule recipes

Instance names below are examples: `qbt-1` downloads, `qbt-2`/`qbt-3` are extra tunnels.

**Everything from one downloader to all other instances**
```yaml
rules:
  - name: main
    sources: [qbt-1]
    targets: ["*"]
```

**Any instance to all others (mesh).** Whatever completes anywhere is seeded everywhere.
Replicas never replicate again.
```yaml
rules:
  - name: mesh
    sources: ["*"]
    targets: ["*"]
```

**By category**: only TV and anime, to qbt-2 only.
```yaml
rules:
  - name: tv
    sources: [qbt-1]
    targets: [qbt-2]
    filter:
      categories: [tv-sonarr, anime]
```

**Different categories to different tunnels**: rules add up, so a torrent can match several.
```yaml
rules:
  - name: movies-to-2
    sources: [qbt-1]
    targets: [qbt-2]
    filter: {categories: [radarr]}
  - name: tv-to-3
    sources: [qbt-1]
    targets: [qbt-3]
    filter: {categories: [tv-sonarr]}
```

**By name**: only episodes (`S01E02`) and never samples.
```yaml
rules:
  - name: episodes
    sources: [qbt-1]
    filter:
      name_regex: '(?i)S\d{2}E\d{2}'
      exclude_name_regex: '(?i)\bsample\b'
```
Use single quotes in YAML so `\d` stays a regular-expression escape.

**By release group or resolution**
```yaml
    filter:
      name_regex: '(?i)(2160p|-FLUX|-NTb)'
```

**By folder (save path)**: only what lands under `/data/tv` or `/data/anime`.
```yaml
    filter:
      save_paths: [/data/tv, /data/anime]
```

**By tracker**
```yaml
    filter:
      trackers: [tracker.example.org, opentrackr]
```

**Opt-in or opt-out with tags.** Tag torrents in qBittorrent to choose per torrent.
```yaml
    filter:
      tags: [seed-everywhere]          # opt-in: only tagged torrents
      exclude_tags: [no-replicate]     # opt-out: never these
```

**Private trackers that allow several IPs**
```yaml
    filter:
      include_private: true
      trackers: [friendly-tracker.example]
```

**Only big torrents, after they proved popular**
```yaml
    filter:
      min_size: 4GiB
      min_ratio: 1.0
```

**Replicate fast, no settle time** (nothing moves files after completion)
```yaml
rules:
  - name: instant
    sources: [qbt-1]
    settle_delay: 0s
```

**Keep replicas when Sonarr/Radarr removes the source**
```yaml
    remove_orphans: false
```

**Replicas in their own category, stopped, for manual review**
```yaml
    add:
      category: replicas
      extra_tags: [from-qbt-1]
      start: false
```

## Full example

```yaml
log:
  level: info

server:
  listen: ":8080"
  auth:
    username: admin
    password: ${QBTR_UI_PASSWORD}

daemon:
  interval: 2s
  dry_run: false
  orphan_grace_period: 2m

instances:
  - name: qbt-1
    url: http://gluetun-1:8080
    username: admin
    password: ${QBT1_PASSWORD}
  - name: qbt-2
    url: http://gluetun-2:8080
    username: admin
    password: ${QBT2_PASSWORD}
  - name: qbt-3
    url: http://gluetun-3:8080
    username: admin
    password: ${QBT3_PASSWORD}

rules:
  - name: tv-and-movies
    sources: [qbt-1]
    targets: ["*"]
    settle_delay: 1m
    filter:
      categories: [tv-sonarr, radarr]
      exclude_tags: [no-replicate]
      exclude_name_regex: '(?i)\bsample\b'
```
