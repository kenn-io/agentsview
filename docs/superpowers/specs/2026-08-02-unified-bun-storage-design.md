# Unified Bun Storage Design

## Goal

Replace the independent SQLite, PostgreSQL, and DuckDB `db.Store` query paths
with one shared Uptrace Bun-backed implementation. Define the durable schema
once, preserve the existing role of each database, and limit engine-specific
code to connection lifecycle, operational metadata, and small full-text/vector
search capabilities.

## Current Problem

AgentsView currently exposes one `db.Store` contract but implements it three
times. The local SQLite archive, PostgreSQL read store, and DuckDB read store
repeat query construction, row scanning, filter handling, analytics, usage, and
curation behavior. A small shared query-dialect helper reduces some filter
drift, but most behavior still lives in backend-specific packages.

This structure makes every storage-visible feature a three-path change. It also
allows the physical schemas and result conversions to diverge even when the
engines support practically identical tables and SQL.

## Scope

This refactor will:

1. Add Uptrace Bun as the common schema, query, execution, transaction, and row
   scanning layer.
1. Define one canonical model registry for durable cross-backend data.
1. Implement a dedicated Bun dialect for DuckDB.
1. Replace the three common `db.Store` implementations with one shared store.
1. Route parser ingestion, PostgreSQL push, and DuckDB mirror population through
   the canonical Bun models and transaction/upsert helpers.
1. Migrate existing SQLite and PostgreSQL databases forward in place.
1. Rebuild and atomically replace DuckDB mirrors at a new schema version.
1. Retain small backend-specific full-text and vector implementations.

The backend roles do not change in this work:

- SQLite remains the persistent writable archive and system of record.
- PostgreSQL remains a synchronized remote store with its current read and
  limited curation/write capabilities.
- DuckDB remains a disposable read mirror. A local push writes it; serve and
  Quack paths remain read-only.

## Architecture

`internal/db` remains the domain package and owner of the `db.Store` contract.
It gains the canonical persistence models and one Bun-backed common store.

The existing concrete types become thin compositions around that common store:

- SQLite `DB` retains writer/reader pool ownership, write serialization,
  checkpointing, reopening, draining, archive maintenance, and resync state.
- `postgres.Store` retains PostgreSQL connection policy, sync coordination,
  schema/search-path setup, and PostgreSQL search capabilities.
- `duckdb.Store` retains mirror file identity, replacement watching, local or
  Quack transport, and DuckDB search capabilities.

These wrappers do not implement duplicate common store queries. They supply a
backend adapter to the common store and expose only responsibilities that are
genuinely backend-specific.

### Backend Adapter

The backend adapter provides:

- engine identity and Bun dialect;
- guarded access to the current Bun read and write handles;
- read-only/write capability policy;
- transaction entry points;
- engine-specific search capabilities; and
- close, reopen, and replacement lifecycle behavior.

Shared methods execute inside adapter callbacks. This prevents a query from
retaining a SQLite or DuckDB handle after the wrapper has swapped or retired it.
PostgreSQL uses the same interface with a stable pool.

### DuckDB Bun Dialect

DuckDB receives a first-class internal implementation of Bun's `schema.Dialect`
contract based on `schema.BaseDialect`. It must not identify itself as SQLite or
inherit SQLite-only sequence/type behavior.

The dialect owns:

- the exact Bun feature flags supported by the pinned DuckDB driver;
- identifier quoting;
- string, JSON, byte, boolean, and UTC timestamp literals;
- Go-to-DuckDB type mapping;
- primary-key and generated-ID DDL behavior;
- default schema/catalog behavior; and
- table metadata normalization required by canonical schema generation.

Focused execution tests against a real temporary DuckDB database determine the
feature set. Unsupported operations fail explicitly rather than falling back to
SQLite behavior.

## Canonical Schema

One Bun model registry defines every durable entity shared between the serving
backends, including sessions, messages, usage events, tool data, pricing,
project identity, worktree mappings, curation, insights, and the related join or
metadata tables needed to serve those features.

