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

### Codex incremental import state

Four SQLite-only tables support local Codex imports: `parser_checkpoints` holds
resume metadata, `parser_checkpoint_blobs` holds cursor and hash state,
`session_signal_state` holds the incremental signal reducer, and
`tool_call_occurrence_agent_state` holds per-agent result coordinates. The last
table is populated lazily when a call receives a late result. Other providers do
not maintain Codex signal state. Full writes commit the signal seed with the
content and bind it to the stored transcript revision inside SQLite. A failed
seed rolls back the content; there is no post-commit revision read. During a
full resync, the disposable replacement archive defers the tool-call ID and
result metadata indexes until after the bulk load. Rebuilding them must succeed
before the replacement can be installed.

Codex result events also retain a raw-content digest and whether the raw event
participates in the summary. These local fields distinguish events that become
identical after sanitization and preserve whitespace and blocked-result rules.
An older session missing this metadata is reparsed from its source before a late
result is applied. Its first rewrite can advance the transcript revision;
subsequent equal parses remain no-ops. These fields are excluded from exports
and mirror fingerprints.

Large Codex imports use a disposable scratch SQLite database for result
payloads. Publication attaches it to the archive writer and commits content,
checkpoint, and enabled derived state together. Cancellation aborts publication;
cleanup detaches with a context that survives cancellation. Scratch storage is
not an archive or a mirror and is removed after the import.

Tool-result image retention uses the canonical `config.ToolResultImages` policy
on writable SQLite handles. The zero value keeps content. Drop mode projects a
valid inline `data:image/...;base64` block into an `agentsview_image`
placeholder before derived lengths, display comparisons, and persistence. Raw
event digests are captured before projection so distinct provider events remain
distinct and replayed late results stay no-ops. The projection preserves
ordinary text, metadata, block order, unsupported shapes, and future
placeholders. Combined summaries project labeled and anonymous sections using
JSON boundaries, so blank lines inside arrays do not split them. Late result
writes also project the rebuilt summary when older events predate drop mode.
`db strip --images` applies the projection to existing rows one session at a
time. The command updates `tool_calls.result_content` and
`tool_result_events.content` directly in one transaction per session,
recalculates their stored lengths, and keeps every event coordinate and metadata
column unchanged. Each changed session also gets a full secret scan of its
projected transcript inside that transaction, preserving findings with their
current offsets and rule version. A changed session gets the normal transcript
revision, Recall, signal, artifact export, usage notification, and post-commit
revocation sequence. An unchanged session gets none of those publications. Full
resync applies this same projection only to the IDs returned by its trashed and
orphaned session copies, before the replacement is published. Freshly parsed
sessions already carry the projection. Large Codex imports project events before
scratch insertion; staged summaries and signals use that projected content. The
command counts raw `tool_calls.result_content` and `tool_result_events.content`
bytes separately from decoded image bytes. `db compact` reports file-size
reclamation separately.

Transcript-only and usage-only writes omit parser checkpoints because resumable
hash state can contain raw trailing transcript bytes. They retain staged parsing
but publish projected messages and tool metadata without staged output. Late
result updates use the same projection as newly inserted messages.

### Rate-limit snapshots

`rate_limit_snapshots` is a SQLite-only vendor-data table, in the same
category as `cursor_usage_events` and the four Codex incremental-import
tables above: it is out of scope for the SQLite/PostgreSQL/DuckDB parity
rule below. It is vendor-keyed (`vendor`, `'codex'` today) from the start
so a future vendor can add rows without a schema change or a migration:
`account_id`, `account_label`, `scope_label`, and `details` are reserved
for a vendor whose rate-limit source has that shape, and stay `''` on
every Codex row -- verified against `~/.codex/sessions` rollouts: no
account id, user id, email, or org field appears in `session_meta` or
`token_count` payloads, so a Codex snapshot's identity omits an account
entirely (see `docs/internal/session-format-sources.md`). An
`account_id` filter scopes vendors that have
accounts, so both `LatestRateLimitSnapshots` and
`RateLimitSnapshotHistory` match a nonempty `account_id` against
`(account_id = '' OR account_id = ?)` rather than a bare equality,
letting an account-less vendor's rows (Codex today) pass through
instead of being excluded by someone else's account filter.

