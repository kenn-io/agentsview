# Storage Rules

Read this file before changing SQLite, PostgreSQL, CockroachDB, DuckDB,
ClickHouse, archive resync, or storage queries.

## SQLite Archive

SQLite is the persistent archive. Never delete, drop, truncate, or recreate it
to handle a data-version change.

Use non-destructive schema migrations such as `ALTER TABLE` and `UPDATE`.

Dashboard analytics, report, and search scans, and other multi-second scans over
message or usage rows, take the lane; the list and sidebar reads every page
loads (session list, stats, project list) and reads scoped to one session stay
outside. The lane uses `acquireBulkRead`, using at most `readerMaxOpenConns - 1`
reader connections so quick reads always get one. Each holder uses one reader
connection at a time. Validate input before acquiring a permit. Release it
before waiting for another bulk reader or doing host git/GitHub lookups.

A parser upgrade normally requires a full resync. A parser change that needs a
full resync must build a fresh database, sync source files, copy orphaned
sessions from the old database, and swap the files atomically. Preserve sessions
even when their source files no longer exist. Schema repairs also require a full
resync. Archive-only archives use the preservation rebuild, which does not read
source files on the receiving host.

Full parser and archive-only preservation rebuilds serve the old archive while
writes receive a retryable HTTP 503. Scoped upgrades and schema repairs still
block startup. A failed or unlaunchable startup rebuild keeps serving with
writes allowed and retries on the archive audit cadence. Startup uses no
in-process fallback.

A parser upgrade can instead reparse only the affected providers in place.
Declare it in `dataVersionScopes` in `internal/db/data_version_scope.go`:

- Declare a version only when the reparse keeps source and session identities.
  Add a regression test that proves it.
- List every provider whose stored output changes, including each provider that
  shares the changed parser. A provider left out keeps stale output marked as
  current.
- Declare only versions from 131. Released binaries treated earlier versions as
  full rebuilds, and their retry rows cannot be told apart from complete rows.
  `TestValidateDataVersionScopes` enforces these limits.

When a writable open applies a declared upgrade, one transaction stamps
unaffected sessions with the new version and deletes the affected providers'
skip and failure cache entries. Rows of affected providers stay at their old
version, so ordinary sync reparses them and retries failures. Rows already below
the archive version are retry markers and also stay stale. Sessions whose
sources are gone keep their old parser version.

The open also sets the `scoped_upgrade_pending` key in the `stats` table. While
that key is set, startup runs its initial sync and read-only opens report that
the archive needs a resync. A full, authoritative sync pass deletes the key,
even when individual sources failed to parse. Incremental passes, aborted
passes, and passes with provider failures or deferred sources leave it in place.

The replacement archive's writer raises `wal_autocheckpoint` to 128 MiB of pages
for the bulk load; the live writer keeps SQLite's 1,000-page default. The
threshold only triggers a passive checkpoint after a commit, so a large
transaction or a pinned reader can push the WAL past it (249 MiB was observed).
Before closing the replacement, the build runs a checked truncate checkpoint.
The swap installs only the main file, so a failed checkpoint or close aborts it.

The `raw_archive_*` tables record retained captures, root identities, accepted
sources and deletion evidence. Seed import and restore permanently enable
archive-only mode, which disables source sync and publication. These archives
store `1 << 20` plus their parser version in `PRAGMA user_version`, so older
builds refuse them. This build decodes that range and preserves pending
rebuilds; ordinary archives keep their parser version unchanged.

### Artifact checkpoint landings

Land artifact checkpoints only through
`RecordArtifactCheckpointLandingFromStage`. Do not write to or drop
`artifact_checkpoint_landing_sessions`. Nothing uses it, but builds up to v0.44
require it to open an archive read-only.

### Conversation export

Conversation exports consume normalized SQLite message records for every agent.
The database is the system of record: use stored content, roles, system markers,
and source identities. Do not add agent allowlists, export-only parser fields,
or source reparse requirements. Export metadata and message writes commit in the
same transaction. When sanitization or content policies change copied messages,
refresh the export index from the final stored messages while preserving their
message IDs. Unchanged full-content copies retain the copied export index.
Usage-only writes publish a session-level coverage gap even when policy removes
every message.

