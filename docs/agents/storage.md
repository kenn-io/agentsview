# Storage Rules

Read this file before changing SQLite, PostgreSQL, CockroachDB, DuckDB, archive
resync, or storage queries.

## SQLite Archive

SQLite is the persistent archive. Never delete, drop, truncate, or recreate it
to handle a data-version change.

Use non-destructive schema migrations such as `ALTER TABLE` and `UPDATE`. A
parser change that needs a full resync must build a fresh database, sync source
files, copy orphaned sessions from the old database, and swap the files
atomically. Preserve sessions even when their source files no longer exist.

## Backend Parity

- Keep observable behavior and query shape aligned between SQLite and
  PostgreSQL/CockroachDB when practical. Match queries, indexes, aggregations,
  filters, and ordering unless a documented constraint requires a difference.
- Do not fix correctness or performance in only one primary backend unless the
  user limits the task to that backend. If implementations must differ,
  explain why and preserve the same behavior.
- DuckDB is a derived mirror and is not part of this parity rule.

## Canonical Bun Ownership

- The Bun model registry in `internal/db/bunmodel` owns the logical schema
  shared by SQLite, PostgreSQL, and DuckDB: table names, columns, types,
  defaults, constraints, and indexes. It does not require every operational
  query to have identical SQL. Do not add a backend-local copy of a common
  table or column projection.
- `db.BunStore` owns every server-facing `db.Store` query, scan, reduction, and
  supported mutation. Concrete stores may add lifecycle, synchronization,
  operational metadata, and narrowly scoped full-text or vector capabilities;
  they must not shadow a common Store method.
- All application query execution and transactions flow through guarded
  `bun.IDB` handles. Raw SQL constructed with `bun.IDB.NewRaw` is still
  Bun-owned execution: it retains dialect formatting, query hooks, and the
  backend's snapshot or serialization guard. SQLite's `Reader` and writer
  facade are Bun-backed for the same reason.
- Direct `database/sql` access is limited to opening and configuring driver
  pools, connection-local commands such as SQLite `PRAGMA` and `ATTACH` or
  DuckDB `USE`, handle swap/drain/close lifecycle, connector state, and
  unavoidable compatibility or capability probes. Keep each such seam inside
  its backend adapter and document why Bun cannot own it.
- Attached archive recovery pins a guarded `bun.Conn`: the adapter owns
  `ATTACH`/`DETACH` and temporary-table lifecycle on that connection, while
  canonical child copies use registry-derived `INSERT ... SELECT` projections
  through the connection's `bun.Tx`. Explicit SQLite transforms remain only
  where physical IDs, pins, provenance, legacy relationships, or legacy
  content sanitization must be remapped.
- Backend-specific query construction is limited to this closed set of seams:
  lifecycle and connection-local operations; canonical schema creation,
  convergence, and validation; replication or mirror synchronization and
  operational metadata; adapter-supplied timestamp ordering and compatibility
  or capability probes; and narrowly scoped full-text or vector
  implementations. Keep each difference behind the backend adapter or search
  capability boundary. The server-facing Store policy, filtering, hydration,
  and reduction remain shared. A new non-search seam requires an explicit
  design update; a backend-specific query plan alone is not one.

### Bun placeholders

- Write Bun placeholders (`?` or indexed `?0`, `?1`, and so on) in every query
  executed through Bun. Never pass driver-native placeholders such as
  PostgreSQL `$1`; Bun must format values for the active dialect.
- Escape a literal question mark as `\?` so Bun does not consume it as a
  placeholder. Use indexed placeholders when one argument is referenced more
  than once.
- Use `bun.List` for portable bounded value lists. PostgreSQL-native arrays use
  `pgdialect.Array` with forms such as `= ANY(?0)` when the adapter genuinely
  needs array semantics; do not pass an ordinary Go slice to a scalar
  placeholder.
- Bun formats arguments into the SQL sent to the driver. Large lists therefore
  enlarge the formatted query and its hook/log record instead of becoming a
  driver-side bind array. Chunk bounded reads and writes, keep sensitive
  values out of ad hoc logging, and inspect the formatted query when
  diagnosing placeholder or dialect failures.