It stores each `rate_limits` observation a Codex `token_count` event
carries beside `info.last_token_usage` (one row per rate-limit window).
It is written with an upsert against a unique `dedup_key` (source
session id + observed timestamp + limit id + window kind + `ordinal`),
never a delete-then-reinsert, so both a full parse (which sees the
whole transcript every time) and an incremental parse (which only sees
the appended tail) can write to it without duplicating or losing rows.
A `dedup_key` collision against a row whose `session_id` is already
NULL (a resync copy for a session absent from the destination -- see
`CopyRateLimitSnapshotsFrom`) reattaches that row to the incoming
session and refreshes its other fields instead of being ignored, since
the copy preserves `dedup_key` unchanged and the session reappearing
later (a fresh parse reproducing the identical key) would otherwise
collide with, and lose to, the stale detached row forever. A collision
against a row that already has a session attached is still a no-op.
`ordinal` is the source `token_count` event's 0-based
position among every `token_count` event in the rollout file, assigned
by the parser (`ParsedRateLimitSnapshot.Ordinal`) and stable across a
full parse, an incremental tail parse resuming from a cached or
reseeded cursor, and any later re-parse of the same file: without it,
two distinct `token_count` events landing on the same observed-at
second would produce the same `dedup_key`, and the second event's row
would be silently dropped by `INSERT OR IGNORE` instead of persisted.
It also participates in `observation_key` for the same reason -- two
such events would otherwise merge their sibling windows into one
`LatestRateLimitSnapshots` bucket. A single malformed
observation (e.g. one missing `limit_id`) is skipped rather than failing
the whole write, so it cannot take down the rest of the batch or the
session ingestion it rode in on. `session_id` is nullable (`ON DELETE SET
NULL`) so a row survives its source session being deleted. `resets_at` is
also nullable: Codex can report a window with no reset time, and that is
kept distinct from a window that resets at the unix epoch all the way
through the Go types and the API response (an absent field, not `0`), so
the Usage page can tell "no known reset time" apart from "resets right
now". A full resync copies existing rows into the replacement archive
the same way model pricing is copied (`CopyRateLimitSnapshotsFrom`), but
only after orphaned sessions are restored: `session_id` is a foreign
key, and copying before restoration could violate it for a snapshot
belonging to an orphaned session, aborting the whole copy. The copy also
NULLs `session_id` for any row whose session does not exist in the
destination even after restoration -- a session resync intentionally
does not restore, such as one superseded by a reparse under a different
id, or one excluded as parser-excluded -- rather than copying that id
unchanged and violating the same foreign key; `dedup_key` is preserved
from the source unchanged either way. The copy also skips any row whose
session was rebuilt by the resync's own reparse rather than merely
restored: it copies a row only when `session_id` is NULL, absent from
the destination's `sessions` table, or one of the ids the orphan copy
restored without reparsing, so a rebuilt session's superseded rows
cannot resurrect on top of its fresh, current ones.
`LatestRateLimitSnapshots` (the
`/current` endpoint) resolves "latest" per (vendor, machine, account_id,
limit_id) bucket -- one level above window_kind -- and returns every
window belonging to that bucket's single newest observation. Ranking
each window_kind independently instead would let a window that stops
being reported (e.g. a session moving from primary+secondary to
primary-only) keep surfacing its last-known row forever, since no newer
row for that window_kind ever arrives to supersede it. `plan_type` is
deliberately not part of this bucket, or of any other window identity in
this codebase: Codex reports it as a label that can flip between
`"pro"` and empty for the same window from one observation to the next,
not a stable identity component, so partitioning on it would let a stale
plan-keyed bucket coexist alongside the newest observation instead of
being superseded by it, and (on the history/frontend side) would split
one window's history across two chart series or silently drop half of
it. `plan_type` and `limit_name` are instead resolved independently as
the latest non-empty value ever observed for the bucket -- so a bucket
whose newest observation happens to omit one or both labels still
displays the last value seen for it rather than blanking the card --
and are otherwise pure display labels carried on each row, never a
grouping key. `RateLimitSnapshotHistory` still returns every row over
time regardless of the current-snapshot grouping, and its query,
downsampling, and frontend cache key never filter or key by `plan_type`
either. A window's identity, for both the current-snapshot and history
paths, is (vendor, machine, account_id, limit_id, window_kind) -- the
same fields `RateLimitCardIdentity` on the frontend groups by. PostgreSQL
and DuckDB implement the read-side
`Store` methods as no-ops returning an empty result, so the Usage page's
rate-limits section is simply hidden when either backend is the active
read store. `LatestRateLimitSnapshots` and `RateLimitSnapshotHistory` both
probe once (cached per `*DB`) whether `rate_limit_snapshots` exists and
return an empty result instead of erroring when it does not, since
`OpenReadOnly` tolerates an older, otherwise-compatible archive that
predates the table. A write that replaces a session's messages wholesale
(an authoritative reparse superseding a fallback parsed at
`parser.DataVersionNeedsRetry`, or any other full delete-and-reinsert)
must delete that session's `rate_limit_snapshots` rows in the same
transaction before inserting the new set via
`InsertRateLimitSnapshotsReplacingSession`, while a normal incremental
parse keeps appending through `InsertRateLimitSnapshots` without
deleting.