DuckDB receives the complete common column set even when a mirror does not
populate a feature. This keeps common queries and scans structurally identical.

Physical representations may differ only where the engines require equivalent
syntax or affinity:

- SQLite stores booleans with integer affinity; PostgreSQL and DuckDB use native
  booleans.
- Identity/sequence syntax and integer widths follow the engine while preserving
  the same logical key.
- SQLite may retain text timestamp affinity in shipped tables; PostgreSQL and
  DuckDB use native timestamp types. Canonical scanners normalize all values
  to UTC domain values.
- JSON uses a portable representation unless a narrowly scoped engine feature
  requires a native type.

The following remain documented extensions rather than canonical data tables:

- SQLite archive, parser, skip, watcher, and resync bookkeeping;
- PostgreSQL push/source bookkeeping;
- DuckDB `sync_metadata` and mirror provenance;
- FTS tables, generated search columns, and search indexes; and
- vector generation tables and vector indexes.

The common table definitions, columns, relationships, and ordinary-index
semantics do not otherwise fork by engine. DuckDB is the narrow constraint
syntax exception: it enforces canonical foreign keys but rejects cascading
actions, so its read-only mirror writer keeps its existing explicit child-first
deletion order. SQLite and PostgreSQL generate `ON DELETE CASCADE` from the same
relationship metadata.

The convergence uses this ownership matrix:

| Area                                   | Canonical serving representation                                                                                 | Adapter-owned extension                                                                                   |
| -------------------------------------- | ---------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| Sessions                               | Domain fields plus `source_archive_id` and `source_database_generation` provenance                               | SQLite parser cursors/linearity, PostgreSQL owner and remote-curation baselines, DuckDB push fingerprints |
| Messages and dependent rows            | `(session_id, ordinal)` is the logical message key; source row IDs remain data columns where present             | FTS/vector derived rows and generation state                                                              |
| Identity and mappings                  | `source_archives` and the three `source_*` identity/mapping tables, including source archive and generation keys | SQLite change journals and publication revisions; PostgreSQL publication-scope ownership tables           |
| Usage, pricing, curation, and insights | One common column set and logical key per registered Bun model                                                   | Provider import cursors and backend capability probes                                                     |

Canonical generated message DDL uses `(session_id, ordinal)` as its composite
primary key and keeps `id` as an optional source row identifier. The shipped
SQLite archive is the one physical compatibility alias: it retains
`id INTEGER PRIMARY KEY` and its existing unique `(session_id, ordinal)`
constraint. Schema validation accepts that pair as logically equivalent and
never rebuilds the persistent table. PostgreSQL already uses the composite key;
DuckDB adopts it on the version-10 rebuild. No common query or relationship may
depend on SQLite rowid identity.

The same rule applies to dependent rows. Canonical tool calls use
`(session_id, message_ordinal, call_index)`, tool-result events extend that key
with `event_index`, and pins relate to messages through `(session_id, ordinal)`.
The shipped SQLite `tool_calls.message_id` and `pinned_messages.message_id`
columns remain non-null physical aliases because removing them would require
destructive table rebuilds. The SQLite adapter resolves those aliases from the
inserted message inside the same transaction; shared queries and every other
backend use only the canonical ordinal keys. Migration tests retain existing
tool calls and pins and prove new writes satisfy both representations.

Canonical foreign-key metadata preserves session/message deletion behavior for
fresh schemas. Canonical usage dedup indexes use portable `CASE` expressions so
empty keys may repeat while non-empty keys remain unique on all engines; shipped
SQLite/PostgreSQL partial indexes are accepted as physically equivalent.
Mirror-only source IDs, including cursor-usage and secret-finding IDs, are
nullable data rather than generated canonical keys. Conversion of a non-empty
timestamp that is not one of the proven persistent forms returns an error and
aborts the enclosing write instead of silently producing `NULL` or a zero time.