Message IDs are opaque archive identities, not row IDs, ordinals, timestamps, or
text hashes. Preserve them through verified appends, unchanged complete
reparses, and unambiguous native source IDs, including retained tombstones.
Changed no-ID replacements must report identity ambiguity. Rebuilds retain these
IDs and tombstones but use the new database generation for revisions and
cursors.

An archive is cold until its first conversation export: sync writes no message
projection rows, and the first `export conversations changes` builds the index
from stored messages in one writer transaction, through the daemon when one owns
the archive. From then on the archive is active and message writes maintain the
index in their own transactions. A rebuild copies the source's cold or active
state with the archive identity before any other copy, and copies between
archives in different states fail. Session-level records (policy gaps, deletion,
project changes) stay transactional in both states. Copied orphans and trash use
the same stored records; absent source files do not make their archived text
unavailable.

Archive refresh seeks live conversation rows by `(session_id, ordinal)` through
`idx_conversation_messages_ordinal`, which excludes removed rows. Keep this
index available during orphan and trash copies so each message lookup does not
scan the session's entire retained history. The next writable open builds it
once for existing archives; no parser resync is required.

Keep only digests and compact latest changes, not a body event log; bounded body
reads take text from the archived message at the projected ordinal and reject it
when its digest no longer matches the pinned revision. Project-only changes
publish session invalidations without changing message revisions. Manifest and
bounded body reads resolve project evidence in their own SQLite snapshot; body
reads also pin the database generation and message revision. This local contract
does not widen raw artifacts or mirror schemas.

Visible conversation exports use the existing `messages.dialogue_text` field.
They do not use message search's fallback to rendered content: a NULL projection
is an explicit coverage gap and an empty projection means no dialogue. Keep
selected-text digests and lengths in the conversation index, and publish a new
revision when dialogue alone changes. The writable-open upgrade initializes
those fields from archived messages without reparsing or replacing message IDs.
Copies preserve both projections and apply content policy to both.

Keep a known dialogue projection even when it equals display content; NULL also
represents unclassified messages and cannot carry both meanings. Data version
146 reparses providers with dialogue classification in place, including the
shared Codex parser. Retained sources that cannot be reparsed keep their content
and classification gaps; export itself never reads the original source.

### Claude subagent sources

Joined local subagent writes record contributing transcript paths with the
messages in one SQLite transaction. Refreshes preserve the saved session when a
recorded contributor is missing or no longer a regular file. Rebuilds copy this
provenance with preserved sessions. Once all contributors are readable, a full
parse can apply corrected or shortened transcripts. This metadata stays local to
the archive; S3 materializations do not record temporary paths.

### Claude persisted tool results