## Archive Content Policy

`archive_content` (`internal/config.ArchiveContent`) narrows what the SQLite
archive stores. The `*db.DB` handle is the single authority: `Open` variants and
`sync.NewEngine` only tighten it, never loosen it, and every write path projects
sessions and messages through `internal/db/archive_content.go` before rows are
written.

- Route any new session, message, tool call, signal, or finding write through
  the existing projection helpers instead of checking the policy inline.
- Resync copies archived rows with `ATTACH`, which bypasses the write path.
  `applyArchiveContentToCopiedSessionsTx` mirrors the Go projection in SQL for
  the orphan and trash copies. Keep the two in step when either changes.
- Copied tool renderings use exact reconstructed text where possible. When
  stored inputs cannot reconstruct a recognizable tool rendering, transcript
  projection keeps the preceding prose and tool label but discards the
  remaining message tail, whose argument boundaries are unknown.
- Usage-only rows retain normalized context/output token values and their
  presence flags as well as `token_usage`; model-mix totals use these columns.
- PostgreSQL pushes record `prompt_evidence_discarded` per session from the
  source archive policy. Automation audits preserve the stored verdict only
  when this marker explains the missing prompt evidence. Full-content rows
  remain eligible for corrections.
- Usage-only mode disables vector building, serving, and export. Opening the
  writable archive clears the local message and recall indexes under the
  vector write lock. PostgreSQL pushes clear all generations of indexed
  content for owned sessions, including sessions already deleted locally.
  Cleanup finds candidates in PostgreSQL and rechecks ownership under the
  session lock before deleting them.
- Compute derived values (signals, secret findings) from the projected messages
  so a later recompute from stored rows reproduces them.

`db migrate --images` moves retained inline payloads out of SQLite and into the
asset store. It writes each decoded payload to
`{dataDir}/assets/<sha256hex><ext>` before any UPDATE commits in the session
transaction. If that transaction fails, complete objects remain unreferenced on
disk and are reused by a matching retry; there is no automatic cleanup of
unreferenced assets. An existing object is reused only when its byte count and
SHA-256 digest match. A missing or corrupt object is replaced while the source
bytes remain available. The inline block is replaced with an `agentsview_image`
placeholder whose `image_ref` field holds `asset://<sha256hex><ext>` and whose
`text` field carries a markdown image
`![Image: <type>, <n> bytes](asset://<sha256hex><ext>)`. The discriminator for
migrated blocks is `image_ref`. A later keep-mode reparse or full resync can
restore inline bytes from provider source files. Only the four passive media
types are migrated: `image/png`, `image/jpeg`, `image/webp`, and `image/gif`.
SVG payloads stay inline. A separate serving host needs the matching
`{dataDir}/assets` directory with the copied database content. The command
otherwise follows the same transaction, revision, and publication sequence as
`db strip --images`. Back up `{dataDir}/assets` together with the archive.

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
fees, deduplication, rollup semantics, or query-time model canonicalization
change. Catalog and user-pricing changes are covered separately by the pricing
content digest; do not add a write-only extractor-version metadata key.

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
gained an outside member and, when one did, reclassifies against the newly
committed facts rather than failing the caller. A finalized daily row must never
survive gaining a sibling.

