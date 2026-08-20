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
