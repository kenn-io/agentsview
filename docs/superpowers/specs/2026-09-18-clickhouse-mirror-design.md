# ClickHouse mirror and read-only serve

Status: approved design, implementation in progress on the
`clickhouse-support-for-agentsview-server` branch.

## Outcome

Operators can push the local SQLite archive into ClickHouse and serve the
read-only HTTP API and web UI from it, using the same three-step story as
PostgreSQL: configure a target, push (once, in the foreground watcher, or as a
per-user OS service), then `agentsview clickhouse serve`.

SQLite stays the archive. ClickHouse is a one-way mirror. Nothing in this design
deletes, drops, truncates, or recreates the SQLite archive.

## Decisions and evidence

Each decision names the option taken, the evidence, and what it rules out.

### D1. Copy the DuckDB write model, the PostgreSQL operator model

- Write model: whole-session replace gated by a per-session fingerprint stored
  on the mirrored session row, with the push cursor kept in the mirror's own
  `sync_metadata` table (`internal/duckdb/sync.go`, `selectChangedSessions`,
  `finalizeIncrementalPush`). No local sync-state scoping, no PG-style
  boundary-state blob.
- Operator model: config shape, CLI verbs, watcher, service install, and
  isolated serve defaults all follow `pg` (`cmd/agentsview/pg.go`,
  `internal/config/config.go` `loadPGServeBase`).
- Ruled out: DuckDB's rebuild-and-swap file lifecycle (a shared remote database
  cannot be swapped), and PostgreSQL's local watermark plus remote reset
  marker (the fingerprint-on-row model already self-repairs a reset mirror
  because a missing row reads back as an empty fingerprint).

### D2. Driver: `github.com/ClickHouse/clickhouse-go/v2` through `database/sql`

- The official driver. Its `database/sql` interface returns `*sql.DB`, so the
  store follows the same `queryContext`/`Scan` shape as the DuckDB and
  PostgreSQL stores and `database/sql` handles numeric conversions (`UInt64`
  counts into `int`, `Nullable(String)` into `*string`).
- Batch inserts use the documented `Begin` / `Prepare("INSERT INTO t")` /
  repeated `Exec` / `Commit` pattern, which the driver turns into one native
  block insert per statement.
- Connection settings: `final = 1` on every connection so `ReplacingMergeTree`
  deduplication applies to all reads without `FINAL` in query text (verified
  on ClickHouse 25.8, including inside recursive CTEs).
- DSN: `clickhouse://user:pass@host:9000/db?secure=true` or
  `https://host:8443/db`. The `database` config key overrides the DSN path.

### D3. Tables are `ReplacingMergeTree(push_version)`

- Every mirrored table carries `push_version UInt64`, the push start time in
  Unix nanoseconds. Same-key rows from a retried push collapse on merge and on
  read (`final = 1`).
- Ordering keys: `sessions (id)`, `messages (session_id, ordinal)`,
  `tool_calls (session_id, message_ordinal, call_index)`,
  `tool_result_events (session_id, tool_call_message_ordinal, call_index, event_index)`,
  `usage_events (session_id, id)`, `secret_findings (session_id, id)`,
  `starred_sessions (session_id)`, `pinned_messages (session_id, message_id)`,
  `sync_metadata (key)`, pricing tables by their natural keys, source identity
  tables by their PostgreSQL primary keys.
- Row removal uses lightweight
  `DELETE FROM t WHERE session_id IN (...) AND push_version < ?`. ClickHouse
  applies it synchronously for subsequent reads (`lightweight_deletes_sync`
  default). One statement per table per batch keeps mutation count
  proportional to pushes, not sessions.
- Timestamps are `DateTime64(6, 'UTC')`, `Nullable` where SQLite allows empty.
  Reads format them back to RFC 3339 like the DuckDB store's `formatDBTime`.
- Text columns that SQLite never leaves NULL are plain `String`. Session pointer
  fields (`display_name`, `ended_at`, `parent_session_id`, ...) are
  `Nullable`.
- `sessions.last_message_at Nullable(DateTime64)` is computed at push time (max
  message timestamp). It replaces the correlated subquery the other dialects
  use for the session date-range filter (D5).
- Ruled out: plain `MergeTree` with delete-then-insert (a crash between the two
  leaves a session with no content and a matching fingerprint), and
  `CollapsingMergeTree` (needs the old rows to write cancel rows).

### D4. Push order makes every failure self-healing

Per batch of up to 100 changed sessions, with `v` = this push's version:

