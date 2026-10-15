# ClickHouse push and serve

SQLite is the archive. `agentsview clickhouse push` copies sessions from that
archive into ClickHouse. `agentsview clickhouse serve` runs the HTTP API and web
UI by querying ClickHouse. The dashboard does not write ClickHouse: rename,
trash, insights, stars, and pins stay on SQLite.

```text
agent files -> SQLite archive -> clickhouse push -> ClickHouse
                                      ^
clickhouse serve <- HTTP API / UI ----+
```

Operator steps, config keys, and commands live in
[ClickHouse Sync](../clickhouse-sync.md). Agent rules live in
[storage](../agents/storage.md#clickhouse-mirror).

## Problem

PostgreSQL already had configure, push, watch or OS service, then serve. DuckDB
is a local file you rebuild and swap, not a remote database. ClickHouse needed
the PostgreSQL operator path with SQL that MergeTree can run.

## Behavior

Configure `[clickhouse]` or `[clickhouse.NAME]`, push once or with `--watch` /
`clickhouse service`, then `clickhouse serve`. Several machines may push into
one database; each archive keeps its own cursor in the mirror's `sync_metadata`.

Serve implements `db.Store` reads: session list and detail, messages, search,
analytics, usage, activity, recent edits, project inventory, identity, stars,
and pins. Writes on that store return `db.ErrReadOnly`. Insights reads are
empty. `HasFTS()` is true (ILIKE). `HasSemantic()` is true once
`clickhouse serve` finds a pushed embedding generation matching the local
`[vector.embeddings]` fingerprint (see the vector decision below).

## Boundaries

- SQLite is never deleted, dropped, truncated, or recreated for a ClickHouse
  schema or data-version change.
- ClickHouse is not the system of record. A destroyed mirror is rebuilt by
  pushing again.
- Hosted raw sync stays off this path.
- Cluster / `ON CLUSTER` DDL is out of scope. ClickHouse Cloud `SharedMergeTree`
  conversion needs no change here.
- Session IDs that appear in two archives are last-writer-wins. There is no
  PostgreSQL-style owner marker.

## Decisions

**Write model from DuckDB, operator model from PostgreSQL.** Whole-session
replace gated by a fingerprint on the session row, cursor in the mirror. Not
DuckDB's rebuild-and-swap file, and not PostgreSQL's local watermark plus remote
reset blob. A missing row reads back as an empty fingerprint, so a reset mirror
repairs on the next push.

**Driver `github.com/ClickHouse/clickhouse-go/v2` through `database/sql`.**
Stores stay in the same `Query`/`Scan` shape as DuckDB and PostgreSQL. Every
connection sets `final=1` so `ReplacingMergeTree` collapses duplicates without
`FINAL` in SQL.

**Vectors are pushed and searched, keyed by fingerprint and archive.**
`clickhouse push` runs the vector phase after the session phase when the local
archive has an active generation and `push_vectors` is not false. A generation
is keyed by its config fingerprint (ClickHouse has no serial ids), all
generations share one `vector_chunks` table with `Array(Float32)` embeddings,
and per-session state is keyed by source archive so several machines push into
one mirror without owner markers: a session's vectors are written only while its
session row from this archive is resident, and evicted when that row goes.
Search is an exact `cosineDistance` scan ordered nearest first; the query shape
matches what the experimental `vector_similarity` index accelerates, which would
need per-dimension tables and a raised `max_limit_for_vector_search_queries` if
scale ever demands it.

The vector phase writes changed sessions in batches of up to 500 sessions or
about 32 MiB of document text and embeddings, whichever fills first. Each batch
inserts its documents and chunks, deletes its sessions' older chunks and
documents, then inserts their `vector_push_state` rows, all under one version.
The statement count grows with the number of batches, not sessions. A crash
before the state rows land leaves every session in the batch stale, so the next
push repeats them.

**Tables are `ReplacingMergeTree(push_version)`.** `push_version` is the push
start time in Unix nanoseconds. Same-key rows from a retry collapse on merge and
on read. Timestamps are `DateTime64(6, 'UTC')`. `sessions.last_message_at` is
computed at push time so date filters do not need a correlated `MAX`.

**Push order is the consistency mechanism.** ClickHouse has no multi-statement
transactions. Per batch of changed sessions (100 by default, set by the target's
`push_batch_size`), with `v` this push's version:

1. Insert dependent rows (messages, tool calls, result events, usage, findings,
   pins).
1. `DELETE ... WHERE session_id IN (...) AND push_version < v` per dependent
   table and for `usage_messages`. Its materialized view never sees deletes,
   so a shorter republished session would otherwise leave obsolete usage rows
   in the mirror.
1. Insert session rows with fingerprint and `source_archive_id`.

A crash before step 3 leaves the fingerprint stale, so the next push re-selects
the session. A crash between 1 and 2 leaves extra old rows that step 2 removes
on retry.

**Full pushes resume.** A full push writes a run record under
`agentsview_full_push_run:<archive>` in `sync_metadata` before its first batch
and clears it with the cursor. The record holds the run's starting push version,
the archive database ID, the scope, and the schema and data versions. A later
push that finds the record with matching values resumes the run. It treats a
session as done when this archive's mirror session row has the local fingerprint
and a push version at or after the run's. The vector phase skips a session whose
`vector_push_state` row has a matching hash and a version at or after the run's.
Any mismatch starts a new full push, and so does `--full`. A run that ends with
session errors stays recorded with an `errored` flag. When the record matches,
the next push resumes it only if another trigger already forces a full push, and
clears it otherwise. A record that does not match keeps the next push full: a
filtered run deletes mirror sessions outside its scope, and only a full push
writes back sessions older than the stored cursor. A resumed run checks vectors
for every session even when the caller scopes vectors to changed sessions,
because the earlier process may have stopped before its vector phase. A resumed
run skips the whole-mirror usage repricing that the run's first process already
did. Pushes send sessions newest activity first (`ended_at`, then `started_at`,
then `created_at`). A Codex revert page goes right after its thread's session,
which mirrors read to decide whether the page is trashed. PostgreSQL pushes and
DuckDB rebuilds use the same order.

**Shared filter builder, ClickHouse dialect.** Existing SQLite, PostgreSQL, and
DuckDB SQL stay byte-identical. ClickHouse uses `UNION ALL` in recursive CTEs,
`IN (SELECT)` instead of correlated `EXISTS`,
`parent_session_id IS NULL OR NOT IN (SELECT id FROM sessions)` for orphans
(`NULL NOT IN (...)` is unknown and would hide NULL-parent rows), `ILIKE` with
the default backslash escape and no `ESCAPE` clause (ESCAPE landed in 26.6; the
mirror was written against 25.8), and `toStartOfDay` / `toStartOfWeek` /
`toStartOfMonth` instead of `date_trunc`.

**Bootstrap through `default`.** `OpenForAdmin` pings the server `default`
database, then `CREATE DATABASE IF NOT EXISTS` the mirror name. Pinging the
DSN-path database fails when that database does not exist yet. The mirror name
is `Target.Database`, else the DSN path, else `agentsview`.

**Named targets share the PostgreSQL parser.** `[clickhouse]`,
`[clickhouse.NAME]`, `default_clickhouse`, reserved names, and env
`AGENTSVIEW_CLICKHOUSE_URL|_DATABASE|_MACHINE` on the default target only.
Non-loopback URLs need verified TLS unless `allow_insecure`.

**CLI matches `pg` minus vectors.** `push`, `status`, `serve`, `service`. When a
daemon owns the archive, push posts to `/api/v1/push/clickhouse`. The OS service
kind changes the unit label, command, and log file so it can sit beside the
PostgreSQL watcher.

## Rejected alternatives

- Pasting PostgreSQL or DuckDB SQL. Correlated `EXISTS`, `date_trunc`, and
  `UNION` without ALL fail or return wrong rows on 25.8.
- Plain `MergeTree` with delete-then-insert. A crash between the two leaves a
  session with no content and a matching fingerprint, so the next push skips
  it.
- `CollapsingMergeTree`. Needs the old rows in order to write cancel rows.
- A generic remote-mirror package. DSN, TLS, `Sync`, and SQL are not
  PostgreSQL's. Shared pieces are `db.Store`, dialect hooks, named-target
  parsing, and `serviceKind`.

## Stored usage prices

Push stores exact Go-computed request prices and their pricing contexts in
ClickHouse. Daily usage queries deduplicate events within the requested window,
join those prices, and return grouped totals. Request rounding, historical
rates, token tiers, and provider adjustments still use the Go pricing rules.

Price records are keyed by their normalized inputs and a digest of the shared
pricing catalog, billing policy, and price format. A catalog change causes the
next push to price the existing mirror again without re-exporting sessions.
Unchanged pushes price new session batches and Cursor events. Old pricing
generations remain available to readers and exporters using them.

Serve stays read-only. Missing prices and reader-specific custom rates are
computed for that request using the current pricing rules while push fills the
shared records. A response never mixes prices from different catalog digests.
Copilot authoritative costs retain their per-session selection and allocation.

The first push after the schema upgrade creates and fills the price tables.
These are derived data; SQLite remains the archive.

## Activity report reads

The Activity report used to rebuild its candidate-session predicate inside every
usage and interval query, parse usage JSON per row, and pair tool events with
their next message through an inequality join that grew with the square of each
session's size. Four changes keep the same output with less work:

**Candidate IDs travel as an external table.** Session discovery runs once. The
selected IDs are attached to the request context as a native external table
named `activity_candidate_ids`, and every later query reads
`SELECT id FROM activity_candidate_ids`. No candidate SQL is repeated and no
`IN (...)` list is inlined into statement text.

**Tool events pair per session in Go.** Serve loads the candidate sessions'
messages and terminal tool events in two ordered scans, then pairs each tool
event with the next stamped message or the next tool event per session. A
segment tree over message timestamps finds the lowest eligible ordinal even when
timestamps run backwards, so ordinal and timestamp behavior matches the old SQL,
including reversed clocks and the tail pairing after the last message.

**Usage is stored, not parsed.** The insert-maintained `usage_messages` table
stores token counters that its view computes from `token_usage` at insert time,
together with the usage-relevant message columns and a time index. The usage
query reads it instead of `messages`, and `messages` keeps only the raw JSON.
Its replacement key is the source message key, not the timestamp, so a retry
that corrects a timestamp or removes usage still replaces the earlier row.

**Terminal events are snapshotted per session version.** The insert-maintained
`terminal_event_snapshots` table stores the latest terminal tool event timestamp
per `(session_id, push_version)`. Candidate discovery filters on it before
version resolution instead of scanning `tool_result_events`.

Both derived tables use `ReplacingMergeTree(revision)` with
`revision = push_version * 2` for backfilled rows and `push_version * 2 + 1` for
rows written by the materialized view. A live insert therefore beats a
concurrent backfill of the same version. Readers compare each row with
`sessions.push_version`. A push writes messages before it publishes the session
row, and an interrupted push never publishes it, so usage rows count when their
version is at or above the session's; an exact match would report no usage for
that session until its next successful push. Rows that a shorter republished
session left behind stay below the session's version and never surface. Terminal
snapshots are written by the session insert itself and match exactly.

The snapshot view runs inside every `sessions` insert, and ClickHouse reads a
view's joined table in full. The view therefore limits `tool_result_events` to
the sessions in the inserted block, which keeps the cost tied to the push batch
and not to the size of the mirror. Materialized views do not see deletes, so
removing a session from the mirror also clears its rows in both tables. The
startup fills record completion in `sync_metadata` only after the work finishes,
so an interrupted startup repeats the fill without touching source tables. A
read-only role cannot run the fill and fails the compatibility check until a
capable push completes it.

**Freshness is checked from part metadata.** Every report request first asks
whether the mirror changed. That source probe used to scan `sessions`,
`messages`, `usage_events`, both pricing tables, and `sync_metadata` for their
counts and maxima on every request. The store now hashes the active
`system.parts` rows of those six tables, plus `usage_session_snapshots` and
`prepared_usage`, which is metadata and reads no data, and reuses the last probe
result while the hash is unchanged. A merge changes the parts without changing
the data, so the hash is never the report token: on a hash miss the store
recomputes the original probe and caches it under the new hash, and only a real
data change moves the token or resets pagination. The mutex protects only the
cached pair, never a query, and an error is not cached. This is whole-mirror
invalidation; any insert, delete, or merge on one of those tables triggers one
full probe. Reading `system.parts` needs its own grant, described in
[ClickHouse sync](../clickhouse-sync.md#3-serve-the-dashboard).

## Prepared usage

Usage, analytics, and Activity reports read date-ordered usage rows. Computing
those rows from `messages` and `usage_events` on every request dominated report
time on small hosts, so the mirror prepares them ahead of time.

**Complete snapshots.** Each push reads a session and its usage from the archive
in one transaction and writes them to `usage_session_snapshots` as one versioned
snapshot. A retry of a failed batch reuses the payload it already loaded, so one
push version never mixes two archive states.

**Refreshed table.** The refreshable materialized view `prepare_usage` turns the
snapshots into `prepared_usage` every 15 minutes, and every push starts a
refresh. Rows are sorted by their stored UTC time, so a day read touches only
that day's granules. Each row stores its price model, price key, and a copy of
its price record, stamped with the pricing digest it was priced under. The table
comment names the query that built it; a table built by another query is dropped
and rebuilt on the next schema step, and a rebuild that stopped halfway finishes
on the next start. The schema step adds a missing projection only with the
refresh stopped, and always starts it again.

**Refresh without a table exchange.** Every refresh of the view ends with
`EXCHANGE TABLES`, which ClickHouse runs as a `renameat2` call with the
`RENAME_EXCHANGE` flag on the database's metadata files and has no fallback for.
Filesystems without that flag, such as NFS, fail every refresh with
`UNSUPPORTED_METHOD`, and the table is never filled. The schema step therefore
exchanges two empty scratch tables in the mirror database. When that fails, it
does not create the view, drops one an earlier version created, and creates the
first rebuild lock if none exists. Probe tables that a killed push left behind
are dropped once they are an hour old.

The lock table is what tells pushes to rebuild. A push without the view rebuilds
only while a lock exists and the newest one is not retired. A database that can
swap tables has no lock. A push there that finds the view missing, as while a
schema step recreates it, never rebuilds.

Each push in rebuild mode rebuilds the table itself before it returns. It runs
the view's query into a new `prepared_usage_build_<n>` table and moves that
table's parts into `prepared_usage` with `REPLACE PARTITION`, which replaces all
the parts at once. Readers see the previous rows or the new ones, never a mix,
and read them as they read rows the view prepared. A push skips its rebuild when
the stored stamp equals the current one, both before and after it takes the
lock. After publishing, the holder rebuilds once more if the snapshots changed
meanwhile. If they changed again, it logs that and leaves the rest to the next
push. Without the view, nothing refreshes on a schedule, and a failed rebuild
waits for the next push. Because `prepared_usage` is then not a refreshable
view's target, native backups include its rows without
`backup_data_from_refreshable_materialized_view_targets`.

Rebuilds take turns, as the view's refreshes do. A read checks freshness and
fetches rows in separate queries, so an older build must never replace a newer
one between them. The lock is a numbered `prepared_usage_rebuild_lock_<n>`
table. A push takes the lock by creating the table with the next number, which
only one push can do. The push drops the older lock tables; the newest stays
after release, so one lock table persists and numbers never repeat. A push that
finds the lock held skips its rebuild, and reads prepare the changed sessions
themselves meanwhile.

A lock's comment holds its state as JSON, which ClickHouse changes atomically.
`status` is `held`, `released`, or `retired`. A held lock also carries
`renewed_at`, the ClickHouse server's Unix time when its holder last renewed it.
A comment that is not valid JSON or has another status fails every read of the
newest lock. Each push logs the error and skips its rebuild, and the schema step
fails. The error names the lock and ends with the statement that repairs it.

A lock can outlive its holder:

- The holder renews the lock's timestamp every ten minutes. A lock not renewed
  for an hour belongs to a stopped push, and the next push takes the next
  number.
- A push that paused between reading the locks and creating its own can create a
  number a newer push already retired. Right after creating its lock, a push
  checks for a higher number and gives its own up if one exists.
- The build table carries the lock's number, and only a lock holder creates one.
  A new holder drops every build table with a lower number. A publish still
  reading one finishes before the drop returns, and a later one fails without
  its table, so older rows never land after newer ones. A push also checks for
  a newer lock before publishing and discards its build if one exists.
- A push drops its own build table when it finishes, so a leftover exists only
  between a killed push and the next rebuild.
- When a later schema step finds that the database can swap tables, as when the
  data directory moved off NFS, it first creates one more lock number with
  status `retired` and drops the older locks and every build table. A rebuild
  that already took a lock finds the newer lock and discards its build;
  dropping its build table waits for a publish already running and fails a
  later one. A push that tries to take the lock afterwards finds it retired
  and skips. Only then does the schema step create the view. Because a push
  rebuilds only while a lock exists, a database with no lock has no rebuild to
  fence.
- If the schema step stops after retiring the lock but before creating the view,
  a push finds the newest lock retired and no view. The push first checks that
  the database can still swap tables. If it can, the push retires again,
  creates the view, and starts its refresh. If it cannot, the push releases
  the retired lock and rebuilds.
- Retirement can pause after reading the locks while pushes take newer numbers,
  so it checks for a higher number right after creating its retired lock, as a
  push does after taking a lock. If one exists, it retires again above it.
- If the database later loses the ability to swap tables, the schema step marks
  the retired lock released and then drops the view. From then on, each push
  rebuilds. A schema step that also replaces `prepared_usage` drops the view
  before it releases the lock. A push in that gap finds the newest lock
  retired and no view, and its swap check sends it to rebuild instead of
  creating the view.

**Currency is proven per read.** A read uses prepared rows only when every
session has a current snapshot. Sessions pushed since the last refresh form a
delta: their rows are prepared and priced in Go once per snapshot, sent to the
read as an external table, and replace that session's stored rows. A refresh
empties the delta. Before the first refresh, or while a session lacks a current
snapshot, reads use raw rows. When the stored price copies no longer match the
price records, for example after the records were cleared, reads keep the
prepared rows and price them through the per-request join.

## Kept reads

Serve keeps the results of recent reads in memory so a repeat request between
pushes reads no rows from ClickHouse; it still runs the small metadata queries
that check whether its inputs changed. Each kept read has one slot per selection
(filter, range, and similar inputs), and its key names the rows it read: the
prepared usage stamp and the parts of the small tables it joins, the candidate
sessions and their push versions, or the parts of the tables it scanned. A push
that changes those replaces the slot; a push that does not leaves it in place.
Usage reads filtered by `active`, `stale`, or `unclean` termination compare
session times with the current time, so their rows are never kept. Kept reads
include usage rows, analytics session listings, Activity pairing inputs per
session version, candidate listings, project label maps, and the whole report of
an ended range.

Nothing rebuilds kept reads in the background. The first request after a push
that changes a read's rows pays for that read.

**Reports on disk.** When a client opens an ended day, its report is written to
the report cache described in
[ClickHouse sync](../clickhouse-sync.md#agentsview-clickhouse-serve), one file
per selection and one directory per mirror. Nothing prepares days ahead of time,
so a day no one opens costs no disk. A file names the binary that wrote it and
the report's key, so a new build or a change to that day's rows rebuilds it.
Every open, whether served from memory or from the file, refreshes the file's
modification time. At startup and about once a day serve removes reports no one
has opened for 30 days, and temporary files that interrupted writes left more
than an hour ago; there is no other size or count limit.

## Tradeoffs

Push copies stars and pins from SQLite, but the ClickHouse UI cannot change
them. PostgreSQL serve does allow those writes in the shared database. The UI
already hides the controls when settings report read-only.

Several machines can share one database without ownership conflict detection.
Two archives that reuse a session ID overwrite each other.

Until a merge, both old and new `push_version` rows can exist on disk. `final=1`
hides the stale ones from readers.

## Tests

Unit tests cover dialect rendering, config, TLS checks, and fingerprints.
Integration tests use the `chtest` tag against
`clickhouse/clickhouse-server:26.8` or `TEST_CLICKHOUSE_URL`. Tests of the
refresh without a table exchange start a second server under a seccomp profile
that fails every `renameat2` call with a flag, as NFS does, or use
`TEST_CLICKHOUSE_NO_EXCHANGE_URL`. `make test-clickhouse` is the suite. Do not
point it at a live mirror.
