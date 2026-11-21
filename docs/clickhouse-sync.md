---
title: ClickHouse Sync
description: Push the SQLite archive into ClickHouse and serve a read-only web UI from it
---

AgentsView stores your sessions locally in SQLite. You can copy them to
ClickHouse with `agentsview replica clickhouse push` and run a read-only web UI
against that copy with `agentsview replica clickhouse serve`. To keep the copy
current, run `push --watch` or install `clickhouse service`.

Each machine pushes its own sessions. SQLite stays the archive, and ClickHouse
is a remote copy with the same operator workflow as
[PostgreSQL sync](/docs/pg-sync/).

The ClickHouse UI includes the session browser, search, analytics, usage,
activity, recent edits, and project inventory. Rename, trash, insights, stars,
and pins stay on the SQLite archive, and the ClickHouse UI does not write them.
Semantic search works when you enable `[vector]` locally and include vectors in
your push. See [Vector push](#vector-push). Hosted raw sync is not part of this
path.

In substring and regex modes, content search matches dialogue when you pass
`--in messages` and reasoning when you pass `--in thinking`. Both modes include
reasoning by default; pass `--in messages` to skip it. If a regex has no literal
prefix, search loads every in-scope body from the selected sources into memory
before matching and paginating. Hybrid keyword search matches dialogue, and
semantic search uses rendered messages.

The first push from each archive after upgrading to a binary that stores
dialogue is a full push. It re-sends every session in your configured mirror
scope, including sessions without dialogue, and needs a role that can update the
ClickHouse schema. Until the schema has `messages.dialogue_text`, reads fail
with a schema compatibility error naming that column.

The Activity, Session Shape, Velocity, Tools, Skills, and Signals panels on the
Analytics page read per-session counts that `push` stores with each session, so
they no longer scan every message on each load. After you upgrade, the first
push from each archive is a full push that republishes each session row still
stored in the older format, reported as
`republishing session rows from an older format`. Until that push finishes,
those panels count the remaining sessions from their messages and return the
same results. `serve` reports a missing-column error until a push from the new
binary updates the schema.

Commit lookups return the copied links and prompts. After you upgrade, the first
push is a full push that copies existing commit links. Upgrade serving binaries
before or alongside publishers; older readers cannot see the new table. If a
serving role cannot create tables, run an upgraded push before commit lookups
can work. When an archive rebuilds, it removes only the deleted-session links
that it pushed. Links from older pushes with no recorded archive stay in
ClickHouse and can reappear after an archive discards its matching copy. A
deleted session remains visible as a missing record. Project filters use the
project saved with its links in SQLite, including after a rebuild or a deletion
before the first push.

When you enable [`[image_text]`](configuration.md#image-text) locally, search
matches text extracted from tool-result images. After you upgrade, the first
push is a full push that copies existing image text.

## Quick start

### 1. Configure ClickHouse

Add a `[clickhouse]` section to `~/.agentsview/config.toml`:

```toml
local_machine_name = "Laptop"

[clickhouse]
url = "clickhouse://user:pass@host:9440/agentsview?secure=true&compress=lz4"
```

`url` is a clickhouse-go DSN. The native protocol uses `clickhouse://` on port
9000, or 9440 with TLS. HTTP uses `http://` or `https://`. You can set the
optional `database` key to override the database in the URL path; when both are
empty, the mirror uses `agentsview`.

Add `compress=lz4` to compress native-protocol transfers. This setting is
independent of the codecs ClickHouse uses to compress columns on disk.

Sessions keep their source installation ID. `local_machine_name` supplies the
display label after a daemon restart and the next push. The optional
`[clickhouse].machine_name` defaults to the installation ID and only supplies a
key for legacy `local` rows; it does not rename recorded session keys.

If you push to multiple ClickHouse destinations, configure named
`[clickhouse.NAME]` blocks and `default_clickhouse` instead of the legacy single
`[clickhouse]` block. Target names are normalized case-insensitively. You cannot
use `all`, `local`, or the legacy `[clickhouse]` field names `url`, `database`,
`machine_name`, `allow_insecure`, `projects`, `exclude_projects`,
`push_vectors`, and `push_batch_size` as `[clickhouse.NAME]` names.

### 2. Push sessions

```bash
agentsview replica clickhouse push
```

This one-shot command syncs all local sessions, messages, and tool calls to
ClickHouse, and creates the schema automatically on the first push.

To keep ClickHouse current automatically, run the foreground watcher:

```bash
agentsview replica clickhouse push --watch
```

You can also install it as a per-user background service on macOS or Linux:

```bash
agentsview replica clickhouse service install
```

### 3. Serve the dashboard

```bash
agentsview replica clickhouse serve
```

This command opens the read-only web UI at `http://127.0.0.1:8080`, backed
entirely by ClickHouse. It does not use local SQLite or file watching, and its
session UI and APIs do not accept session uploads.

______________________________________________________________________

## Commands

<a id="agentsview-clickhouse-push"></a>

### `agentsview replica clickhouse push`

Sync sessions from the local SQLite database to ClickHouse.

```bash
agentsview replica clickhouse push [target] [flags]
```

| Flag                 | Default | Description                                                               |
| -------------------- | ------- | ------------------------------------------------------------------------- |
| `--all`              | `false` | Push every configured ClickHouse target sequentially                      |
| `--full`             | `false` | Force full local resync and re-push, bypassing change detection           |
| `--projects`         |         | Comma-separated projects to push (inclusive)                              |
| `--exclude-projects` |         | Comma-separated projects to exclude                                       |
| `--all-projects`     | `false` | Ignore configured project filters for this run                            |
| `--watch`            | `false` | Run continuously, pushing on change plus a periodic floor                 |
| `--no-start-daemon`  | `false` | With `--watch`, require a writable daemon; exit if none is running        |
| `--debounce`         | `30s`   | Coalesce window after a filesystem change before pushing (`--watch` only) |
| `--interval`         | `15m`   | Periodic floor push interval (`--watch` only)                             |
| `--no-vectors`       | `false` | Skip the semantic-search vector phase for this run                        |

Without `--watch`, `clickhouse push` runs once on demand. With `--watch`, it
stays in the foreground and keeps pushing until you stop it.

If you omit a target name, `clickhouse push` uses the effective default target.
Pass a named target to push only to that destination, or pass `--all` to fan out
across every configured target. Combining `--all` with `--watch` is rejected.

During a push, AgentsView performs these steps:

1. Runs a local sync to pick up any new or modified session files.
1. Compares local sessions against the ClickHouse fingerprint to find what
   changed since the last push.
1. Rewrites changed sessions by inserting dependent rows first, including
   messages, tool calls, usage, secrets, and pins, running a version-bounded
   delete of stale rows, and then writing the session rows.
1. Pushes the machine's embedding generation when `[vector]` is enabled. See
   [Vector push](#vector-push).
1. Advances this archive's cursor in the mirror's `sync_metadata` only when
   every session succeeds.
1. Compacts the rewritten tables when the push rewrote at least a quarter of the
   mirror's live messages, as a `--full` push does. See
   [Compaction after a large push](#compaction-after-a-large-push).

Pass `--full` to bypass fingerprints and re-push everything, for example after a
schema reset or when message content was rewritten in place.

Every push sends the most recently active sessions first, so recent activity
reaches the mirror before older history. If a full push stops partway, for
example because the process restarted, the next push resumes from where it left
off and skips sessions that already succeeded. The summary reports
`resuming an unfinished full push` and counts the skipped sessions. The push
starts over instead if you pass `--full`, or if the archive was rebuilt, the
push scope changed, or the mirror format changed since it began. When `--full`
discards an unfinished full push, the summary says
`discarded an unfinished full push`. If a full push reaches the end with session
errors, it resumes only while something else still requires a full push.
Otherwise, the next push is incremental: it picks up failed sessions that
changed since the last clean push and skips the rest. If a `--full` push ends
with errors, run it again.

If a full push ended with session errors and began under a different push scope,
archive, or mirror format, the next push is also full. Pushes stay full until
one finishes without errors. When you restore a wider scope after a filtered
push, this brings back sessions that the filtered push removed from the mirror.
The push log names each session that failed, and pushes return to incremental
once those sessions push cleanly.

If any sessions fail during an incremental push, the cursor does not advance, so
the next push retries them. The exit code is 1 when any errors occur, and 0
otherwise.

#### Compaction after a large push

When a session is rewritten, ClickHouse keeps its older rows on disk until a
merge removes them, and after full pushes those merges rarely happen on their
own. Every dashboard read then has to skip the older copies. When a push rewrote
at least a quarter of the mirror's live messages, it runs
`OPTIMIZE TABLE ... FINAL` on each rewritten table, largest first, and waits for
each merge to finish before returning. The push log names each table and how
long its merge took. If a full push resumes after an interruption, it counts the
messages its earlier attempt wrote, so it compacts like a full push that ran
straight through. A push that ends with session errors skips compaction; the
push that later finishes cleanly compacts what both wrote.

Reads keep working during the merge, though they compete with it for CPU and
disk. ClickHouse starts the merge only when the disk has free space for twice
the size of the table it merges; otherwise, the push logs the refusal.

The push role needs the `OPTIMIZE` grant on the mirror database and
`SELECT ON system.parts`. Without them, the push logs
`skipped compacting the rewritten tables` with the server's error and still
succeeds; reads stay correct, but they slow down as older copies pile up. To
grant them, run `GRANT OPTIMIZE ON <database>.* TO <push_user>` and
`GRANT SELECT ON system.parts TO <push_user>`, or add the matching `<query>`
lines to an XML-managed user's `<grants>`.

#### Automatic push watcher

`agentsview replica clickhouse push --watch` runs a long-lived auto-push daemon
in the foreground:

```bash
agentsview replica clickhouse push --watch
agentsview replica clickhouse push --watch --debounce 1m
agentsview replica clickhouse push --watch --interval 5m
```

The watcher runs one initial local sync and ClickHouse push, then pushes again
after changes in the session directory settle for the debounce window. The
interval acts as a floor: even if filesystem events are missed, the next
interval push catches up. When a push triggered by file changes finds no new
local changes, it skips session work, though it can still bring search vectors
up to date. Model prices, machine labels, and the removal of mirror sessions the
archive no longer holds then wait for the next push that runs, at most one interval
later.

Operational details:

- Only one ClickHouse watcher can run per AgentsView data directory. A runtime
  lock named `clickhouse-watch` prevents competing pushes from racing cursors.
- The watcher opens ClickHouse connections lazily and resets them after errors,
  so if the database is temporarily unavailable, it retries on the next trigger
  instead of crashing.
- On shutdown with `Ctrl+C` or `SIGTERM`, the watcher attempts one bounded final
  flush.
- The watcher writes logs to `clickhouse-watch.log` under the AgentsView data
  directory.
- The watcher uses the ClickHouse target you pass, or `default_clickhouse` when
  no name is passed, along with the same machine name and project filters as
  one-shot `clickhouse push`.

#### Project filtering

By default, `clickhouse push` syncs all projects. To push a subset, use project
filters:

```bash
# Push only these projects
agentsview replica clickhouse push --projects alpha,beta

# Push everything except this project
agentsview replica clickhouse push --exclude-projects scratch

# Ignore config-file filters for this run
agentsview replica clickhouse push --all-projects
```

You cannot combine `--projects` and `--exclude-projects`, and you cannot combine
`--all-projects` with either one.

You can also set project filters in `config.toml`:

```toml
[clickhouse]
url = "clickhouse://..."
projects = ["alpha", "beta"]
# or: exclude_projects = ["scratch"]
```

CLI flags override config values. To list available project names, run
[`agentsview project list`](/docs/commands/#agentsview-projects).

#### Push batch size

`clickhouse push` writes changed sessions in batches of 100. Each batch ends
with one delete per session table. Under ClickHouse's default
`lightweight_delete_mode`, each delete rewrites every active part of its table,
even when no rows match. Fewer batches mean fewer rewrites, which matters most
for full pushes and for servers on slow storage.

To raise the batch size per target, set `push_batch_size`:

```toml
[clickhouse]
url = "clickhouse://localhost:9000/agentsview"
push_batch_size = 500
```

Larger batches hold more session content in memory at once. On a test archive of
about 46,800 sessions, a full push peaked at about 2.8 GB of memory at the
default of 100, 3.4 GB at 500, 4.5 GB at 1,000, and 7.5 GB at 2,000, while the
batch phase took 510 s, 206 s, 135 s, and 142 s. Peak memory depends on the
largest sessions that land in one batch, so leave headroom. The value must be at
least 1; remove the key to use the default. A delegated push sends the target's
value to the daemon, so the setting applies there too.

#### Vector push

When you enable `[vector]` locally, `clickhouse push` runs a vector phase after
the session phase. It copies the machine's active embedding generation from
`vectors.db` into ClickHouse so `clickhouse serve` can answer `--semantic` and
`--hybrid` searches. Only changed sessions are re-sent. A session's vectors
follow its session row, so a session that leaves the push scope loses its
vectors with it. A watch push scoped to changed sessions widens to the whole
generation until this machine completes one clean generation-wide pass, so an
interrupted first push finishes on the next push. To skip the phase for one run,
pass `--no-vectors`. To disable it persistently, set `push_vectors = false`
under `[clickhouse]`. The push summary reports the phase as
`Vectors: N session(s) pushed, ...` or `Vectors: skipped (<reason>)`. See
[semantic search: ClickHouse](/docs/semantic-search/#clickhouse) for how `serve`
matches a pushed generation.

#### Enabling semantic search

On the machine that owns the sessions, go to
**Settings → Embeddings → Turn on** to approve the download and build the local
index without restarting. To set it up from the CLI instead, follow these steps:

1. Build the local index. AgentsView discovers Ollama with Nomic or uses Kit's
   shared local server. For other providers, see
   [Enabling `[vector]`](/docs/semantic-search/#enabling-vector). The CLI asks
   before the first download, and declining stops the download and build.
   Passing `--yes` skips rebuild confirmation but does not approve this
   download; approve the first install interactively before running unattended
   builds. Confirm that an active generation exists:

    ```bash
    agentsview index embeddings build --yes
    agentsview index embeddings list
    ```

    `embeddings list` prints one row per generation with a `STATE` column. The
    generation to push shows `active` with `MISSING` at `0`. Note its
    `FINGERPRINT` prefix.

1. Push to ClickHouse and confirm the vector phase ran:

    ```bash
    agentsview replica clickhouse push
    ```

    The summary ends with
    `Vectors: N session(s) pushed, M unchanged, D docs, C chunks`. If you see
    `Vectors: skipped (<reason>)`, the phase did not run, and the reason names
    the cause: `[vector]` disabled, no active local generation, or the local
    index not ready. A `Warning: deferred vectors` line means the next push
    finishes the remaining sessions.

1. Start the server and confirm the searcher attached. Run this command on the
   serving machine. Its `[vector]` and `[vector.embeddings]` config must match
   the pushing machine's, and its embeddings endpoint must be reachable because
   `serve` embeds queries at search time.

    ```bash
    agentsview replica clickhouse serve --no-browser
    ```

    The startup log contains
    `clickhouse serve: semantic search enabled (fingerprint <fp>, model <model>)`.
    If you see a line starting with
    `clickhouse serve: semantic search: ClickHouse has no embedding generation matching fingerprint`,
    your serving config differs from the pushed one. The same line lists the
    fingerprints ClickHouse holds, and `serve` still starts with lexical search only.

1. Run a semantic search against the server:

    ```bash
    agentsview session search --server http://127.0.0.1:8080 --semantic "your query"
    ```

    Ranked hits confirm the setup. An HTTP 501 carrying the reason from the
    previous step means the searcher did not attach.

Keeping vectors current is two jobs. First, new sessions are embedded locally:
the `agentsview` daemon does it after each sync while `[vector.embed]`
`run_after_sync` is on, which is the default. Without a running daemon, run
`agentsview index embeddings build --yes` before each push. Second, every
`clickhouse push` replicates the result in its vector phase, so the same watcher
or service you use for sessions, `clickhouse push --watch` or
`clickhouse service install`, carries new vectors once they exist locally. There
is no separate vector command to schedule, and the watcher never generates
embeddings itself.

<a id="agentsview-clickhouse-status"></a>

### `agentsview replica clickhouse status`

Check the current sync state:

```bash
agentsview replica clickhouse status [target] [flags]
agentsview replica clickhouse status --all
agentsview replica clickhouse status --projects alpha,beta
```

If you omit a target name, `clickhouse status` uses the effective default
target. Pass a named target to inspect that destination, or pass `--all` to
print every configured target sequentially.

You can pass the same `--projects`, `--exclude-projects`, or `--all-projects`
filter flags as `clickhouse push`.

When the [push watcher](#automatic-push-watcher) runs on this machine, `status`
also shows what it last did. The watcher records each push in
`clickhouse-watch-health.json` in the data directory. `Watcher last success:`
shows when the last push that worked ended, and `Watcher failing since:` and
`Watcher last error:` appear while pushes are failing.

To alert on a stalled watcher, run `status` with `--max-stale` from a timer or
monitor:

```bash
agentsview replica clickhouse status --max-stale 10m
```

The command exits non-zero and names the cause in any of these cases:

- Every push has failed for longer than the limit, for any reason. A watcher
  that exits during a push, or exits with an error before its first push, counts
  as a failed push, so a crash loop or restart loop fails the check too.
- A push started more than the limit ago and has not finished.
- No push has finished for longer than the `--interval` floor plus the limit.
  The watcher pushes at least once per interval even when nothing changed, so
  this catches a watcher that stopped, or one whose pushes wait on a local sync
  that never finishes.

The check reads only what the watcher recorded about its own pushes and does not
inspect the archive to guess what a push would send. A failure or a push that is
still running gets the full limit to recover before the check fails. With the
default 15-minute interval, `--max-stale 10m` fails after 25 minutes without a
finished push.

When the record already shows a stall, `status` reports it without querying the
replica, because a stuck push can hold locks that block the status query.
Otherwise, the replica gets one minute to answer, and the check fails if it does
not.

The check uses wall-clock time. After a machine wakes from a sleep longer than
the interval plus the limit, the check fails until the watcher's next push:
within the debounce window after a session changes, or within one interval
otherwise. On a laptop, alert only when the check fails twice in a row, spaced
at least one interval apart.

`--max-stale` fails without a watcher record for the target, because nothing
reports what pushes did. It also fails when the record was written before the
target's URL or schema changed, until a watcher using the new settings starts.

<a id="agentsview-clickhouse-service"></a>

### `agentsview replica clickhouse service`

Install and manage the [`clickhouse push --watch`](#automatic-push-watcher)
auto-push daemon as a per-user OS service:

```bash
agentsview replica clickhouse service install
agentsview replica clickhouse service status
agentsview replica clickhouse service logs -f
agentsview replica clickhouse service stop
agentsview replica clickhouse service start
agentsview replica clickhouse service uninstall
```

Supported service managers:

| Platform | Manager             |
| -------- | ------------------- |
| macOS    | launchd LaunchAgent |
| Linux    | `systemd --user`    |

The generated unit runs `agentsview replica clickhouse push --watch`, pins
`AGENTSVIEW_DATA_DIR` to the data directory used at install time, and writes
logs to `~/.agentsview/clickhouse-watch.log` unless you changed the data
directory.

If you run your own collector service, install with
`agentsview replica clickhouse service install --no-start-daemon`. The push
service exits if no writable daemon is running, and the service manager restarts
it until the collector answers. It retries pushes while the collector restarts,
without starting a daemon or opening the archive directly. Reinstalling restarts
the service with the new flag. The default installation still starts a missing
daemon for hosts that run only the push service.

`install` requires a literal ClickHouse URL in the effective default target of
`~/.agentsview/config.toml`, either the legacy `[clickhouse].url` or the target
selected by `default_clickhouse` from named `[clickhouse.NAME]` blocks. It
rejects `AGENTSVIEW_CLICKHOUSE_URL` and environment-expanded URLs because
background services do not inherit your interactive shell environment. Put
persistent settings in `config.toml` before installing.

On headless Linux machines, `systemd --user` services stop at logout and do not
start at boot unless user lingering is enabled. If lingering is disabled,
`install` prints the exact `loginctl enable-linger` command for your user and
offers to run it.

<a id="agentsview-clickhouse-serve"></a>

### `agentsview replica clickhouse serve`

Start a read-only web UI backed by ClickHouse:

```bash
agentsview replica clickhouse serve [flags]
```

This command accepts the same serve flags as
[`pg serve`](/docs/pg-sync/#agentsview-pg-serve): `--host`, `--port`,
`--base-path`, and the proxy and TLS flags. The session server is read-only,
with session uploads, file watching, and local sync disabled. Sessions from all
machines appear in a single unified view.

On startup, `clickhouse serve` applies the current schema if its ClickHouse role
can create tables. If the role is read-only, it skips that step and falls back
to the schema compatibility check.

The first push or serve after upgrading to this schema also fills two derived
tables before serving: `usage_messages` and `terminal_event_snapshots`. The
Activity report reads these tables instead of parsing usage JSON and scanning
tool events on every request. The fill runs once per mirror, keeps the source
tables unchanged, and resumes from the start if interrupted. Until it completes,
a read-only serve role fails the compatibility check with a message naming the
missing fill; run `agentsview replica clickhouse push` with a role that can
create tables to finish it.

If `clickhouse serve` runs with a read-only role, upgrade the machine that
pushes first, run `agentsview replica clickhouse push` once, and then upgrade
the machines that serve. This release adds the `session_plan_usage` table for
Codex Cloud plan usage and the `usage_price_models` table, which lists the
models stored usage prices cover. A read-only role cannot create them. Until a
push creates them, an upgraded `clickhouse serve` with that role fails to start
with "clickhouse mirror schema is missing session_plan_usage". A serve role that
can create tables adds the tables itself at startup, so the upgrade order does
not matter for it.

#### Required system-table permissions

Schema setup needs `SELECT ON system.projections` to inspect the prepared usage
projections. This applies to `clickhouse push` and to `clickhouse serve` when
its role can create tables. Without that grant, setup fails with
`reading prepared usage projections` and an access-denied error. An
administrator must run `GRANT SELECT ON system.projections TO <schema_user>`.
For an XML-managed user, add `<query>GRANT SELECT ON system.projections</query>`
to its `<grants>` and run `SYSTEM RELOAD USERS`. A read-only serve role does not
need this grant; run a push with the schema-capable role first.

The serve role also needs `GRANT SELECT ON system.parts`. Before rescanning, the
Activity report checks whether the mirror changed by hashing the active parts of
its source tables. With the default server setting
`select_from_system_db_requires_grant`, a role granted only
`SELECT ON <database>.*` can read `system.columns` but not `system.parts`, and
`serve` refuses to start with an error naming the missing grant. An
administrator must grant it to the serve user. For XML-managed users, add
`<query>GRANT SELECT ON system.parts</query>` to that user's `<grants>` in the
deployment configuration and run `SYSTEM RELOAD USERS`. SQL `GRANT` cannot
modify XML-managed users, and schema upgrades cannot grant a permission the
application account lacks.

`clickhouse serve` caches the Activity report of each past day a client opens on
disk, so reopening it is fast, including after a restart. Days no one opens are
never written, and a report no one has opened for 30 days is removed. The files
live under the service cache directory: `$CACHE_DIRECTORY` when the service
manager sets it, or the user cache directory under
`agentsview/clickhouse-activity-reports`. They are derived data; deleting them
only makes the next open of those days slower, and a new binary rebuilds them on
its own. If `serve` cannot create the directory, it logs a warning and keeps
reports in memory only.

Usage also caches fully prepared selections under `agentsview/clickhouse-usage`
in the same cache directory. Each selection can use up to 128 MiB, with a
256 MiB limit per mirror. The next read rebuilds a selection after a push, a
pricing change, a new binary, or deletion of the cache directory. If the cache
is unavailable, Usage reads the mirror instead.

Term-trend reads, through the `get_term_trends` MCP tool or
`GET /api/v1/trends/terms`, keep per-session counts in the same cache. The first
read builds them, and after a push it recounts only changed sessions. Hour,
model, Local timezone, and time-dependent selections still read the mirror
directly.

When `require_auth` is enabled, `serve` generates a bearer token if needed and
prints it on startup. Pass it in the `Authorization: Bearer <token>` header on
API requests.

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
| `push_batch_size`    | Sessions per push batch; default `100`                                |
| `default_clickhouse` | Named target used when more than one `[clickhouse.NAME]` block exists |

Environment overrides for the effective default target:

| Variable                         | Maps to        |
| -------------------------------- | -------------- |
| `AGENTSVIEW_CLICKHOUSE_URL`      | `url`          |
| `AGENTSVIEW_CLICKHOUSE_DATABASE` | `database`     |
| `AGENTSVIEW_CLICKHOUSE_MACHINE`  | `machine_name` |

Because `config.toml` holds credentials, restrict its permissions:

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
`AGENTSVIEW_CLICKHOUSE_MACHINE` still work in named-target mode, but they apply
only to the effective default target and do not rewrite every named
`[clickhouse.NAME]` entry.

`clickhouse serve` and `clickhouse service` always use the effective default
target. In named-target mode, set `default_clickhouse` to choose which target
those long-running commands use.

### Transport security

Non-loopback URLs must use TLS unless you set `allow_insecure = true`. For the
native protocol, add `secure=true` to the URL, typically on port 9440. For HTTP,
use an `https://` URL. `skip_verify=true` is rejected on a non-loopback host
unless `allow_insecure` is set. Loopback addresses, `127.0.0.1` and `localhost`,
can stay plaintext or skip verification.

### Connections and query threads

Each `agentsview` process opens up to 12 ClickHouse connections, and each query
uses at most 8 threads. The Analytics page sends 11 requests at once, so every
panel's query can start right away while sharing the server's cores instead of
each claiming one thread per core. `clickhouse serve` opens 4 more connections
for the session list, sidebar filters, and single-session views, so page
navigation never waits behind dashboard or Usage queries.

To adjust either limit, add a URL parameter:

```toml
[clickhouse]
url = "clickhouse://user:pass@host:9440/agentsview?secure=true&max_open_conns=16&max_threads=4"
```

- `max_open_conns` sets the main pool size. The 4 page connections stay fixed.
- `max_threads` sets the per-query thread cap. Setting `max_threads=0` restores
  the ClickHouse server default of one thread per core.

The best values depend on your server. A query thread costs more CPU when the
data directory sits on network storage, so a host with slow storage or few cores
may want fewer threads. The ClickHouse user must be allowed to change settings;
a profile with `readonly=1` rejects them.

______________________________________________________________________

## Differences from PostgreSQL

- ClickHouse has no schema name. Tables live in a database set by the `database`
  key or the DSN path.
- Vectors live in shared `Array(Float32)` tables keyed by config fingerprint
  rather than per-generation pgvector tables, and search runs an exact cosine
  scan. There is no hosted raw-sync control plane.
- `clickhouse serve` does not run PostgreSQL-style migrations beyond
  `CREATE TABLE IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`, the one-time fill of
  derived usage and terminal-event tables described earlier, and changing string
  columns with few distinct values, such as message roles, models, and agents,
  to `LowCardinality(String)`. That change keeps every value. The `ALTER`
  returns at once, and ClickHouse rewrites existing data in a background
  mutation, which takes time and needs temporary disk space on a large mirror.
  Older AgentsView versions can keep pushing to the converted mirror.
- Deletes are version-bounded `DELETE` statements that ClickHouse applies at
  merge time. Readers always open connections with `final=1`, so they see one
  row per key without writing `FINAL` in query text.
- Every mirrored table is `ReplacingMergeTree(push_version)`. The highest
  `push_version` for an ordering key wins.

______________________________________________________________________

## ClickHouse notes

The mirror is designed for MergeTree rather than copied from PostgreSQL SQL.

- **ReplacingMergeTree.** Each table is ordered by its natural key and versioned
  with `push_version`. A later push of the same key supersedes the earlier row.
- **Deletes.** A push inserts the current dependent rows, runs
  `DELETE ... WHERE session_id IN (...) AND push_version < v`, and then writes
  session rows. Until a merge runs, both versions can exist on disk, and
  `final=1` hides the stale rows from readers.
- **No `FINAL` in queries.** The connection setting covers every statement.
  Putting `FINAL` in query text is rejected by this design.
- **Lightweight deletes.** These need ClickHouse 23.3+; the integration suite
  pins `clickhouse/clickhouse-server:26.8`.
- **Data directory on NFS.** ClickHouse cannot swap tables with
  `EXCHANGE TABLES` when its data directory is on a filesystem without atomic
  rename exchange, such as NFS. The `prepare_usage` view needs that swap for
  every scheduled refresh. The schema step detects this, logs
  `this database cannot swap tables (EXCHANGE TABLES)`, drops the view, and
  creates a `prepared_usage_rebuild_lock_<n>` table. To detect it, the schema
  step tries one swap of two empty tables, and the server logs that failed swap
  as an `UNSUPPORTED_METHOD` error. It runs this check once after each server
  start and reuses the answer until the next restart. While that lock table
  exists, a push rebuilds the prepared usage table itself before returning once
  a rebuild is due, and logs why and how long it took. The next item explains
  when a rebuild is due. If a push finds another push already rebuilding, it
  skips its rebuild and logs which lock the other push holds. The running
  rebuild checks once more after publishing and rebuilds again if data that
  arrived meanwhile makes a rebuild due. A skipped or failed rebuild does not
  fail the push: usage reads stay correct but slower until a later rebuild
  succeeds, and the next push tries again. The mirror database keeps one small
  lock table between pushes.
- **Prepared usage rebuilds.** Usage reads stay exact while the prepared usage
  table is behind: they compute the rows of sessions pushed since the last
  rebuild themselves. A rebuild rewrites the whole table, so a push rebuilds it,
  or starts the `prepare_usage` view's refresh, only when the table is empty,
  prices changed, or the rows reads compute themselves reach one-fiftieth of the
  stored rows. Other pushes write nothing to the table. Where the view exists,
  it also refreshes the table once a day, at midnight in the ClickHouse server's
  time zone, for a push that ended with errors or whose refresh request failed.
  Rows of deleted sessions stay in the table until the next rebuild, and reads
  skip them.
- **Lock of a stopped push.** A lock expires an hour after its push last renewed
  it, so a push stopped mid-rebuild makes later pushes skip their rebuilds for
  up to an hour. If the skip message names a held lock and you know no push is
  running, end that wait by marking that lock released. Run this in the mirror
  database:
  `ALTER TABLE prepared_usage_rebuild_lock_<n> MODIFY COMMENT '{"status":"released"}'`.
  A lock comment is JSON whose `status` is `held`, `released`, or `retired`. If
  an edit leaves a comment that is not one of these, each push logs an error and
  skips its rebuild, and the schema step fails. The error names the lock and
  gives the statement above to repair it. Do not drop the lock table; the next
  push would reuse its number. Leave a lock whose status is `retired` alone.
- **Moving off NFS.** The first schema step after the server restarts on the new
  data directory checks again. Restarting the server is how you force a new
  check. When it finds that the database can swap tables again, it creates a new
  lock table with status `retired`, drops the older lock tables, and then
  creates the `prepare_usage` view. Do not drop the retired lock: it stops a
  push that started before the switch from overwriting the view's rows. If the
  database later loses table swapping again, the schema step marks the retired
  lock released, and pushes rebuild again.
- **Half-finished switch.** If the schema step stops between retiring the lock
  and creating the view, the next push creates the view. If the database can no
  longer swap tables by then, the push releases the lock and rebuilds instead.
- **Missing view.** If a push logs that the `prepare_usage` view is missing,
  usage reads stay correct but slower until it exists. Another client may be
  recreating it. If the message repeats, restart `agentsview`; its schema step
  recreates the view.