Claude Code (and iCodeMate CLI, which shares its transcript layout and parser)
moves a large tool output to a side file in the session's `tool-results` folder
and leaves a placeholder in the transcript. The parser inlines the side file
while it exists, so the first sync archives the full output. Once Claude Code
deletes the side file after its cleanup period, a later full parse sees only the
placeholder. The parser marks exactly the tool_result block that genuinely tried
and failed to resolve a `persistedOutputPath`
(`parser.ParsedToolResult.PersistedOutputUnresolved`; never derived from the
placeholder text's own shape). `ingest.ReconcileProviderHistory`
(`internal/ingest/history.go`) restores each flagged current event from the one
archived event whose `tool_use_id`, source, and timestamp all match — never by
array position, so a dedup collapse or a retry/branch reorder can never pair the
wrong occurrence; no unique match leaves the event untouched, and a later,
unflagged occurrence of the same `tool_use_id` is never overwritten. Restoration
only happens when the archive held real text; that text, its length, and its raw
digest are restored. When the archive held only a length without text (the
`transcripts`/`usage` archive-content policies, or a blocked category), the
event is left alone: "blank content, kept length" already means something else
to every reader of `tool_result_events` (a deduped summary, re-derived from its
one content-bearing event), and restoring a bare length would reproduce that
same shape for an unrelated reason with no way to tell the two apart later.
Accepted limit: once such an event's side file ages out, its archived length
cannot be recovered on a later reparse; the event keeps whatever length its
current placeholder text itself resolves to, not the archived length.

This is the same provider-history dispatcher every other agent's archive
preservation already goes through, gated by `ingest.IsClaudeFormatTranscript`
(the agent and a `.jsonl` path, covering both Claude Code and iCodeMate CLI;
`ingest.IsClaudeFormatAgent` alone would also match iCodeMate's unrelated
VSCode-extension storage path), so hosted Postgres mirrors reach it with no
separate wiring. `internal/sync/engine.go`'s `reconcileProviderHistoryContext`
checks cheaply, over the parsed messages and before any store read, whether
anything is flagged before loading the archived session. Rebuilds read the old
archive, and S3 Claude sessions follow the same rule.

### Codex deletion scope

Before data version 131, deleting a Codex-format thread excluded all its rollout
files under one ID. The upgrade marks those exclusions with
`include_codex_pages`, so pages remain excluded even if their files were absent
during the upgrade and return later. Copies preserve that scope across later
rebuilds. New deletions keep the default per-file scope. Data version 134 moves
pages stored under their bare rollout ID to `<thread>_<rollout>`, using the file
name kept with each deletion when the file is gone. Session and recall imports
use the same exclusion check.

Data version 137 rebuilds the archive so each Codex rollout keeps its first
session metadata ID. During this upgrade, pins, Recall entries and export
message IDs follow a misfiled transcript only when a fresh parse at its exact
stored path proves the corrected thread. Names and stars stay on a surviving
original thread. Missing, moved or compressed files, trashed or deleted misfiled
rows, and chains of misfiled rows follow the existing rebuild behavior. If
another file reparses under the saved ID, its messages replace the old
transcript. PostgreSQL-only pins on a misfiled row's messages are dropped on the
next push.

Legacy trash keeps the same thread-wide scope in
`sessions.trash_includes_codex_pages`. The retained thread row blocks imports of
absent pages even when they return at a different path. Restoring the thread or
one of its pages ends that inherited scope; other existing trash rows remain
trashed. An archived page stored under the old thread ID moves to its page ID
even while trashed; an empty thread row retains the scope. Restoring and
reparsing the original thread therefore cannot replace that saved page.
Permanently deleting the thread transfers the scope to `excluded_sessions` and
removes its covered pages in the same transaction. New trash actions remain
per-file.

PostgreSQL mirrors this scope and keeps a separate source baseline for it, so
restoring a page in PostgreSQL survives later pushes even when its thread row
stays trashed. Restoring a page locally also advances the thread row's sync
marker. A filtered page push synchronizes the owning archive's thread trash
scope even when the thread's project is excluded. It preserves independent
remote trash decisions and does not publish the excluded transcript. PostgreSQL
upgrades existing Codex thread trash and permanent exclusions to thread-wide
scope once; later deletions remain per-file. Pages pushed from the same source
inherit a trashed PostgreSQL thread's scope until a member is restored or
purged. PostgreSQL purge retains thread-wide exclusions for later pushes; the
hosted legacy routes use the same behavior without removing raw projections.
Page publication locks the retained thread row through commit and checks
exclusions after acquiring that lock, so a concurrent purge either removes the
new page or prevents its publication. Thread-wide restore, purge, and new
exclusions use the same owner and agent checks as inherited trash. Old permanent
exclusions that have no recorded owner retain their existing global scope.
Exact-session exclusions remain unchanged. PostgreSQL restore and purge retry
complete transactions on deadlocks or serialization conflicts, including hosted
legacy writes. Retries stop after five attempts or when the request is canceled.

When an old PostgreSQL thread row held a revert page, the next push moves its
remote pins and notes to that page before replacing the original thread. Both
writes share a transaction, including when the page belongs to a later batch.
When the original rollout is absent locally, the push also moves remote names,
stars, and trash state to a newly published page, then removes the duplicate
thread transcript and usage. Existing page curation takes precedence. An empty
thread row retains any inherited trash scope for restore and purge. If the
retained page cannot be published, the push stops and keeps the original row. A
filtered push must include the page's project for this migration. A surviving
original rollout without remote pins can still be pushed when its old page has
been removed or filtered out.

If the original rollout moves outside a filtered push's project scope, the
curation transfer and removal of the old thread row share a transaction. A
failed transfer leaves the thread and its pins intact.

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

## Attached archive source state

Ordinary installations can read fixed Docbank backup snapshots through
`internal/archivesource`. See the
[configuration guide](../configuration.md#history-from-a-saved-archive) for the
user contract and limits. Archive-only installations cannot attach sources.

The compact index under `archive-sources/` is disposable. It contains accepted
source metadata, not messages. Rebuild it from the verified saved application
database. Do not migrate that saved database or adopt its runtime identity. Key
a directory attachment by its archive ID, never by catalog bytes; a changed
catalog re-registers sources under the same attachment and keeps completion.

`archive_attachments` and `archive_attachment_sources` track membership and
per-source completion targets. `archive_source_bindings` binds original
archive/device/root/source identity to an immutable manifest.
`archive_session_provenance` binds each namespaced session to that source.
Publish provenance and content in the same ordinary or staged write transaction;
ordinary ingestion must refuse IDs owned by an attached source. Completion
follows successful parsing and scratch cleanup. Global parser markers do not
clear unfinished attachment work.

Validate ownership of kept and parser-excluded results, including ordinary
remote imports, and run exclusion policies before staging freshness versions or
changing session metadata. A collision in one DAG branch must leave all existing
branches unchanged. Repeat ownership checks in content write and exclusion
transactions.

Restoring an attached session invalidates source completion in every matching
attachment and updates pending counts in the restore transaction. The next drain
reparses the restored row even if an upgrade completed while it was trashed.
Restores take no sync lock, so they write a fresh completion marker, and a
source completes only if the marker it read before parsing is unchanged.

Rebuilds copy registrations and ownership bindings before parsing, and copy
session provenance with retained rows. Available sources run as contributors
against the replacement database. Initially offline or detached sources,
including empty mount directories with no archive layout, retain their previous
content and completion. Corrupt archives and missing pinned snapshots in
available repositories fail preparation. After a contributor starts, a failed
read discards the replacement; only repository-writer contention waits and
retries. Keep explicit column lists when carrying these tables forward.

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

The daemon may retain recently served canonical image bytes in a bounded
in-process cache for up to seven days from generation. Entries can leave sooner
when the cache reaches its entry or byte limit, and process exit clears them.
`{dataDir}/assets` remains the durable image store and backup target. Cache
eviction never changes it, and the browser's own cache policy is separate.

## Backend Roles and Contract

Adding a remote database means implementing Go interfaces, not copying the
PostgreSQL command, config, push, and serve code. `internal/storage` names the
three roles and holds the contract; `internal/backendcontract` asserts every
backend at compile time, so a missing method fails `go build`.

| Role    | Today                  | Contract                                   |
| ------- | ---------------------- | ------------------------------------------ |
| Archive | SQLite `*db.DB`        | `db.Store`; the only writable ingest store |
| Replica | PostgreSQL, ClickHouse | `db.Store` plus `storage.Replica`          |
| Mirror  | DuckDB                 | `db.Store` plus `storage.Mirror`           |

`internal/db` owns shared result processing: `NormalizeSessionLimit` and
`BuildSessionPage` for pagination, `BuildHeatmapResponse` for daily levels,
`TrendAccumulator` for term date filtering, buckets, and counts, and
`MessageScope` with `AnalyticsFilter.MessageScopeFilter` for model-scoped
projections. Backends retain their queries, cursor codecs, and timestamp
scanning.

A replica is a remote database the archive pushes into and that serves the web
UI read-only. A replica may keep its push cursor in the archive sync state
(PostgreSQL) or in its own metadata (ClickHouse); the contract does not care. A
mirror is a disposable local derived file. A new remote SQL backend is a
replica. Do not model it on DuckDB, and do not add a fourth role.

Common pure storage helpers live in `internal/db`; every backend calls that
owner. `BuildAnalyticsWhere`, `BuildUsageSourceFilter`,
`BuildUsageSessionFilter`, and `BuildRecentEditsQuery` own shared reporting SQL.
Helpers with different timestamp parsing, UTC padding, or output formats stay in
their backend.

### How to add a replica backend

1. Create `internal/<name>` with a `Store` that implements `db.Store` with
   `ReadOnly() bool` returning true, a `Sync` (or similar) that implements
   `storage.Pusher`, and a `Backend` struct that implements `storage.Replica`.
   `ValidateTarget` runs before any local work; put connection rules the
   backend enforces up front there (ClickHouse rejects plaintext remote URLs
   without `allow_insecure`), and return nil when there are none. Use
   `internal/postgres/backend.go` and `internal/clickhouse/backend.go` as the
   two worked examples. The push returns `storage.PushResult` and reports
   progress as `storage.PushProgress`; a backend without a vector phase sets
   `Vectors.Skipped`. Route catalog reads through `readbase.NewCatalog` with
   an adapter implementing every `readbase.CatalogBackend` method; route
   DuckDB- or ClickHouse-style analytics through `readbase.NewAnalytics`.
   Route mirror session and transcript reads through `readbase.NewSessions`,
   with every `readbase.SessionsBackend` SQL method explicitly forwarding to
   shared `readbase.SessionQueries` builders or supplying specialized SQL.
   Embed `readbase.MirrorStubs` in mirror stores; PostgreSQL uses
   `readbase.ReadOnlyStubs`. Keep typed loaders in the backend.
1. Add the config section and its resolvers in `internal/config` the way
   `[pg]`/`[pg.NAME]` and `[clickhouse]` work: a struct, `Resolve<Name>`,
   `Resolve<Name>Target`, and `<Name>TargetNames`. `Backend.Targets` and
   `Backend.ResolveTarget` map them onto `storage.ReplicaTargetRef` and
   `storage.ConfiguredReplica`.
1. Register the backend in `cmd/agentsview/backends.go` (`replicaBackends`) and
   add the compile-time assertions in `internal/backendcontract/contract.go`.
1. Add the CLI verb in `cmd/agentsview/cli.go` with
   `newReplicaCommand(<name>.Backend{}, extra...)`. That gives `<name> push`,
   `<name> push --watch`, `<name> status`, and `<name> serve` from
   `replica.go` and `replica_watch.go` with no new command code. Backend-only
   verbs (like `pg vectors`) are the `extra` commands. A background service
   needs a `serviceKind` entry in `pg_service_manager.go`.
1. Regenerate the OpenAPI document and clients with
   `cd frontend && npm run generate:api`, then add the backend's generated
   daemon operation to `replicaPushOperations` in
   `cmd/agentsview/daemon_push.go`. The daemon route `/api/v1/push/<name>`
   comes from the registry; `internal/server` needs no change.
   `TestReplicaBackendsHaveDaemonPushOperations` fails until the entry exists.
1. Add tests: a `Backend` unit test for target mapping (no database), the
   backend's own tagged integration tests, and an entry in the classifier
   wiring guard (`classifier_wiring_test.go`) if the backend opens stores
   through a variable not named `backend`.

### Vector search on a replica

Semantic and hybrid search on a replica has two seams in `internal/storage`
(`vector_search.go`), both optional:

- `storage.VectorSearchProvider` on the `Backend`: `VectorGenerations` lists the
  embedding generations the store holds, and `OpenVectorSearcher` returns a
  `db.VectorSearcher` over the one matching the local config fingerprint, or a
  reason when none is ready.
- `storage.VectorSearchStore` on the `Store`: `SetVectorSearcher` and
  `SetSemanticUnavailableReason`, which the store's semantic and hybrid
  `SearchContent` paths read.

The push side needs no new interface: `storage.PusherOptions.VectorSource` is
non-nil when the archive has an active generation and the target accepts vectors
(`push_vectors`), and the pusher replicates what the export hands it.

`cmd/agentsview/replica_vector_search.go` is the one serve-side gate. It runs
for every replica in `prepareReplicaServeImpl` and on the CLI direct-read path,
handles usage-only archives, `[vector]` disabled, fingerprint lookup, encoder
construction, and the miss notice, then installs the searcher. A replica that
implements neither interface gets a plain unsupported reason. PostgreSQL
(`internal/postgres/backend.go`, `vector_search.go`) and ClickHouse
(`internal/clickhouse/backend.go`, `vector_search.go`, `vector_push.go`) are the
two implementations; `internal/backendcontract` asserts both.

What a new backend does not touch: `internal/server` HTTP handlers,
`archive_write_backend.go`, `replica.go`, `replica_watch.go`,
`replica_vector_search.go`, or the PostgreSQL and DuckDB packages. `pg serve`
extras (raw-upload ingestion) live in `cmd/agentsview/pg.go` behind the optional
`replicaServeExtras` interface; a backend with no extras implements nothing.

Known limits: `pg vectors`, the CLI direct-read transport that selects
PostgreSQL, and `clearPGClassifierHash` remain PostgreSQL-specific. The daemon
push request carries a backend-neutral `replica` target, so a CLI and daemon
must run the same `server.APIVersion`, which the CLI already enforces. In that
target `push_vectors` is optional: omitted means the vector phase runs, the same
default as the `[pg]` config key, and an explicit `false` opts out.

## Backend Parity

Reporting project-label keys are not repository identities. The reporting
catalog must resolve every contributing session from the same read transaction;
aggregate catalogs that ignore unknown observations cannot supply that evidence.
Keep identity-only corrections in the reporting digest. The wire contract is in
[reporting exports](../reporting-export.md#project-identity-evidence).

- Keep observable behavior and query shape aligned between SQLite and
  PostgreSQL/CockroachDB, DuckDB and ClickHouse. Match queries, indexes,
  aggregations, filters and ordering unless a documented SQL, storage layout
  or disposable mirror constraint requires a difference.
- Do not fix correctness or performance in only one backend unless the user
  limits the task to that backend. If implementations must differ, explain why
  and preserve the same behavior.
- PostgreSQL, DuckDB and ClickHouse share catalog orchestration in
  `internal/readbase.Catalog`. Shared query builders own SQL that differs only
  in syntax. Each backend explicitly supplies every required SQL operation and
  its typed timestamp, observation and snapshot loaders.
- DuckDB and ClickHouse share report orchestration in
  `internal/readbase.Analytics`. Backends supply every required SQL operation
  and retain typed loaders and caches.
- DuckDB and ClickHouse share session and transcript reads in
  `internal/readbase.Sessions`, with required backend SQL and typed scanners.
  `readbase.ReadOnlyStubs` owns unsupported archive writes; `MirrorStubs` adds
  mirror curation and insight stubs. `HostedStore` keeps explicit forwards.
- All four backends share activity report assembly and session usage selection
  in `internal/activity`. Explicit inputs supply usage, candidate streams,
  message counts and project maps; SQL, pricing, caches and read lifetimes
  stay in each backend. Daily usage query strategies and aggregation loops
  remain per backend.

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
change. Catalog and user-pricing changes are covered per session instead: each
rollup install records the distinct
`(provider, reported model, canonical model)` lookups its daily rows used, and a
read re-resolves only those against the current catalog. A price change
therefore rebuilds just the sessions whose lookups resolve differently, while
`updated_at`-only refreshes rebuild nothing. Do not add a write-only
extractor-version metadata key.

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
survive gaining a sibling. The daemon requeues invalidated sessions after commit
and rebuilds their rollups; CLI writes leave rebuilding to the request.

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
candidate session's facts fingerprint, exact baked metadata, per-session pricing
identity, resolved rate hashes, and Cursor high-water mark. A result is no older
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

SQLite keeps paired candidates in memory per session and range start, with
separate limits of 200,000 candidates and 4,096 entries. A full cache measured
64.3 MiB of resident Go heap on Windows amd64, using the post-GC
`runtime.MemStats.HeapInuse` delta for 200,000 candidates across 4,096 entries,
with 36-byte session IDs, 24-byte models, and append-grown slices. Empty
sessions use only the entry budget. A week report then a month report keep
separate entries for their range starts. Entries run to
`max(EffectiveEnd, RangeEnd)` and remain valid while the transcript revision and
session creation time match. Reader swaps clear the entries and reject stores
from earlier generations. Reports exceeding the entry limit reuse retained hits
and store up to the cap. Reports exceeding the candidate budget skip storing new
entries. Warm reads still load session revisions and merge the per-session
candidates in SQLite's microsecond order.

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

Discovery collects distinct session IDs per bounded timestamp range before
checking `sessions`, so each session is looked up once per range.

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

### Sidebar tree index

The sidebar's recursive child lookup and parent-only lookups share
`idx_sessions_parent` on `(parent_session_id, is_automated)`, excluding null
parents. Writable opens widen the parent-only form that `schema.sql` creates,
after column migrations, and startup `PRAGMA optimize` gathers its statistics.
Bulk imports retain the index, and widening it needs no parser resync. Read-only
opens use whichever form the archive has until a writable open widens it.

PostgreSQL keeps `idx_sessions_parent` on `parent_session_id` alone. The wider
SQLite index exists because, without it, SQLite builds a temporary
`is_automated` index on every recursive step of the child lookup. PostgreSQL
never builds temporary indexes, so it filters `is_automated` while it uses the
parent-only index.

### Dashboard metadata index

SQLite's summary and tool analytics can read model names and message timestamps
through `idx_messages_analytics_metadata` on
`messages(session_id, id, timestamp, model, ordinal)` without loading message
body pages. The next writable open builds or upgrades this index and logs that
startup is waiting for the message scan. Existing rows are preserved; no parser
resync is required.

Writable opens and completed bulk index builds run `PRAGMA optimize = 0x10012`.
This considers all archive tables and limits the statistics samples. Statistics
must cover competing indexes too: analyzing only the metadata index can leave
SQLite choosing an older index that still reads message bodies. Full resync
defers this index during import and rebuilds it before the replacement archive
is installed. Read-only opens do not create indexes or collect statistics.

### Transcript usage identity

Token usage, Claude message/request identities, and source UUID participate in
transcript revision equality. Finalizing a streamed message can therefore bump
`transcript_revision` and `local_modified_at`, invalidate secret-scan freshness,
mark the session updated for read-progress/UI purposes, and enqueue the normal
artifact, recall, PostgreSQL, and DuckDB refreshes. Full resync reconciliation
must compare the same fields so incremental and resync paths agree. A no-op
message replacement preserves existing secret findings; changed transcript
content clears them for a fresh scan.

### PostgreSQL usage comparisons

PostgreSQL stores usage timestamps in UTC with microsecond precision, while
SQLite keeps the timestamp text that the parser produced. Before PostgreSQL push
compares usage events, it converts local timestamps to the same UTC microsecond
form, as it already does for messages and tool results. Both sides then sort the
encoded usage records, so the comparison does not depend on timestamp text or
row IDs. The SQLite archive and the usage cache keep the original text and
order.

A metadata-only push therefore leaves unchanged messages, tool calls and usage
events in place. When the transcript matches, accounting changes replace only
usage events. A changed transcript replaces messages and their tool calls and
results from the first changed message. A malformed local timestamp keeps its
original text in the comparison, so it never matches PostgreSQL and the push
reports the session as an error.

### Tool result summaries

`tool_calls.result_content` is a display summary derived from the call's
`tool_result_events` rows at sync time. When exactly one of a call's events has
content and the summary equals that content, the summary is not stored: the
column is empty while `result_content_length` still records the summary's size.
Empty events, such as timing-only `tool_execution` marks, don't count. That
pair, an empty column with a non-zero length, tells a reader to take the text
from the one event with content. Summaries over several content-bearing events,
summaries that differ from their event, calls with no content-bearing event, and
blocked categories store exactly what the parser produced. Load tool calls
through the message loaders, which refill the summary once events are attached;
a query that selects the column directly must apply the same fallback, and
PostgreSQL and DuckDB apply the same write rule so their tool-call fingerprints
match SQLite. Anyone reading the archive or a mirror by hand sees the empty
column and must join the events table to recover the text.

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

## ClickHouse Replica

ClickHouse is a replica in the `storage.Replica` sense: the archive pushes into
it and `clickhouse serve` reads from it. Its push cursor lives in the mirror's
own metadata, which is why the docs below call the database a mirror.

- SQLite is the archive. `clickhouse push` writes ClickHouse. `clickhouse serve`
  queries ClickHouse for the HTTP API and UI. Vectors ride the same push:
  `vector_generations`, `vector_documents`, `vector_chunks`, and
  `vector_push_state` are ReplacingMergeTree tables keyed by config
  fingerprint and source archive, and the searcher ranks chunks with an exact
  `cosineDistance` scan (no vector similarity index yet; the query already has
  the `ORDER BY distance LIMIT` shape that index accelerates). Dashboard
  writes (rename, trash, insights, stars, pins) return `db.ErrReadOnly` and
  stay on SQLite. Never delete, drop, truncate, or recreate SQLite to handle a
  ClickHouse schema or data-version change. Design decisions live in
  [ClickHouse push and serve](../internal/clickhouse-mirror.md).
- Keep push order per batch: insert dependents, then
  `DELETE ... WHERE session_id IN (...) AND push_version < v`, then session
  rows. Store every ClickHouse push cursor and version in the mirror's
  `sync_metadata`. Never store ClickHouse sync state in SQLite.
- Every mirrored table is `ReplacingMergeTree(push_version)`. Every connection
  sets `final = 1`. Do not write `FINAL` in query text. `OpenForAdmin`
  bootstraps through the server `default` database; do not ping a DSN-path
  database that does not exist yet.
- Derived tables (`usage_messages`, `terminal_event_snapshots`) are
  insert-maintained by materialized views and keyed by
  `ReplacingMergeTree(revision)`, where live rows use an odd revision above
  the backfill's even one. Readers check them against `sessions.push_version`:
  `usage_messages` rows count at or above it, because messages land before the
  session row is published, and `terminal_event_snapshots` rows match it
  exactly. A materialized view runs inside the insert and reads joined tables
  in full, so filter a joined table to the inserted block's sessions. Views
  never see deletes; `deleteMirrorSessions` clears the derived tables. Startup
  backfills record completion in `sync_metadata` only after finishing, never
  modify source tables, and must stay safe to repeat. See the
  [Activity report reads](../internal/clickhouse-mirror.md#activity-report-reads)
  design section.
- Activity report queries receive the selected session IDs as the
  `activity_candidate_ids` external table on the request context. Do not
  rebuild candidate discovery inside later queries, and never materialize a
  recursive CTE.
- Design ClickHouse SQL for MergeTree. Do not paste PostgreSQL or DuckDB queries
  unchanged. Orphan filters must treat `parent_session_id IS NULL` as an
  orphan (`NULL NOT IN (...)` is unknown).
- clickhouse-go inlines every bound argument into the statement text, and the
  server rejects statements over `max_query_size` (256 KiB by default). Never
  build an `IN (...)` list from a set the database already selected, such as
  the sessions matching a filter or the Claude snapshot keys of those
  sessions. Embed the selecting predicate as a subquery (`chSessionSet`) or
  derive the keys in a CTE instead. Chunked lists (`db.QueryChunkedSize`) are
  for sets that arrive from outside the database, and they bound entry count,
  not bytes.
- Tests use the `chtest` build tag. Run `make test-clickhouse` against a
  dedicated test server (`TEST_CLICKHOUSE_URL` or the compose service). Do not
  point those tests at a live mirror.

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