1. Insert new dependent rows (messages, tool calls, tool result events, usage
   events, secret findings, pinned messages) for the batch.
1. `DELETE ... WHERE session_id IN (batch) AND push_version < v` per dependent
   table.
1. Insert the session rows with `agentsview_push_fingerprint` and
   `source_archive_id`.

A crash before step 3 leaves the stored fingerprint stale, so the next push
re-selects the session. A crash between steps 1 and 2 leaves extra old rows that
step 2 removes on the retry. ClickHouse has no multi-statement transactions, so
this ordering is the consistency mechanism.

Hard-deleted local sessions are removed with `DELETE ... WHERE id IN (...)`
across all tables, driven by the local deletion journal the DuckDB push already
consumes. Filtered pushes list the window without project filters and remove
mirror rows whose project moved out of scope, copying `internal/duckdb/sync.go`
`deleteOutOfScopeMirrorSessions`.

The cursor keys are scoped per source archive so several machines can push into
one database: `agentsview_last_push_cutoff:<archive_id>`,
`agentsview_last_push_at:<archive_id>`,
`agentsview_last_push_machine:<archive_id>`,
`agentsview_push_scope:<archive_id>`. The cursor advances only when a push
finishes with zero session errors. A scope change or `--full` re-fingerprints
every in-scope session and rewrites the ones whose fingerprint differs; `--full`
also deletes this archive's mirror sessions that no longer exist locally.

Ownership: the session row records `source_archive_id`. Pushes do not enforce
cross-archive ownership conflicts the way PostgreSQL's `owner_marker` does. A
session ID that appears in two archives is last-writer-wins. Documented gap.

### D5. Shared filter builder gains a ClickHouse dialect

`internal/db/query_dialect.go` already renders session filters, sorts, and
keyset cursors for SQLite, PostgreSQL, and DuckDB. Add
`ClickHouseQueryDialect()` and three hooks, verified against ClickHouse 25.8:

- `recursiveUnion`: ClickHouse recursive CTEs accept only `UNION ALL`.
- `starredPredicate` and `orphanPredicate`: correlated `EXISTS` returned no rows
  in the spike, so ClickHouse uses
  `id IN (SELECT session_id FROM starred_sessions)` and
  `parent_session_id NOT IN (SELECT id FROM sessions)`.
- `dateEndExpr` uses
  `COALESCE(ended_at, last_message_at, started_at, created_at)` instead of a
  correlated `MAX(m.timestamp)` subquery.

Other dialect values: `?` placeholders, `true`/`false`, `ILIKE` with backslash
escapes and no `ESCAPE` clause, `match(col, concat('(?i)', ?))` for regex,
`parseDateTime64BestEffort(?, 6, 'UTC')` for timestamp parameters, `toInt64(?)`
and `toFloat64(?)` cursor casts, `NULLS LAST`.

The existing dialects keep their current SQL byte-for-byte; the hooks default to
the current behavior.

### D6. Store surface

`internal/clickhouse.Store` implements every `db.Store` method and is asserted
in `internal/backendcontract`.

- Real SQL: sessions, sidebar index, messages and windows, resume model counts,
  session activity and timing, substring and regex search, session search,
  secret findings, session version, stats, projects, agents, machines, machine
  labels and aliases, branches, project identity observations and map, project
  inventory, project rules, worktree candidates, all analytics, trends,
  activity report (plus the artifact, probe, and token extensions), recent
  edits, usage, starred IDs, pinned message lists, trash list.
- `db.ErrReadOnly`: every write, including stars, pins, insights, session
  management, recall, and uploads. This matches the DuckDB store and differs
  from PostgreSQL, where dashboard curation writes land in the shared
  database. Documented gap; the UI already hides those controls when the
  settings response reports read-only.
- Insights reads return empty, matching DuckDB.
- `HasFTS()` is true (ILIKE search is available), `HasSemantic()` is false.
  `SearchContent` validates semantic and hybrid requests first, then returns
  `db.ErrSemanticUnavailable`, matching DuckDB. Vector push is not part of
  this design.

Every statement goes through one `queryContext`/`queryRowContext` pair so a
later transport change touches one place.

### D7. Config

`[clickhouse]` legacy single block, `[clickhouse.NAME]` named targets, and
`default_clickhouse`, with the same normalization, reserved names (`all`,
`local`, and the field names), mixing rules, and defaulting as `[pg]`. The
PostgreSQL section parser and target resolution are generalized so both sections
share one implementation.

