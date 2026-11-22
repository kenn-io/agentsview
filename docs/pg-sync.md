---
last_edited: 2026-11-13
title: PostgreSQL Sync
description: Share sessions across machines with PostgreSQL push sync, an auto-push service, and a read-only server
---

AgentsView stores sessions locally in SQLite by default. With PostgreSQL sync,
you can push sessions from one or more machines into a shared PostgreSQL
database, keep that database current with an optional auto-push watcher or OS
service, and serve a read-only web UI from it for team dashboards or
multi-machine setups.

The PostgreSQL usage optimization baseline and its architecture boundary are
recorded in `docs/internal/pg-usage-native-optimization.md`.

`pg push` and `pg serve` install a usage change journal that records changed
sessions and advances a revision for usage, pricing, and project identity
writes. Alongside normal table access, installation requires `CREATE` on the
schema and `TRIGGER` on the watched tables. It takes a `SHARE ROW EXCLUSIVE`
lock on those tables until it commits; reads continue while writes wait.
CockroachDB skips this journal. Usage cache invalidation relies on `pg push`
updating each parent session when it changes that session's messages. If you
edit messages directly in SQL, also update the parent session's `updated_at`
to invalidate cached usage and Signals counts. For `pg serve` roles granted
access later, Activity reuse needs `SELECT` on `usage_session_changes`. Signals
count reuse needs `SELECT`, and saving counts needs `INSERT` and `UPDATE`, on
`session_frustration_counts`.

On `pg serve`, ended Activity ranges stay in memory until a push touches their
sessions or range, pricing changes, or project identity changes. Hosted
PostgreSQL and archives without the journal rebuild every time. For ranges that
include today, native PostgreSQL saves Activity inputs on a graceful close under
`$CACHE_DIRECTORY/postgres-activity`, or `agentsview/postgres-activity` in the
user cache directory. Each file and the whole cache are capped at 256 MiB. After
a restart, Activity refreshes changed sessions. Saving and reusing these inputs
requires the identity-function permissions described for the Usage cache below;
without them, Activity reads directly and can still reuse inputs in memory.

`pg serve` also keeps finished Usage results in memory, up to 64 selections and
64 MiB per read kind, until the journal revision or custom pricing changes.
Hosted PostgreSQL, archives without the journal, and filters that compare
against the current time read every time.

Native PostgreSQL keeps priced Usage rows in `<data-dir>/usage-postgres/`, so
restarts reuse prior work and reads refresh sessions and prices changed by a
push. When a read would first need to rebuild much of that cache, such as on
first use, after a price or project identity change, or after new account usage,
it returns the direct PostgreSQL result and rebuilds the cache in the
background. Journals upgraded by a current writer need `SELECT` access.
Persistent caching also requires `EXECUTE` access to PostgreSQL's
`pg_control_system()`, `pg_current_wal_lsn()`, and `pg_walfile_name(pg_lsn)`
functions. Readers without these permissions use direct reads, as do hosted and
borrowed transaction readers, CockroachDB, and unavailable journals. PostgreSQL
standbys keep complete results in memory when a current writer has installed the
journal history token; otherwise they read directly. Readers without a version 2
journal repeat direct reads until a current writer upgrades it, and readers
without the identity functions can retain direct results when the journal has a
history token. Recovery onto a different PostgreSQL timeline starts a new cache
generation. If a cache attempt fails while a request is active, the existing
direct PostgreSQL read supplies its result or error. A journal installed later
enables persistent reuse without a server restart. If the cache directory cannot
be created, the reader uses temporary storage. The cache is disposable and
contains no source sessions. Plan usage and per-session details keep their
existing read paths.

Term trends store per-session counts under `$CACHE_DIRECTORY/postgres-trends`,
or `agentsview/postgres-trends` in the user cache directory, and recount only
changed sessions after a push. Hosted and borrowed transaction readers read
directly. These files are disposable. The same directory can hold saved search
snippets. After a session is deleted, its snippets can remain until the same
search overwrites the page or the cache is trimmed.

Sync runs one way, from SQLite to PostgreSQL. Each machine pushes its own
sessions, and `pg serve` reads from the shared database. The web UI includes the
session browser, analytics dashboard, search, and, as of 0.23.0, the Usage
dashboard.

!!! note "Separate hosted raw sync"

    [Hosted Raw Sync](/docs/hosted-raw-sync/) is a separate path that keeps original
    provider artifacts in hosted custody through authenticated, resumable uploads.
    Explicitly provisioned tenants can enable `raw_derivation` to parse accepted
    sources directly into browsable PostgreSQL sessions, without a local SQLite
    archive. Embedding consumption remains future work. Ordinary `pg push` and
    read-only PostgreSQL serving keep their existing behavior; `pg push` refuses
    a schema that has been adopted for hosted raw processing.

