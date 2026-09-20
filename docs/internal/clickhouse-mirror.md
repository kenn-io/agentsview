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
empty. `HasFTS()` is true (ILIKE). `HasSemantic()` is false.

## Boundaries

- SQLite is never deleted, dropped, truncated, or recreated for a ClickHouse
  schema or data-version change.
- ClickHouse is not the system of record. A destroyed mirror is rebuilt by
  pushing again.
- Vector search and hosted raw sync stay off this path.
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

**Tables are `ReplacingMergeTree(push_version)`.** `push_version` is the push
start time in Unix nanoseconds. Same-key rows from a retry collapse on merge and
on read. Timestamps are `DateTime64(6, 'UTC')`. `sessions.last_message_at` is
computed at push time so date filters do not need a correlated `MAX`.

**Push order is the consistency mechanism.** ClickHouse has no multi-statement
transactions. Per batch of changed sessions, with `v` this push's version:

1. Insert dependent rows (messages, tool calls, result events, usage, findings,
   pins).
1. `DELETE ... WHERE session_id IN (...) AND push_version < v` per dependent
   table.
1. Insert session rows with fingerprint and `source_archive_id`.

A crash before step 3 leaves the fingerprint stale, so the next push re-selects
the session. A crash between 1 and 2 leaves extra old rows that step 2 removes
on retry.

**Shared filter builder, ClickHouse dialect.** Existing SQLite, PostgreSQL, and
DuckDB SQL stay byte-identical. ClickHouse uses `UNION ALL` in recursive CTEs,
`IN (SELECT)` instead of correlated `EXISTS`,
`parent_session_id IS NULL OR NOT IN (SELECT id FROM sessions)` for orphans
(`NULL NOT IN (...)` is unknown and would hide NULL-parent rows), `ILIKE` with
the default backslash escape and no `ESCAPE` clause (ESCAPE landed in 26.6; the
pin is 25.8), and `toStartOfDay` / `toStartOfWeek` / `toStartOfMonth` instead of
`date_trunc`.

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
`clickhouse/clickhouse-server:25.8` or `TEST_CLICKHOUSE_URL`.
`make test-clickhouse` is the suite. Do not point it at a live mirror.