- Canonical slice writes use a 16 MiB approximate dynamic-payload budget,
  matching the largest tool result the archive deliberately persists. This is
  a pre-format row-payload target, not a statement-length, row-count, or
  bind-variable guarantee: SQL syntax, escaping, and Bun's intermediate copies
  add overhead. One larger logical row is written alone rather than split
  across statements.

### Timestamp compatibility

- Canonical non-empty timestamps use the layouts accepted by
  `bunmodel.ParseTimestamp`, normalize to UTC, and persist at microsecond
  precision on every backend. Empty timestamps are unavailable.
- Unsupported provider message timestamps are blanked during archive ingestion
  and counted as a validation repair. The data-version 84 rebuild repairs
  already archived live, orphaned, and trashed sessions so all backends read
  the same canonical timestamp shape.
- PostgreSQL, DuckDB, SQLite tool-result rows, and other common timestamp models
  remain strict: unsupported non-empty values reject the session write or
  replication transaction. Correcting the source value makes the next
  canonical rewrite eligible to succeed; failed target transactions do not
  advance their synchronization cursor.

### Usage cache divergence

SQLite's aggregate usage APIs read timezone-specific daily rollups from a
disposable sibling database. Normalized, unpriced facts in the same database are
the exact build substrate, not the warm aggregate read path. Per-session detail
remains on the live row path, and PostgreSQL continues to aggregate its live
normalized archive rows. The live path is never a fallback for a failed or stale
SQLite aggregate read. Both implementations are co-maintained under the same
behavior contract: daily usage, top sessions, billed session counts, relaxed
matching counts, and per-session usage must remain observably equal. The
`pgtest` complete-result parity fixture is the acceptance boundary. Track the
PostgreSQL-native optimization in
[issue #1451](https://github.com/kenn-io/agentsview/issues/1451).

The usage cache filename is derived from its format version and the archive
`database_id`. A format or database-ID change selects a new generation; it does
not migrate or rewrite the archive. Facts contain only message- and
usage-event-derived data. Aggregate fingerprints additionally bake the exact
session `agent` and `started_at`, because those fields affect deduplication and
day bucketing. All other session metadata and filters come from the archive read
snapshot. Do not widen or narrow this live/baked boundary implicitly.

The cache format version is also the extractor compatibility version. Bump
`usageCacheFormatVersion` whenever fact extraction, `priceUsageFact`, web-search
fees, deduplication, or rollup semantics change. Catalog and user-pricing
changes are covered separately by the pricing content digest; do not add a
write-only extractor-version metadata key.

Deduplication groups are classified per group at rollup build time. A group is
finalized into daily rows only when its resolution provably cannot vary with the
query window or live filters: every member shares one source session and one
local date, general (`source:`/`usage:`) groups additionally share one model and
headless state, no member links snapshot and general dedup, no member carries a
Copilot authoritative cost, and the group's identity appears in no other cached
session (nor, for usage keys, in the Cursor fact store). Only the remaining
irreducible groups go to the timezone-specific exception tier that resolves
narrow rows at read time, preserving the window-scoped dedup semantics. Cursor
facts stay entirely on the exception tier. Because query windows are whole local
days, a single-date group is inside or outside any window as a unit.

Cross-session identity checks are conservative and served by dedicated
`usage_facts` identity indexes, not a membership table. Whenever a fill, Cursor
batch, or deletion changes the set of dedup identities a session (or the Cursor
store) contributes, it must, in the same cache transaction, delete the timezone
rollup installs of every other session holding a changed identity; rollup
installation re-verifies inside its transaction that no finalized identity
gained an outside member and retries as a moved source otherwise. A finalized
daily row must never survive gaining a sibling.

Treat a usage-cache file as identifiable only after both its SQLite
`application_id` and `usage_cache_metadata.cache_kind` match. Filename matching
alone never permits deletion or replacement, and generations are not removed
automatically because an SQLite transaction lock cannot prove that no other
process holds an idle handle. If persistent cache storage is unavailable or the
current generation is incompatible, use the same schema and query path in a
process-owned temporary file and warn that the cache will rebuild after restart.

Usage reads are exact. A cold aggregate request fills facts, builds the required
timezone rollups, then reads them in one pinned cache transaction. Verify every
candidate session's facts fingerprint, exact baked metadata, canonical pricing
digest, resolved rate hashes, and Cursor high-water mark. Recheck each changed
source session before installing it. A result is no older than the archive
snapshot captured when the read began. A session confirmed deleted during fill
is dropped from the request. Other fingerprint movement or archive-busy races
are retried at most three times and then fail clearly rather than serving stale
data. `cached_at` is diagnostic only.

Timezone rollup identity includes both the resolved zone name and its rule
fingerprint. Cache-generation retirement cancels detached work immediately but
keeps immutable coordinator pointers and the cache database alive until active
query, backfill, fill, and rollup leases drain.

`sync_marker` is a fingerprint component, not a monotonic version: its trigger
recomputes the maximum of mutable timestamp fields, so it can decrease. A fill
must recheck the full source fingerprint before installation. Do not replace
that recheck with ordering comparisons.

### Usage archive indexes

The usage cache discovers bounded-window candidates through
`idx_messages_usage_timestamp` and `idx_messages_activity_timestamp`, then
extracts each selected session through the index-only
`idx_messages_usage_session_covering` scan. The global activity index is for
usage-cache candidate discovery, not the Activity report; that report continues
to avoid a global timestamp scan. Keep these indexes narrow except for the
single session-keyed covering index that carries `token_usage`.

Changing any of these index column lists rebuilds the affected archive index on
the next writable open, before HTTP readiness, and must log that startup is
waiting for the migration. Read-only opens require the current indexes and may
therefore reject an archive that has not first been opened by the matching
writable version. Treat this as executable/archive version skew, not as a reason
to mutate the archive from a read-only command.

Full resync drops these indexes in the temporary database during the bulk load
(the FTS trade: one post-load build instead of per-row B-tree maintenance) and
must rebuild them before the swap; a failed rebuild aborts the swap because
read-only opens require the indexes.

### Transcript usage identity

Token usage, Claude message/request identities, and source UUID participate in
transcript revision equality. Finalizing a streamed message can therefore bump
`transcript_revision` and `local_modified_at`, invalidate secret-scan freshness,
mark the session updated for read-progress/UI purposes, and enqueue the normal
artifact, recall, PostgreSQL, and DuckDB refreshes. Full resync reconciliation
must compare the same fields so incremental and resync paths agree. A no-op
message replacement preserves existing secret findings; changed transcript
content clears them for a fresh scan.

## DuckDB Mirror

- Treat DuckDB as a disposable read mirror of SQLite, never as a system of
  record. Deleting the mirror must lose nothing.
- Do not add in-place mirror migrations. A schema or source-data version change
  must bump `internal/duckdb.SchemaVersion`, rebuild a fresh file, validate
  it, and swap it atomically. Do not add `ALTER` migrations, version-bridging
  reads, or compatibility shims for old mirrors.
- Store every DuckDB push cursor and version in the mirror's `sync_metadata`.
  Never store DuckDB sync state in SQLite.
- Replace whole sessions during incremental updates and gate them with
  per-session fingerprints. Do not add per-table, per-column, or diff-based
  updates.
- Keep Quack read-only. `duckdb push` writes the local mirror; it never writes
  to a remote DuckDB service.
- Replace a file only after identifying it as an agentsview DuckDB mirror. Fail
  closed for unknown files.

## PostgreSQL Integration Tests

Run PostgreSQL integration tests only against a dedicated test database. The
tests create and drop the `agentsview` schema.

Use `make test-postgres` to start the test container and run the suite. It
leaves the container running. If you started that container, use
`make postgres-down` when it is no longer needed.

To use an existing dedicated instance, run:

```bash
TEST_PG_URL="postgres://user:pass@host:5432/dbname?sslmode=disable" \
  CGO_ENABLED=1 go test -tags "fts5,pgtest" ./internal/postgres/... -v
```