Commit lookups return the copied links and prompts. After you upgrade, the first
push is a full push to copy existing commit links. Upgrade serving binaries
before or alongside publishers; older readers cannot see the new table.
A serving role that cannot create tables needs an upgraded push, or
`hosted-provision` for a hosted schema, before commit lookups work. Each
archive's rebuild removes only the deleted-session links that archive pushed.
Links with no recorded archive, from older pushes or permanent deletion in the
replica, stay and can reappear after an archive discards its matching copy. A
deleted session remains visible as a missing record. Project filters use the
project saved with its links in SQLite, including after a rebuild or a deletion
before the first push.

## Hosted raw processing

For uploads that the server parses itself, provision a schema and tenant with
`agentsview replica pg hosted-provision <owner-target>`, then serve with a
separate restricted runtime role. The selected PG target needs `raw_tenant`, an
explicit schema, and `raw_derivation = true`. The server also requires
authentication, a stable cursor secret, and a supported Linux/cgo sandbox.
Runtime startup checks schema protections and grants, and never runs hosted
migrations.

See [Hosted Raw Sync](/docs/hosted-raw-sync/#provision-a-hosted-instance) for
the configuration, least-privilege grants, and platform limits. For bounded
parser-version rollouts, run
`agentsview replica pg raw-reparse <runtime-target> --run-id <id> --batch-size 64`.
Exhausted jobs can resume after a new source arrives, a processing-version
selection changes, or an operator runs
`agentsview replica pg raw-reparse <runtime-target> --repair --batch-size 64`.
Repair uses this executable's processing version. You can repeat it safely until
it prints `Rescheduled 0 failed jobs`; it leaves healthy jobs, in-progress jobs,
and heads at other versions unchanged. Use `--repair` separately from
`--run-id`.

To stop the worker, set `raw_derivation = false` and keep `raw_tenant`,
authentication, and the cursor secret. That preserves hosted public reads and
raw custody, and does not restore `pg push` access to the owned schema.

## Quick start

### 1. Configure PostgreSQL

Add a `[pg]` section to `~/.agentsview/config.toml`:

```toml
local_machine_name = "Laptop"

[pg]
url = "postgres://user:pass@host:5432/dbname?sslmode=require"
```

Sessions keep their source installation ID. `local_machine_name` sets the
display label after a daemon restart and the next push. The optional
`[pg].machine_name` defaults to the installation ID and only supplies a key for
legacy `local` rows; it does not rename recorded session keys.

To configure multiple PostgreSQL destinations, use named `[pg.NAME]` blocks and
`default_pg` instead of the legacy single `[pg]` block. Target names are
normalized case-insensitively. You cannot use `all`, `local`, or any of the
legacy `[pg]` field names (`url`, `schema`, `machine_name`, `allow_insecure`,
`projects`, `exclude_projects`, `push_vectors`, `raw_tenant`, `raw_derivation`,
`raw_poll_seconds`, `raw_attempt_seconds`, and `raw_max_attempts`) as a
`[pg.NAME]` target name.

### 2. Push sessions

```bash
agentsview replica pg push
```

This one-shot command syncs all local sessions, messages, and tool calls to
PostgreSQL and creates the schema automatically on the first push.

After the owner upgrades the schema, repeat pushes recheck automation only for
new sessions, changed evidence, or different automation settings. On the first
push from the schema-owner connection, installing automation tracking takes an
exclusive lock on `sessions`, which blocks reads and writes until the upgrade
commits. The first tracking audit checks and stamps every session once, and
different classifier settings trigger a full recheck and restamp. Publishers
without schema-change privileges keep running a full audit until that upgrade,
and CockroachDB keeps the full audit.

Push stores each message's output tokens and billed web searches in their own
columns, so Usage reads them without parsing the message's token JSON. The first
push after upgrading adds the columns and fills them for every existing message,
and each later push also fills messages that an older binary pushed in the
meantime. Until then, Usage leaves out those messages' web search fees, and when
Claude logged one request more than once, Usage may count a copy with fewer
output tokens. A read-only `pg serve` fails its schema check until a push
upgrades the schema.

The next push using the schema-owner connection installs
`idx_sessions_data_version`, `idx_sessions_parentless_workers`, and
`idx_sessions_source_missing` on existing archives. Each first build scans
`sessions` and blocks writes until installation commits. Restricted publishers
keep pushing before owner installation; their version and repair checks use the
existing queries until the indexes are available.

To keep PostgreSQL current automatically, run the foreground watcher:

```bash
agentsview replica pg push --watch
```

Or install it as a per-user background service on macOS or Linux:

```bash
agentsview replica pg service install
```

### 3. Serve the dashboard

```bash
agentsview replica pg serve
```

This opens the read-only web UI at `http://127.0.0.1:8080`, backed entirely by
PostgreSQL. Source sessions stay in PostgreSQL, and Usage keeps a disposable
local cache. The server does no file watching, and its session UI and APIs do
not accept session uploads. A writable deployment can also expose the separate
[hosted raw-sync control plane](/docs/hosted-raw-sync/#http-control-plane).

______________________________________________________________________

## Commands

<a id="agentsview-pg-push"></a>

### `agentsview replica pg push`

Sync sessions from the local SQLite database to PostgreSQL.

```bash
agentsview replica pg push [target] [flags]
```

| Flag                 | Default | Description                                                                                                                    |
| -------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `--all`              | `false` | Push every configured PG target sequentially                                                                                   |
| `--full`             | `false` | Force full local resync and re-push, bypassing change detection                                                                |
| `--no-vectors`       | `false` | Skip the semantic-search vector phase for this run                                                                             |
| `--embed`            | `false` | Push sessions, then build and push vectors; watch builds after syncs in the background and pushes vectors on the next interval |
| `--projects`         |         | Comma-separated projects to push (inclusive)                                                                                   |
| `--exclude-projects` |         | Comma-separated projects to exclude                                                                                            |
| `--all-projects`     | `false` | Ignore configured project filters for this run                                                                                 |
| `--watch`            | `false` | Run continuously, pushing on change plus a periodic floor                                                                      |
| `--no-start-daemon`  | `false` | With `--watch`, require a writable daemon; exit if none is running; incompatible with `--embed`                                |
| `--debounce`         | `30s`   | Coalesce window after a filesystem change before pushing (`--watch` only)                                                      |
| `--interval`         | `15m`   | Periodic floor push interval (`--watch` only)                                                                                  |

Without `--watch`, push runs on demand whenever you invoke it. With `--watch`,
the command stays in the foreground and keeps pushing until you stop it.

If you omit the target name, `pg push` uses the effective default target. Pass
one named target explicitly to push only to that destination, or pass `--all` to
push to every configured target in sequence. `--all --watch` is rejected.

**What happens on push:**

1. Runs a local sync to pick up any new or modified session files.
1. Compares local sessions against the PostgreSQL watermark to find what changed
   since the last push.
1. Upserts sessions, messages, tool calls, and (as of 0.30.0)
   [curation metadata](#curation-metadata) in batches of 50.
1. Advances the watermark timestamp on success.

Incremental pushes use a two-layer fingerprint to skip unchanged sessions.
First, session metadata and local dependency fingerprints identify candidates.
Then, for candidates that already have messages in PostgreSQL, PostgreSQL
computes SHA-256 hashes of message text, dialogue text, thinking text, tool
inputs, results, file paths, and tool-result-event content. Only large text
values are replaced by hashes; IDs, timestamps, token JSON, and other message,
tool, and event metadata still cross the connection. Same-length text changes
still trigger publication. A changed session is rewritten from its first changed
message onward, leaving earlier messages, their tool calls and results, and
their pins in place. Usage events are replaced whole when they change, and the
pushed-message count includes only the rewritten messages. Rows written before
control-character cleanup may be rewritten once, and unchanged text compares
normally after that. To bypass both layers and re-push everything, for example
after a schema reset or when message content was rewritten in place, pass
`--full`.

A push can also become full on its own, for example when an upgrade adds a
backfill, after an archive rebuild, or when PostgreSQL lost this archive's push
marker. Such a push still checks and updates every session, but it compares each
transcript with PostgreSQL and rewrites only changed messages. Only `--full`, a
push resuming an interrupted `--full` push, or a push that rebuilt the local
archive itself rewrites every transcript.

Every push sends the most recently active sessions first, so recent activity
reaches PostgreSQL before older history. If a full push stops partway, for
example because the process restarted, the next push resumes it and skips
sessions it already pushed unless PostgreSQL lost them. The summary reports
`resuming an unfinished full push` and counts the skipped sessions. Progress is
saved at most every 15 seconds, so a restart repeats at most that much work.
The push starts over instead if you pass `--full`, or if the target, the local
archive, or the data format changed since it began. When `--full` discards an
unfinished full push, the summary says `discarded an unfinished full push`. A
full push that reaches the end with session errors resumes only while something
else still requires a full push; otherwise the next push is incremental and
retries the failed sessions.

After an archive rebuild, the next push sends all sessions again. Once every
session publishes successfully, a full push also removes PostgreSQL sessions
that this archive owns but no longer contains. Project filters limit this
cleanup to the selected projects, and sessions owned by another archive, or
whose owner is unknown, stay untouched. An interrupted push resumes before
cleanup. Codex thread trash state remains available for restoring or permanently
deleting its pages.

If any sessions fail to push, AgentsView does not advance the watermark so the
next run retries them. The exit code is 1 when any errors occur, and 0
otherwise.

When a daemon handles an incremental push against a current archive, an
incomplete local ingestion pass still lets committed sessions copy over. The
daemon logs the incomplete ingestion pass, and the command returns the mirror
push result, including any row errors or deferred vectors. Local sync retries
failed sources independently; a completed mirror push does not trigger an
immediate mirror retry only because ingestion was incomplete. A failed full
resync still blocks the push. Watcher batches keep their acknowledgement rules,
and the startup and periodic unscoped pushes let healthy archived sessions catch
up.

#### Automatic Push Watcher

Start the watcher in the foreground with `agentsview replica pg push --watch`:

```bash
agentsview replica pg push --watch
agentsview replica pg push --watch --debounce 1m
agentsview replica pg push --watch --interval 5m
```

The watcher runs one initial local sync and PostgreSQL push, then pushes again
after session-directory changes settle for the debounce window. Ordinary changes
carry a bounded set of changed paths to the daemon, so editing one session does
not rediscover the full local archive. Watcher overflow, lost events, and
ambiguous renames promote to authoritative reconciliation of the currently
available provider roots. The interval sets a floor: even if filesystem events
are missed or a root cannot be watched because of OS watch limits, the next
interval push catches up.

Operational details:

- Only one watcher can run per AgentsView data directory; a runtime lock
  prevents competing pushes from racing watermarks.
- PostgreSQL connections open lazily and reset after errors, so a transiently
  unavailable database is retried on the next trigger instead of crashing the
  watcher.
- On shutdown (`Ctrl+C`, `SIGTERM`), the watcher attempts one bounded final
  flush.
- Logs go to `pg-watch.log` under the AgentsView data directory.
- The watcher uses the selected PostgreSQL target, or the `default_pg` target
  when you omit a name, along with the same machine name, project filters,
  classifier settings, and `result_content_blocked_categories` behavior as
  one-shot `pg push`.
- The watcher publishes an identity-bound local lifecycle record. Native memory
  hooks can wake that exact owner with
  `agentsview memory session-start --mode hosted-contributor`; the wake enters
  the normal debounce loop and does not bypass its credentials or scheduling.
  Restart a watcher created by an older binary once before using this hook.
  Lifecycle wake delivery is currently available on macOS and Linux; Windows
  watchers continue to use their normal filesystem-event and interval cadence.

#### Project Filtering

By default, `pg push` syncs all projects. Use project filters to push a subset:

```bash
# Push only these projects
agentsview replica pg push --projects alpha,beta

# Push everything except this project
agentsview replica pg push --exclude-projects scratch

# Ignore config-file filters for this run
agentsview replica pg push --all-projects
```

`--projects` and `--exclude-projects` are mutually exclusive, and
`--all-projects` cannot be combined with either.

You can also set project filters in `config.toml` so you do not have to pass
them on every run:

```toml
[pg]
url = "postgres://..."
projects = ["alpha", "beta"]
# or: exclude_projects = ["scratch"]
```

CLI flags override config values. To list available project names, run
[`agentsview project list`](/docs/commands/#agentsview-projects).

Filtered pushes keep their own local push watermark for each target and filter
set. For example, repeated `agentsview replica pg push --projects alpha,beta`
runs use a different watermark from unfiltered pushes and from
`agentsview replica pg push --projects gamma`. This keeps allow-list pushes
incremental without advancing the unfiltered/global cursor.

After upgrading from an older version, the first filtered push for a given
project set may still scan the matching local sessions once to seed that scoped
watermark. Later pushes with the same filter set use the scoped watermark.

#### Curation Metadata

As of 0.30.0, `pg push` also syncs two pieces of per-user curation state
alongside session content:

| PostgreSQL table   | What it holds                                                                                     |
| ------------------ | ------------------------------------------------------------------------------------------------- |
| `starred_sessions` | One row per starred session: `session_id`, `created_at`                                           |
| `pinned_messages`  | One row per pin: `id`, `session_id`, `message_id`, `ordinal`, `source_uuid`, `note`, `created_at` |

Stars are keyed by session ID and overwrite cleanly across machines. Pins are
reconciled by `source_uuid`, a stable identifier derived from the underlying
message, so a pin survives a re-parse that shifts message ordinals. Without
this, pins would silently drift to the wrong message after any session resync.

A pin or note you make in a PostgreSQL-served UI lives only in PostgreSQL. After
a Claude.ai zip replacement or Sync branch switch, a push preserves it when the
message's role, text, and occurrence rank still match. If the message cannot be
matched, the push drops the pin and note. SQLite's Trash copy does not hold
them.

The same `pg push` run populates curation tables; no separate command or flag is
required. [`agentsview secrets`](/docs/commands/#agentsview-secrets) findings
also push through this codepath with the same parity guarantees as session
content (`secret_findings` table, per-session `secret_leak_count`, and the
[`has-secret`](/docs/session-api/#agentsview-session-list) list filter).

#### Vector push

When `[vector]` is enabled locally, `pg push` runs a vector phase after the
session and message phases. It copies the machine's active embedding generation
from `vectors.db` into pgvector so a shared PostgreSQL deployment can answer
`--semantic` and `--hybrid` queries, re-sending only changed sessions. To skip
the phase for one run, pass `--no-vectors`. To disable it persistently, set
`push_vectors = false` under `[pg]`. Databases without pgvector, for example
CockroachDB, skip the phase and keep syncing session content. See
[semantic search: PostgreSQL](/docs/semantic-search/#postgresql) for the full
push, serve, and maintenance workflow.

Use `pg vectors list` and `pg vectors drop <id>` to inspect and remove pushed
generations.

#### Build and push embeddings in a container

`pg push --embed` (and `pg push --watch --embed`) syncs the local archive,
pushes sessions, and builds pending embeddings in one process. A container
running it needs no shell wrapper:

- With `[vector]` in its `config.toml`, it builds with that recipe.
- Without `[vector]`, it adopts the recipe a configured workstation published to
  PostgreSQL. Set `AGENTSVIEW_EMBEDDINGS_ENDPOINT` and, if the endpoint needs a
  key, either `AGENTSVIEW_EMBEDDINGS_API_KEY` or
  `AGENTSVIEW_EMBEDDINGS_API_KEY_FILE` (never both) under
  [`AGENTSVIEW_MODE`](/docs/configuration/#container-deployment). Adoption needs
  exactly one published recipe; see
  [semantic search: PostgreSQL](/docs/semantic-search/#postgresql). To change
  the number of inputs per request from the default `32`, set
  `AGENTSVIEW_EMBEDDINGS_BATCH_SIZE` to a positive integer.

AgentsView resolves the recipe on the first cycle that succeeds and keeps it
until the process restarts. `--embed` never starts a daemon and refuses to run
while a daemon owns the archive; run `agentsview daemon stop` first to build
here. Watch mode builds in the background after each sync that commits changes
with a 30-second debounce, and honors `[vector.embed] backstop_interval` to
reconcile older imports. With `[vector.embed] recall = true`, it also keeps the
Recall embeddings current. Built vectors reach PostgreSQL on the next interval
push. Until the recipe's own generation is built, the watcher pushes no vectors,
even if an older generation is still active locally. While another process holds
`vectors.write.lock`, such as a direct `agentsview index embeddings build`, the
watcher keeps pushing sessions, logs that embeddings have not started, and
starts them on the first push after the lock frees.

A one-shot `pg push --embed` checks for a published recipe before it syncs, so a
missing or ambiguous recipe fails before any session work. It then pushes
sessions, builds embeddings with progress output, and pushes vectors in the same
run. It builds only after PostgreSQL confirms it can store vectors and register
the recipe's generation, so a server without pgvector, or a role that cannot
create the vector tables or register a generation, skips the build.
`--full --embed` repairs vector rows too. A build error returns after the
sessions land, with their counts shown.

A watcher started before the first recipe is published adopts it on a later
push, checking PostgreSQL at most once per `--interval`. If `pg serve` starts
before a workstation pushes its first generation, restart `pg serve` after that
push to enable semantic search. After you change the recipe, stop the hub's
`pg push --watch --embed`, run `agentsview replica pg vectors drop <old>`, then
start the watcher and restart `pg serve` to adopt the new recipe. A running
watcher keeps pushing the generation it adopted and recreates the dropped row.
You cannot combine `--embed` with `--no-vectors`, `--all`,
`push_vectors = false`, or a usage-only archive.

To keep the database password out of the URL, mount a passfile and set
`PGPASSFILE`; see
[container deployment](/docs/configuration/#container-deployment).

<a id="agentsview-pg-status"></a>

### `agentsview replica pg status`

Show the current sync state.

```bash
agentsview replica pg status [target] [flags]
agentsview replica pg status --all
agentsview replica pg status --projects alpha,beta
```

If you omit the target name, `pg status` uses the effective default target. Pass
one named target explicitly to inspect that destination, or pass `--all` to
print every configured target in sequence.

To inspect a matching filtered or unfiltered watermark, pass the same
`--projects`, `--exclude-projects`, or `--all-projects` filter flags you use
with `pg push`.

If you use a `pg push --watch` watcher on this machine, status also shows its
last success and any failure streak, and `--max-stale` alerts on a stalled
watcher. Both work as described for
[`clickhouse status`](/docs/clickhouse-sync/#agentsview-clickhouse-status); the
record is `pg-watch-health.json`.

Output:

```
Machine:     my-laptop
Last push:   2026-03-24T10:30:00Z
PG sessions: 1842
PG messages: 47291
```

| Field       | Description                                                |
| ----------- | ---------------------------------------------------------- |
| Machine     | Configured machine key or installation ID        |
| Last push   | Timestamp of last successful push ("never" if no push yet) |
| PG sessions | Total session count in PostgreSQL (all machines)           |
| PG messages | Total message count in PostgreSQL (all machines)           |

<a id="agentsview-pg-service"></a>

### `agentsview replica pg service`

Install and manage the [`pg push --watch`](#automatic-push-watcher) auto-push
daemon as a per-user OS service.

```bash
agentsview replica pg service install
agentsview replica pg service status
agentsview replica pg service logs -f
agentsview replica pg service stop
agentsview replica pg service start
agentsview replica pg service uninstall
```

Supported service managers:

| Platform | Manager               |
| -------- | --------------------- |
| macOS    | launchd LaunchAgent   |
| Linux    | `systemd --user` unit |

The generated unit runs `agentsview replica pg push --watch`. It pins
`AGENTSVIEW_DATA_DIR` to the data directory used at install time and writes logs
to `~/.agentsview/pg-watch.log` unless you changed the data directory.

If you run your own collector service, install with
`agentsview replica pg service install --no-start-daemon`. The push service
exits if no writable daemon is running, and the service manager restarts it
until the collector answers. It retries pushes while the collector restarts,
without starting a daemon or opening the archive directly. Reinstalling restarts
the service with the new flag. The default installation still starts a missing
daemon for hosts that run only the push service.

`install` requires a literal PostgreSQL URL in the effective default target of
`~/.agentsview/config.toml`, either the legacy `[pg].url` or the target selected
by `default_pg` from named `[pg.NAME]` blocks. It intentionally rejects
`AGENTSVIEW_PG_URL` and environment-expanded URLs such as `${PG_PASSWORD}`
because background services do not inherit your interactive shell environment.
Other session-directory environment variables are not copied into the unit
either; put persistent settings in `config.toml` before installing.

On headless Linux machines, `systemd --user` services stop at logout and do not
start at boot unless user lingering is enabled. If lingering is disabled,
`install` prints the `loginctl enable-linger` command with your actual username
filled in, for example `loginctl enable-linger alice`, and offers to run it.

<a id="agentsview-pg-serve"></a>

### `agentsview replica pg serve`

Start the PostgreSQL web UI. Legacy mode reads pushed sessions; an explicitly
provisioned hosted target can also derive sessions from accepted raw captures.

```bash
agentsview replica pg serve [flags]
```

| Flag                | Default     | Description                                         |
| ------------------- | ----------- | --------------------------------------------------- |
| `--host`            | `127.0.0.1` | Bind address                                        |
| `--port`            | `8080`      | Explicit nonzero port must be free; `0` selects any |
| `--base-path`       |             | URL prefix for reverse-proxy subpath                |
| `--public-url`      |             | Browser URL, also added to trusted origins                  |
| `--public-origin`   |             | Trusted browser origin (repeatable/comma-separated) |
| `--public-port`     | URL port or `8443`      | Managed Caddy HTTP/HTTPS listener and URL port                     |
| `--proxy`           |             | Managed proxy mode (`caddy`)                        |
| `--caddy-bin`       | `caddy`     | Caddy binary path                                   |
| `--proxy-bind-host` | `127.0.0.1`   | Caddy bind address                                  |
| `--tls-cert`        |             | TLS certificate path                                |
| `--tls-key`         |             | TLS key path                                        |
| `--allowed-subnet`  |             | CIDR allowlist (repeatable/comma-separated)         |

The session HTTP API is read-only. Local sync and file watching run only when
`--push-watch` is enabled. Sessions from all machines appear in one unified
view, and the same deployment also serves the analytics dashboard and the Usage
page from PostgreSQL-backed queries.

```bash
AGENTSVIEW_DATA_DIR=/data agentsview replica pg serve --push-watch
# Add --embed to build and publish embeddings.
```

`--push-watch` ingests mounted roots and publishes to the default PG target in
the same process. The data directory is the persistent archive; stop any other
writer using it before starting.

`--embed`, `--debounce` (default `30s`), and `--interval` (default `15m`)
require `--push-watch`. It rejects `--no-sync` and hosted
`raw_tenant`/`raw_derivation` configurations. The first published vector
generation enables semantic search without a restart when vector search is
configured. Shutdown waits for ingestion and embedding work, including a final
push limited to 30 seconds.

When the PostgreSQL role can write the raw-sync tables and ingest-job sequence,
`pg serve` also registers the
[hosted raw-sync control plane](/docs/hosted-raw-sync/#http-control-plane).
Those routes can write raw custody metadata and local raw-sync storage, and they
do not make the session APIs writable. A PostgreSQL role without the required
raw-sync write privileges omits the runtime routes and logs the missing
requirements. Existing least-privilege raw-sync roles now also need `SELECT` and
`UPDATE` on `raw_ingest_jobs` so manifest commits can retire obsolete parse
jobs:

```sql
GRANT SELECT, UPDATE ON agentsview.raw_ingest_jobs TO raw_sync_runtime;
```

Run this as the schema owner, substitute your schema and runtime role, and
restart `pg serve`. Keep the existing `INSERT` and ingest-job sequence `USAGE`
grants. This enables legacy raw-custody HTTP routes; for server-owned parsing,
use the explicit hosted setup above.

In legacy mode, `pg serve` applies any pending schema migrations to PostgreSQL
automatically, creating new tables and indexes added in newer AgentsView
versions so you do not need to run `pg push` before starting the server after an
upgrade. If the PostgreSQL role is read-only, the server skips the migration and
falls back to the schema compatibility check.

When you enable `require_auth`, AgentsView generates a bearer token if needed
and saves it in `config.toml` as `auth_token`; startup only reports that auth is
enabled. Pass the token in the `Authorization: Bearer <token>` header on API
requests. The SSE watch endpoint also accepts `?token=<token>` for native
`EventSource` clients; see [SSE Endpoints](/docs/remote-access/#sse-endpoints)
for when to use it.

The raw-sync machine routes do not accept this shared bearer token. They use
their own device credentials and operation-scoped tokens as described in the
[hosted raw-sync security boundary](/docs/hosted-raw-sync/#security-and-deployment-boundary).

For LAN access, combine `require_auth = true` with a non-loopback bind such as
`agentsview replica pg serve --host 0.0.0.0`, or keep the backend on loopback
and expose it through a proxy.

`pg serve` does **not** expose the global live-refresh event stream used by
normal `agentsview serve`, because there is no local sync engine attached to the
server. The session browser, analytics, and usage views still work normally;
they are not auto-refreshed by the global SSE path.

As of 0.33.0, the web UI distinguishes a **degraded backend** (the server
responds with 5xx errors because PostgreSQL is temporarily unavailable) from an
unreachable one. Instead of forcing a page reload that could loop while the
database is down, the app stays interactive with its current data, shows a
compact warning in the status bar, and clears it once a real data read succeeds.
Click the status bar to retry immediately. True network failures still use the
reload-based recovery path.

CockroachDB also works as the shared database. Version 0.33.0 reworked the
analytics and usage queries to perform well on CockroachDB (filter pushdown
into the source tables, SQL-side tool-call aggregation, and batched pricing
writes) without changing any report semantics on PostgreSQL or SQLite.

In a multi-tenant deployment behind PostgreSQL row-level security, slow
analytics or usage panels usually point to the policy shape rather than to
AgentsView itself. Because the read-side queries are already session-id-shaped,
expose child rows through a set-based predicate such as
`session_id IN (SELECT ...)`, not a per-row `SECURITY DEFINER` call that runs
once per scanned row. If shared-dataset aggregates still need more time, raise
the API deadline with `--write-timeout`; see
[Remote Access](/docs/remote-access/#slow-aggregates-behind-a-proxy) for the
flag, its search exceptions, and the larger troubleshooting path.

When a reverse proxy reaches the backend by an internal hostname or service
name, keep `--public-url` on the browser origin and add the proxy's upstream
origin with `--public-origin`. The forwarded-access contract and troubleshooting
details live in
[Remote Access](/docs/remote-access/#public-url-and-trusted-origins).

!!! warning

    Query-parameter tokens can leak through server logs, browser history, and
    Referer headers. Prefer the `Authorization` header for all non-SSE requests, and
    use TLS (with managed Caddy or an external reverse proxy) to protect tokens in
    transit.

For managed Caddy mode, keep the backend `--host` on loopback and use
`--proxy-bind-host` and `--public-port` to expose the public listener. The
`pg serve` and normal `serve` modes keep separate managed-Caddy state, so both
can coexist on one host.

**Examples:**

!!! warning "Enable auth before exposing `pg serve`"

    Only the loopback example below is safe without auth. Every other example binds
    off `127.0.0.1` or fronts a public proxy, so set `require_auth = true` in
    `~/.agentsview/config.toml` before starting the server. The same bearer-token
    mechanism described in [Remote Access](/docs/remote-access/#authentication)
    applies.

```bash
# Local development — loopback, no auth required
agentsview replica pg serve

# Team viewer with managed Caddy and TLS
# Requires require_auth = true in config.toml
agentsview replica pg serve \
  --proxy caddy \
  --public-url https://viewer.example.com \
  --public-port 8443 \
  --tls-cert /path/to/cert.pem \
  --tls-key /path/to/key.pem

# Remote access on a trusted private network (no TLS)
# Requires require_auth = true; only use behind a VPN or on a
# private LAN because tokens cross the wire in cleartext.
agentsview replica pg serve --host 0.0.0.0 --port 8080

# External reverse proxy under a subpath; the browser origin
# and the proxy's upstream origin are different.
agentsview replica pg serve \
  --host 0.0.0.0 \
  --base-path /agentsview \
  --public-url https://console.example.com \
  --public-origin http://agentsview:8080
```

______________________________________________________________________

## Machine labels

Each session keeps its source installation ID. For readability, the web UI shows
display labels. To change a machine's label, set `local_machine_name` in the
source's `config.toml`, restart the daemon, and push again. Use the multi-host
filter in the sidebar to show sessions from specific machines.

The [installation upgrade](/docs/configuration/#upgrading-historical-machine-keys)
moves historical local sessions to their installation ID in SQLite. The next
incremental PostgreSQL push republishes those sessions and their metadata, and
also copies adopted hostname aliases so existing machine filters and URLs keep
working. If two installations publish the same old hostname alias, the latest
push determines its filter target. To select machines unambiguously in a shared
mirror, use installation IDs. Equal display labels do not merge machines.

![Machine labels on session items](/docs/assets/generated/screenshots/machine-labels.png)

______________________________________________________________________

## Configuration

Single-target PostgreSQL settings can live in the legacy `[pg]` section of
`~/.agentsview/config.toml`:

```toml
[pg]
url = "postgres://user:pass@host:5432/dbname?sslmode=require"
schema = "agentsview"
allow_insecure = false
```

| Field              | Default      | Description                                                            |
| ------------------ | ------------ | ---------------------------------------------------------------------- |
| `url`              | (required)   | PostgreSQL connection string                                           |
| `machine_name`     | Installation ID | Explicit machine key for legacy local-sentinel rows; new sessions retain their recorded installation ID |
| `schema`           | `agentsview` | PostgreSQL schema name                                                 |
| `allow_insecure`   | `false`      | Allow non-TLS connections to non-loopback hosts                        |
| `projects`         |              | Array of project names to include in push                              |
| `exclude_projects` |              | Array of project names to exclude from push                            |

To manage more than one PostgreSQL destination, define named `[pg.NAME]` blocks
and select the effective default target with `default_pg`:

```toml
default_pg = "work"

[pg.work]
url = "postgres://user:pass@work-db:5432/agentsview?sslmode=require"

[pg.archive]
url = "postgres://user:pass@archive-db:5432/agentsview?sslmode=require"
exclude_projects = ["scratch"]
```

`agentsview replica pg push` and `agentsview replica pg status` use the
effective default target when you omit a target name, accept one target name
explicitly, and also support `--all` for sequential multi-target runs.
`agentsview replica pg push --watch` follows the effective default target unless
you pass one named target explicitly. `agentsview replica pg serve` and
`agentsview replica pg service` stay on the effective default target in this
release, and `--all --watch` is rejected.

!!! warning

    The `url` field is required for all `pg` commands. If it contains credentials,
    make sure `config.toml` has restricted file permissions (`0600`).

The connection string supports standard PostgreSQL parameters. Use
`sslmode=require` or `sslmode=verify-full` for remote databases, and use
`sslmode=disable` only for trusted local connections.

AgentsView expands environment variables in the URL using `${VAR}` syntax:

```toml
[pg]
url = "postgres://${PG_USER}:${PG_PASSWORD}@host:5432/dbname?sslmode=require"
```

### Environment variables

You can also configure PostgreSQL settings with environment variables. In legacy
single-target mode they override the `[pg]` values, and in named-target mode
they apply only to the effective default target:

| Variable                       | Description                                                                                                   |
| ------------------------------ | ------------------------------------------------------------------------------------------------------------- |
| `AGENTSVIEW_PG_URL`            | PostgreSQL connection URL                                                                                     |
| `AGENTSVIEW_PG_MACHINE`        | Machine name for push sync                                                                                    |
| `AGENTSVIEW_PG_SCHEMA`         | Schema name (default `agentsview`)                                                                            |
| `AGENTSVIEW_PG_ALLOW_INSECURE` | Sets `allow_insecure`; read only under [`AGENTSVIEW_MODE`](/docs/configuration/#container-deployment)        |

______________________________________________________________________

## Multi-Machine Workflow

A typical team setup follows these steps:

1. **Each developer** configures `[pg]` in their local `config.toml`. The
   installation ID identifies their sessions, and `local_machine_name` sets the
   display label.
1. **Each developer** installs `agentsview replica pg service` or runs
   `agentsview replica pg push --watch` to sync their sessions.
1. **One server** runs `agentsview replica pg serve` pointed at the shared
   PostgreSQL database.
1. **The team** opens the shared dashboard to browse everyone's sessions and
   filter by machine if needed.

```bash
# Developer A's machine
agentsview replica pg service install

# Team server behind an external reverse proxy
agentsview replica pg serve \
  --host 0.0.0.0 \
  --base-path /agentsview \
  --public-url https://console.example.com \
  --public-origin http://agentsview:8080
```

______________________________________________________________________

## Limitations

- **One-way sync.** Sessions flow from SQLite to PostgreSQL only. Changes in
  PostgreSQL do not propagate back to local machines.
- **Permanent deletes not propagated.** Sessions you remove with
  `agentsview session prune` are not deleted from PostgreSQL because the local
  rows no longer exist at push time. Use a direct SQL DELETE to clean up
  PostgreSQL if needed. Soft-deleted sessions (trash) sync normally.
- **Schema compatibility.** Legacy `pg serve` applies pending schema migrations
  automatically on startup. If the PostgreSQL role lacks DDL permissions, run
  `agentsview replica pg push` from a machine with write access to update the
  schema.
- **Trigram index bloat on pre-0.33.0 schemas.** The content search index was
  created with GIN `fastupdate` on, which let a pending-insert list grow
  unbounded under continuous ingest. Version 0.33.0 creates the index with
  `fastupdate = off` and alters existing indexes automatically, stopping further
  growth, though space already consumed is reclaimed only by a one-time
  `REINDEX INDEX idx_messages_content_trgm;`.
