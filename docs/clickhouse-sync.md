---
title: ClickHouse Sync
description: Push the SQLite archive into ClickHouse and serve a read-only web UI from it
---

AgentsView stores sessions locally in SQLite. `agentsview clickhouse push`
copies those sessions into ClickHouse. `agentsview clickhouse serve` runs the
web UI by querying that copy. Keep the copy current with `push --watch` or
`clickhouse service`.

Each machine pushes its own sessions. SQLite stays the archive. ClickHouse is a
remote copy, the same operator story as [PostgreSQL sync](/docs/pg-sync/), not a
disposable local file like [DuckDB](/docs/duckdb/).

The UI includes the session browser, search, analytics, usage, activity, recent
edits, and project inventory. Rename, trash, insights, stars, and pins stay on
the SQLite archive; the ClickHouse UI does not write them. Semantic search works
when `[vector]` is enabled locally and the push includes vectors (see
[Vector Push](#vector-push)). Hosted raw sync is not part of this path.

## Quick Start

### 1. Configure ClickHouse

Add a `[clickhouse]` section to `~/.agentsview/config.toml`:

```toml
local_machine_name = "Laptop"

[clickhouse]
url = "clickhouse://user:pass@host:9440/agentsview?secure=true&compress=lz4"
```

`url` is a clickhouse-go DSN. Native protocol uses `clickhouse://` on port 9000
(or 9440 with TLS). HTTP uses `http://` or `https://`. The optional `database`
key overrides the database in the URL path; when both are empty the mirror uses
`agentsview`.

Use `compress=lz4` to compress native-protocol transfers. This is independent of
the codecs ClickHouse uses to compress columns on disk.

Sessions retain their source installation ID. `local_machine_name` supplies the
display label after a daemon restart and the next push. The optional
`[clickhouse].machine_name` defaults to the installation ID and only supplies a
key for legacy `local` rows; it does not rename recorded session keys.

For multiple ClickHouse destinations, use named `[clickhouse.NAME]` blocks and
`default_clickhouse` instead of the legacy single `[clickhouse]` block. Named
target names are normalized case-insensitively, and `all`, `local`, plus the
legacy `[clickhouse]` field names `url`, `database`, `machine_name`,
`allow_insecure`, `projects`, and `exclude_projects` are unavailable as
`[clickhouse.NAME]` names.

### 2. Push Sessions

```bash
agentsview clickhouse push
```

This one-shot command syncs all local sessions, messages, and tool calls to
ClickHouse. The schema is created automatically on first push.

To keep ClickHouse current automatically, run the foreground watcher:

```bash
agentsview clickhouse push --watch
```

Or install it as a per-user background service on macOS or Linux:

```bash
agentsview clickhouse service install
```

### 3. Serve the Dashboard

```bash
agentsview clickhouse serve
```

Opens the read-only web UI at `http://127.0.0.1:8080`, backed entirely by
ClickHouse. It does not use local SQLite or file watching, and its session UI
and APIs do not accept session uploads.

______________________________________________________________________

## Commands

### `agentsview clickhouse push`

Sync sessions from the local SQLite database to ClickHouse.

```bash
agentsview clickhouse push [target] [flags]
```

| Flag                 | Default | Description                                                               |
| -------------------- | ------- | ------------------------------------------------------------------------- |
| `--all`              | `false` | Push every configured ClickHouse target sequentially                      |
| `--full`             | `false` | Force full local resync and re-push, bypassing change detection           |
| `--projects`         |         | Comma-separated projects to push (inclusive)                              |
| `--exclude-projects` |         | Comma-separated projects to exclude                                       |
| `--all-projects`     | `false` | Ignore configured project filters for this run                            |
| `--watch`            | `false` | Run continuously, pushing on change plus a periodic floor                 |
| `--debounce`         | `30s`   | Coalesce window after a filesystem change before pushing (`--watch` only) |
| `--interval`         | `15m`   | Periodic floor push interval (`--watch` only)                             |
| `--no-vectors`       | `false` | Skip the semantic-search vector phase for this run                        |

Without `--watch`, push is on-demand. With `--watch`, the command stays in the
foreground and keeps pushing until interrupted.

When no target is passed, `clickhouse push` uses the effective default target.
Pass one named target explicitly to push just that destination, or use `--all`
to fan out across every configured target. `--all --watch` is rejected.

**What happens on push:**

1. Runs a local sync to pick up any new or modified session files.
1. Compares local sessions against the ClickHouse fingerprint to find what
   changed since the last push.
1. Rewrites changed sessions: dependent rows first (messages, tool calls, usage,
   secrets, pins), then a version-bounded delete of stale rows, then the
   session rows.
1. Pushes the machine's embedding generation when `[vector]` is enabled: see
   [Vector Push](#vector-push).
1. Advances this archive's cursor in the mirror's `sync_metadata` only when
   every session succeeded.

Use `--full` to bypass fingerprints and re-push everything — for example after a
schema reset or when message content was rewritten in place.

If any sessions fail to push, the cursor is not advanced so they are retried on
the next run. The exit code is 1 when any errors occur, 0 otherwise.

#### Automatic Push Watcher

`agentsview clickhouse push --watch` runs a long-lived auto-push daemon in the
foreground:

```bash
agentsview clickhouse push --watch
agentsview clickhouse push --watch --debounce 1m
agentsview clickhouse push --watch --interval 5m
```

The watcher performs one initial local sync plus ClickHouse push, then pushes
again after session-directory changes settle for the debounce window. The
interval acts as a floor: even if filesystem events are missed, the next
interval push catches up.

Operational details:

- Only one ClickHouse watcher can run per AgentsView data directory; a runtime
  lock (`clickhouse-watch`) prevents competing pushes from racing cursors.
- ClickHouse connections are opened lazily and reset after errors, so a
  transiently unavailable database is retried on the next trigger instead of
  crashing the watcher.
- On shutdown (`Ctrl+C`, `SIGTERM`), the watcher attempts one bounded final
  flush.
- Logs are written to `clickhouse-watch.log` under the AgentsView data
  directory.
- The watcher uses the selected ClickHouse target, or the `default_clickhouse`
  target when no name is passed, along with the same machine name and project
  filters as one-shot `clickhouse push`.

#### Project Filtering

By default, `clickhouse push` syncs all projects. Use project filters to push a
subset:

```bash
# Push only these projects
agentsview clickhouse push --projects alpha,beta

# Push everything except this project
agentsview clickhouse push --exclude-projects scratch

# Ignore config-file filters for this run
agentsview clickhouse push --all-projects
```

`--projects` and `--exclude-projects` are mutually exclusive. `--all-projects`
cannot be combined with either.

Project filters can also be set in `config.toml`:

```toml
[clickhouse]
url = "clickhouse://..."
projects = ["alpha", "beta"]
# or: exclude_projects = ["scratch"]
```

CLI flags override config values. Use
[`agentsview projects`](/docs/commands/#agentsview-projects) to list available
project names.

#### Vector Push

When `[vector]` is enabled locally, `clickhouse push` runs a vector phase after
the session phase, copying the machine's active embedding generation from
`vectors.db` into ClickHouse so `clickhouse serve` can answer
`--semantic`/`--hybrid`. Only changed sessions are re-sent; a session's vectors
follow its session row, so a session that leaves the push scope loses its
vectors with it. A watch push scoped to changed sessions widens itself to the
whole generation until this machine has completed one clean generation-wide
pass, so an interrupted first push finishes on the next push. Skip the phase for
one run with `--no-vectors`, or disable it persistently with
`push_vectors = false` under `[clickhouse]`. The push summary reports the phase
as `Vectors: N session(s) pushed, ...` or `Vectors: skipped (<reason>)`. See
[semantic search: ClickHouse](/docs/semantic-search/#clickhouse) for how serve
matches a pushed generation.

#### Enabling Semantic Search

Follow these steps on the machine that owns the sessions. Every step names the
output that confirms it, so an operator or an agent can run the sequence
unattended and stop at the first step that does not confirm.

1. Enable vectors in `~/.agentsview/config.toml`. `model`, `dimension`, and one
   `[vector.embeddings.servers.<name>]` entry with an `endpoint` are required;
   the full key list is in
   [Enabling `[vector]`](/docs/semantic-search/#enabling-vector).

    ```toml
    [vector]
    enabled = true

    [vector.embeddings]
    model = "nomic-embed-text"
    dimension = 768
    default_server = "local"

    [vector.embeddings.servers.local]
    endpoint = "http://localhost:11434/v1"
    ```

1. Build the local index and confirm an active generation exists:

    ```bash
    agentsview embeddings build --yes
    agentsview embeddings list
    ```

    `embeddings list` prints one row per generation with a `STATE` column; the
    generation to push shows `active` with `MISSING` at `0`. Note its
    `FINGERPRINT` prefix.

1. Push to ClickHouse and confirm the vector phase ran:

    ```bash
    agentsview clickhouse push
    ```

    The summary ends with
    `Vectors: N session(s) pushed, M unchanged, D docs, C chunks`.
    `Vectors: skipped (<reason>)` means the phase did not run; the reason names
    the cause (`[vector]` disabled, no active local generation, or the local
    index not ready). A `Warning: deferred vectors` line means the next push
    finishes the remaining sessions.

1. Serve and confirm the searcher attached. Run this on the machine that will
   serve; its `[vector]` and `[vector.embeddings]` config must match the
   pushing machine's, and its embeddings endpoint must be reachable because
   queries are embedded at search time.

    ```bash
    agentsview clickhouse serve --no-browser
    ```

    The startup log contains
    `clickhouse serve: semantic search enabled (fingerprint <fp>, model <model>)`.
    A line starting
    `clickhouse serve: semantic search: ClickHouse has no embedding generation matching fingerprint`
    means the serving config differs from the pushed one; the same line lists
    the fingerprints ClickHouse does hold, and serve still starts with lexical
    search only.

1. Run one semantic search against the server:

    ```bash
    agentsview session search --server http://127.0.0.1:8080 --semantic "your query"
    ```

    Ranked hits confirm the setup. An HTTP 501 carrying the reason from the
    previous step means the searcher did not attach.

Keeping vectors current is two jobs. Embedding new sessions happens locally: the
`agentsview` daemon does it after each sync while `[vector.embed]`
`run_after_sync` is on (the default), and without a running daemon you run
`agentsview embeddings build --yes` before each push. Replicating the result is
the vector phase inside every `clickhouse push`, so the same watcher or service
as the session push (`clickhouse push --watch` or `clickhouse service install`)
carries new vectors once they exist locally. There is no separate vector command
to schedule, and the watcher never generates embeddings itself.

### `agentsview clickhouse status`

Show the current sync state.

```bash
agentsview clickhouse status [target] [flags]
agentsview clickhouse status --all
agentsview clickhouse status --projects alpha,beta
```

Without a target name, `clickhouse status` uses the effective default target.
Pass one named target explicitly to inspect that destination, or use `--all` to
print every configured target sequentially.

Use the same `--projects`, `--exclude-projects`, or `--all-projects` filter
flags as `clickhouse push`.

### `agentsview clickhouse service`

Install and manage the [`clickhouse push --watch`](#automatic-push-watcher)
auto-push daemon as a per-user OS service.

```bash
agentsview clickhouse service install
agentsview clickhouse service status
agentsview clickhouse service logs -f
agentsview clickhouse service stop
agentsview clickhouse service start
agentsview clickhouse service uninstall
```

Supported service managers:

| Platform | Manager             |
| -------- | ------------------- |
| macOS    | launchd LaunchAgent |
| Linux    | `systemd --user`    |

The generated unit runs `agentsview clickhouse push --watch`, pins
`AGENTSVIEW_DATA_DIR` to the data directory used at install time, and writes
logs to `~/.agentsview/clickhouse-watch.log` unless you changed the data
directory.

`install` requires a literal ClickHouse URL in the effective default target of
`~/.agentsview/config.toml`, either the legacy `[clickhouse].url` or the target
selected by `default_clickhouse` from named `[clickhouse.NAME]` blocks. It
rejects `AGENTSVIEW_CLICKHOUSE_URL` and environment-expanded URLs because
background services do not inherit your interactive shell environment. Put
persistent settings in `config.toml` before installing.

On headless Linux machines, `systemd --user` services stop at logout and do not
start at boot unless user lingering is enabled. If lingering is disabled,
`install` prints the exact `loginctl enable-linger "$USER"` command and offers
to run it.

### `agentsview clickhouse serve`

Start a read-only web UI backed by ClickHouse.

```bash
agentsview clickhouse serve [flags]
```

Accepts the same serve flags as [`pg serve`](/docs/pg-sync/#agentsview-pg-serve)
(`--host`, `--port`, `--base-path`, proxy and TLS flags). The session server is
read-only: session uploads, file watching, and local sync are disabled. Sessions
from all machines appear in a single unified view.

On startup, `clickhouse serve` applies the current schema if the ClickHouse role
can create tables. If the role is read-only, that step is skipped and the server
falls back to the schema compatibility check.

The first push or serve after upgrading to this schema also fills two derived
tables before serving: `usage_messages` and `terminal_event_snapshots`. The
Activity report reads these instead of parsing usage JSON and scanning tool
events on every request. The fill runs once per mirror, keeps the source tables
unchanged, and resumes from the start if interrupted. Until it completes, a
read-only serve role fails the compatibility check with a message naming the
missing fill; run `agentsview clickhouse push` with a role that can create
tables to finish it.

The serve role also needs `GRANT SELECT ON system.parts`. The Activity report
checks whether the mirror changed by hashing the active parts of its source
tables before it rescans them. With the default server setting
`select_from_system_db_requires_grant`, a role granted only
`SELECT ON <database>.*` can read `system.columns` but not `system.parts`, and
serve refuses to start with an error naming the missing grant. An administrator
must grant it to the serve user. For users managed in XML, add
`<query>GRANT SELECT ON system.parts</query>` to that user's `<grants>` in the
deployment configuration and run `SYSTEM RELOAD USERS`. SQL `GRANT` cannot
modify XML-managed users, and schema upgrades cannot grant a permission the
application account lacks.

When `require_auth` is enabled, a bearer token is generated if needed and
printed on startup. Pass it via `Authorization: Bearer <token>` on API requests.

`clickhouse serve` does **not** expose the global live-refresh event stream used
by normal `agentsview serve`, because there is no local sync engine attached to
the server.

______________________________________________________________________

## Configuration

| Key                  | Description                                                           |
| -------------------- | --------------------------------------------------------------------- |
| `url`                | ClickHouse DSN (`clickhouse://`, `http://`, or `https://`)            |
| `database`           | Database name; overrides the DSN path; default `agentsview`           |
| `machine_name`       | Optional machine key for this pusher                                  |
| `allow_insecure`     | Allow plaintext or unverified TLS to a non-loopback host              |
| `projects`           | Inclusive project filter                                              |
| `exclude_projects`   | Exclusive project filter                                              |
| `push_vectors`       | Run the vector phase on push; default `true`                          |
| `default_clickhouse` | Named target used when more than one `[clickhouse.NAME]` block exists |

Environment overrides for the effective default target:

| Variable                         | Maps to        |
| -------------------------------- | -------------- |
| `AGENTSVIEW_CLICKHOUSE_URL`      | `url`          |
| `AGENTSVIEW_CLICKHOUSE_DATABASE` | `database`     |
| `AGENTSVIEW_CLICKHOUSE_MACHINE`  | `machine_name` |

Protect the config file, since it holds credentials:

```bash
chmod 600 ~/.agentsview/config.toml
```

Named-target example:

```toml
default_clickhouse = "work"

[clickhouse.work]
url = "clickhouse://user:pass@work-db:9440/agentsview?secure=true"

[clickhouse.archive]
url = "clickhouse://user:pass@archive-db:9440/agentsview?secure=true"
exclude_projects = ["scratch"]
```

`AGENTSVIEW_CLICKHOUSE_URL`, `AGENTSVIEW_CLICKHOUSE_DATABASE`, and
`AGENTSVIEW_CLICKHOUSE_MACHINE` still work, but in named-target mode they apply
only to the effective default target. They do not rewrite every named
`[clickhouse.NAME]` entry.

`clickhouse serve` and `clickhouse service` always use the effective default
target. In named-target mode, set `default_clickhouse` to choose which target
those long-running commands use.

### Transport security

Non-loopback URLs must use TLS unless `allow_insecure = true`. For the native
protocol, add `secure=true` to the URL (typical TLS port 9440). For HTTP, use an
`https://` URL. `skip_verify=true` is rejected on a non-loopback host unless
`allow_insecure` is set. Loopback (`127.0.0.1`, `localhost`) may stay plaintext
or skip verification.

______________________________________________________________________

## Differences from PostgreSQL

- ClickHouse has no schema name; tables live in a database (`database` key or
  the DSN path).
- Vectors live in shared `Array(Float32)` tables keyed by config fingerprint
  rather than per-generation pgvector tables, and search is an exact cosine
  scan. There is no hosted raw-sync control plane.
- `clickhouse serve` does not run PostgreSQL-style migrations beyond
  `CREATE TABLE IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`, and the one-time
  fill of derived usage and terminal-event tables described above.
- Deletes are version-bounded `DELETE` statements. ClickHouse applies them at
  merge time; readers always open connections with `final=1` so they see one
  row per key without writing `FINAL` in query text.
- Every mirrored table is `ReplacingMergeTree(push_version)`. The highest
  `push_version` for an ordering key wins.

______________________________________________________________________

## ClickHouse notes

The mirror is designed for MergeTree, not copied from PostgreSQL SQL.

- **ReplacingMergeTree.** Each table is ordered by its natural key and versioned
  with `push_version`. A later push of the same key supersedes the earlier
  row.
- **Deletes.** A push inserts the current dependents, then
  `DELETE ... WHERE session_id IN (...) AND push_version < v`, then writes
  session rows. Until a merge runs, both versions can exist on disk; `final=1`
  hides the stale ones from readers.
- **No `FINAL` in queries.** The connection setting covers every statement.
  Putting `FINAL` in query text is rejected by this design.
- **Lightweight deletes** need ClickHouse 23.3+; the integration suite pins
  `clickhouse/clickhouse-server:26.8`.