Treat a usage-cache file as identifiable only after both its SQLite
`application_id` and `usage_cache_metadata.cache_kind` match. Filename matching
alone never permits deletion or replacement. Lease-aware generations hold a
shared cross-process lease for every open SQLite pool; retirement requires the
exclusive lease plus a fresh application-ID, cache-kind, protocol-version,
format-version, and source-database-ID check against the exact filename. Keep
the lease file after retirement so a racing opener cannot lock a replacement
inode. Preserve pre-protocol generations because an older binary may hold an
idle handle without a lease, and preserve generations newer than the running
format so a downgraded binary does not force the newer one to rebuild. If
persistent cache storage is unavailable or the current generation is
incompatible, use the same schema and query path in a process-owned temporary
file and warn that the cache will rebuild after restart.

Usage reads are exact. A cold aggregate request fills facts, builds the required
timezone rollups, then reads them in one pinned cache transaction. Verify every
candidate session's facts fingerprint, exact baked metadata, canonical pricing
digest, resolved rate hashes, and Cursor high-water mark. A result is no older
than the archive snapshot captured when the read began, and may be newer for a
session whose facts were refilled meanwhile. A session confirmed deleted during
fill is dropped from the request. `cached_at` is diagnostic only.

The layers are kept apart so a live archive cannot veto a read. A fill reads one
session's facts and that session's source version inside a single archive read
transaction, installs both together, and reports the version it actually read,
which may be newer than the one the caller asked for. Rollup aggregation then
reads committed facts out of the usage cache only; it never touches the archive,
so an append landing mid-build cannot abort it. An install is stale when the
fact versions it was built from differ from the ones the cache now holds, and
only those installs are rebuilt. Sessions written during a build are refilled by
their own mutation notification and appear in the next aggregation, so staleness
of a few seconds is expected and intended. Do not reintroduce a whole-snapshot
recheck against the archive: validating a snapshot against a source that changes
one session at a time livelocks the request.

Timezone rollup identity includes both the resolved zone name and its rule
fingerprint. Cache-generation retirement cancels detached work immediately but
keeps immutable coordinator pointers and the cache database alive until active
query, backfill, fill, and rollup leases drain.

`sync_marker` is a fingerprint component, not a monotonic version: its trigger
recomputes the maximum of mutable timestamp fields, so it can decrease. A fill
must read the full source fingerprint in the same transaction as the facts it
installs. Do not compare fingerprints for ordering, and do not skip a refill
because a cached fingerprint merely looks newer.

### Activity report index

Activity session selection checks terminal tool-execution events even when a
session's `ended_at` predates the report. Keep the partial
`idx_tool_result_events_terminal` index on `(session_id, timestamp)` aligned
between SQLite and PostgreSQL. It includes completed and errored executions with
non-null timestamps, so the lookup can skip unrelated result payloads and seek
directly to the report's lower bound.

The next writable SQLite open or PostgreSQL schema setup builds the index once
for existing archives. PostgreSQL push must also detect its absence before
taking the schema-current fast path. Creating the index scans existing tool
results and can delay that first startup; it does not require a session resync.

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

### Tool result summaries

`tool_calls.result_content` is a display summary derived from the call's
`tool_result_events` rows at sync time. When a call has exactly one event and
the summary equals that event's content, the summary is not stored: the column
is empty while `result_content_length` still records the summary's size. That
pair, an empty column with a non-zero length, tells a reader to take the text
from the single event. Multi-event summaries, single-event summaries that differ
from their event, calls with no events, and blocked categories store exactly
what the parser produced. Load tool calls through the message loaders, which
refill the summary once events are attached; a query that selects the column
directly must apply the same fallback, and PostgreSQL and DuckDB apply the same
write rule so their tool-call fingerprints match SQLite. Anyone reading the
archive or a mirror by hand sees the empty column and must join the events table
to recover the text.

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