Existing SQLite `project_identity_observations`,
`session_project_identity_snapshots`, and `worktree_project_mappings` are
one-time migration inputs. The convergence transaction backfills the canonical
`source_*` tables with the archive ID/generation before shared reads switch; the
same cutover redirects subsequent writes, so the old tables are not a runtime
fallback or dual-write path. PostgreSQL and DuckDB already use the canonical
source-scoped identity shape.

`sessions.source_archive_id` and `sessions.source_database_generation` are
required canonical provenance. The SQLite adapter reads the stable archive ID
and database ID under its guarded handle and stamps every session write before
the first shared-query cutover. This stamping lands with schema convergence, not
the later canonical-write rewrite, so Tasks 5-9 cannot create sessions with
empty provenance. The convergence migration backfills both columns on legacy
sessions in the same transaction as the source-scoped identity tables; empty
values fail compatibility validation after cutover.

## Schema Creation and Migration

Canonical Bun models generate fresh-database DDL and provide the expected schema
used by compatibility checks. Bun automatic reset or destructive schema diffing
is not used on persistent databases.

Existing shipped migrations remain immutable. This change adds one forward
schema-convergence migration and follows these rules:

- SQLite receives only additive or otherwise non-destructive changes. The
  archive is never dropped, truncated, or recreated, and existing sessions are
  preserved.
- PostgreSQL applies the corresponding changes transactionally in place and
  retains existing synchronized data.
- The migration leaves no permanent dual-schema read or write path. Once the
  transaction succeeds, only the canonical model is used.
- DuckDB does not migrate in place. Its `SchemaVersion` is incremented; a push
  builds a new canonical mirror, validates it, and swaps it atomically after
  confirming the target is an AgentsView mirror.

Where an existing physical type is a valid engine-specific representation of the
canonical logical type, compatibility validation treats it as an explicit alias
rather than rewriting stored data without benefit.

## Query and Write Flow

Each backend opener configures its native `database/sql` pool and wraps the
current handle with Bun. Direct `database/sql` use is limited to opening,
driver-specific connection setup, pool configuration, handle draining, and
handle replacement. Application queries, schema operations, and transactions
flow through Bun.

Common store methods use Bun models and query builders. Complex CTEs and
aggregates may use parameterized Bun raw fragments, but query composition,
literal formatting, execution, transactions, and model scanning remain under
Bun.

Those fragments use only the portable SQL subset exercised by all three
backends. UTC parsing, calendar bucketing, percentiles, regex normalization, and
JSON interpretation move to shared Go reducers whenever the engines do not share
semantics. Non-search methods do not gain a backend expression switch; an
operation that cannot be expressed portably requires a design update.

Parser ingestion writes canonical rows through the SQLite adapter. PostgreSQL
push and DuckDB mirror population consume the same row models and common batch
helpers. The PostgreSQL adapter supplies the narrow replication conflict policy
that protects `owner_marker`, exclusions, aliases, and target-owned rename/trash
baselines; these operational fields never participate in generic replacement.
DuckDB keeps fingerprint gating and replaces a whole session in one transaction:
it deletes every dependent message, usage, tool, finding, and curation row
before inserting the canonical replacement, and propagates hard deletes before
advancing mirror metadata.

Quack uses the SQL generated by the DuckDB Bun dialect. The DuckDB adapter sends
that SQL through the attached catalog's `query()` transport and passes returned
rows to Bun's model scanner. Quack transport details do not leak into shared
store methods.

## Full-Text and Vector Capabilities

Full-text and vector search are the allowed backend-specific query areas.
Capabilities accept canonical filters and return canonical identifiers,
ordinals, stable tool/result coordinates, unit ranges, subordinate state,
scores, or ranks. The common store resolves lexical message hits to semantic
units, hydrates results, and owns final ordering.

SQLite may continue to use FTS5 and sqlite-vec, PostgreSQL may use its native
full-text facilities and pgvector, and DuckDB may use its supported regex/FTS
and vector facilities. These implementations may differ in syntax and index
maintenance, but their observable filters, result identity, and ordering remain
aligned as closely as engine semantics permit.