Fields: `url`, `database` (default `agentsview`), `machine_name` (default
installation ID), `allow_insecure`, `projects`, `exclude_projects`.

Environment: `AGENTSVIEW_CLICKHOUSE_URL`, `AGENTSVIEW_CLICKHOUSE_DATABASE`,
`AGENTSVIEW_CLICKHOUSE_MACHINE`, applied only to the default target like the PG
variables.

Transport security: unless `allow_insecure` is set or the host is loopback, the
URL must use TLS (`secure=true` for the native protocol or an `https` scheme).
Same policy as PostgreSQL's `CheckSSL`.

### D8. CLI and serve wiring

- `agentsview clickhouse push [target]` with `--all`, `--full`, `--projects`,
  `--exclude-projects`, `--all-projects`, `--watch`, `--debounce`,
  `--interval`. No `--no-vectors` (no vector phase).
- `agentsview clickhouse status [target]` reads counts and the archive's last
  push from the mirror.
- `agentsview clickhouse serve` with the PG serve flag set and the same isolated
  defaults (`loadPGServeBase`), read-only server options as `duckdb serve`.
- `agentsview clickhouse service install|uninstall|status|start|stop|logs`
  reuses the PG service manager with a target kind that changes the unit label
  (`agentsview.clickhouse-watch`), the command (`clickhouse push --watch`),
  and the log file (`clickhouse-watch.log`).
- Push runs through `archiveWriteBackend` like PG and DuckDB: when a daemon owns
  the archive, the CLI posts to a new `/api/v1/push/clickhouse` route;
  otherwise it opens the archive locally. The Go API client is regenerated
  from the OpenAPI document (`frontend/scripts/generate-api-client.mjs`),
  which also refreshes the generated TypeScript client. No hand-written
  frontend change.
- The watcher reuses the DuckDB watch loop shape (`duckDBPusher`, push loop,
  file watcher, unwatched-root poller, one-instance lock named
  `clickhouse-watch`).

### D9. Schema lifecycle

- `EnsureSchema` runs `CREATE DATABASE IF NOT EXISTS`,
  `CREATE TABLE IF NOT EXISTS` for every table, and
  `ALTER TABLE ADD COLUMN IF NOT EXISTS` for columns added after a table's
  first version. It records `agentsview_schema_version` in `sync_metadata`.
  Serve runs the same routine and tolerates permission errors so a read-only
  role can still serve.
- `CheckSchemaCompat` on serve verifies required tables and columns exist and
  names the missing ones.
- `CheckDataVersionCompat` compares `max(data_version)` in `sessions` with
  `db.CurrentDataVersion()`, like PostgreSQL.
- Replicated or `ON CLUSTER` deployments are out of scope; ClickHouse Cloud's
  automatic `SharedMergeTree` conversion needs no change.

### D10. Tests

- Unit tests without a server: dialect rendering, config parsing and target
  resolution, transport-security check,
  fingerprint-covers-every-mirrored-column reflection check, push ordering
  helpers.
- Integration tests under the `chtest` build tag start
  `clickhouse/clickhouse-server:25.8` with the existing testcontainers
  dependency, or use `TEST_CLICKHOUSE_URL` when set. They seed SQLite through
  `dbtest.OpenTestDB` and `WriteSessionBatchAtomic`, push through the
  production path, and assert absolute expectations through the store and
  through `server.New` over `httptest` (session list, session detail,
  messages, search). A second push after a local change proves the fingerprint
  gate and the version-bounded delete; a push with an injected failure before
  the session row proves the retry.
- The cross-backend activity-report parity harness
  (`internal/activity/parity_pgtest_test.go`) gains a ClickHouse leg.
- `make test-clickhouse` and a CI job with a ClickHouse service container mirror
  the PostgreSQL targets.

### D11. Docs and changelog

- `docs/clickhouse-sync.md` leads with what the operator can do, then commands,
  config, security, and the documented differences from PostgreSQL (read-only
  curation, no vectors, no ownership conflicts, eventual merge of replaced
  rows).
- Nav entry in `docs/zensical.toml`, `## Unreleased` changelog entry, a
  ClickHouse section in `docs/agents/storage.md`, task-route and project-map
  rows in `AGENTS.md`, config keys in `docs/configuration.md`, commands in
  `docs/commands.md`, README backend mention.

## Out of scope

Vector search push and semantic search from ClickHouse, hosted raw sync,
curation writes from the ClickHouse UI, ownership-conflict detection, cluster
DDL, and the DuckDB-style local mirror probe or rebuild.