Engine switches, placeholder builders, and duplicate scan functions are not
allowed in common store methods. A difference that cannot fit the small search
capability boundary requires an explicit design update rather than an ad hoc
backend branch.

## Errors and Atomicity

Errors include operation and backend context while preserving established
sentinel behavior, including `db.ErrReadOnly`, `db.ErrSemanticUnavailable`, and
not-found handling based on `sql.ErrNoRows`.

Unsupported dialect features return a direct capability error. The shared store
does not retry a query through a legacy implementation or another dialect.

All multi-row writes and migrations are transactional. A model, extension, or
metadata failure rolls back the transaction. DuckDB replacement remains
validate-then-swap, so a failed build cannot replace a valid mirror. SQLite and
PostgreSQL schema versions advance only in the transaction that installs the
complete migration.

## Testing

Implementation follows vertical test-driven slices: add a failing observable
contract test, implement the common Bun behavior, verify it on the participating
backends, and then remove the superseded backend-specific method.

A reusable store-contract suite seeds hand-written canonical rows and asserts
literal outcomes for filtering, pagination, ordering, analytics, usage,
curation, and mutations. It runs against real temporary SQLite and DuckDB
databases locally and against PostgreSQL under the existing `pgtest` setup.

DuckDB dialect tests cover owned behavior only:

- canonical DDL/type generation;
- literal encoding;
- declared feature support;
- transactions and conflict handling; and
- representative Bun queries executed against a real temporary DuckDB file.

Migration tests cover a fresh database and the previous shipped schema with
existing data. SQLite and PostgreSQL tests assert that data survives and the
canonical schema becomes usable. DuckDB tests assert rebuild, validation, and
atomic replacement rather than in-place migration.

Existing lifecycle tests continue to protect SQLite pool reopening/draining,
DuckDB mirror swaps, and Quack reattachment. FTS/vector parity tests use shared
fixtures and literal expected result identities while allowing capability-
specific setup.

The Quack integration suite also runs representative Bun-generated parameterized
reads and model scans through a real `query()` attachment, including quotes,
binary/JSON values, stale-attachment retry, and credential redaction. A
resolver-only unit test is not sufficient evidence for the remote path.

Performance-sensitive list, search, usage, and analytics paths retain benchmark
coverage. Completion requires focused tests, the full Go suite, PostgreSQL
integration tests, DuckDB-tagged tests, formatting, vetting, and the
repository's backend benchmark gate where supported.

Deletion of the old query paths is verified by the compiler, focused source
searches during handoff, and passing replacement behavior tests. No test asserts
that deleted files or symbols remain absent.

## Cutover and Completion Criteria

The cutover is atomic at the code level. There is no runtime flag, compatibility
wrapper, or dual old/new store implementation.

The refactor is complete when:

1. All common `db.Store` behavior is implemented once through Bun.
1. SQLite, PostgreSQL, and DuckDB use the canonical model registry.
1. DuckDB uses the dedicated Bun dialect for local and Quack queries.
1. Existing SQLite and PostgreSQL data upgrades in place.
1. DuckDB mirrors rebuild and swap under the new schema version.
1. Backend-specific code is limited to lifecycle, operational metadata, sync
   transport, and small FTS/vector capabilities.
1. Duplicate PostgreSQL and DuckDB common query/scanning implementations are
   removed.
1. Contract, migration, lifecycle, integration, and performance checks pass.

## Non-goals

- Making DuckDB a system of record or accepting remote DuckDB writes.
- Changing SQLite, PostgreSQL, or DuckDB configuration and CLI behavior.
- Making all three backends equally writable.
- Replacing the underlying database drivers.
- Adding a permanent compatibility adapter for the old store implementations.
- Unifying engine internals that do not represent shared durable domain data.
- Moving SQLite Recall extraction, evidence reconciliation, FTS, or vector
  generation into the common schema. Common Store entry points use the SQLite
  Bun handle when Recall is available; other adapters reject them before SQL.
