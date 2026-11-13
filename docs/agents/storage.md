# Storage Rules

Read this file before changing SQLite, PostgreSQL, CockroachDB, DuckDB,
ClickHouse, archive resync, or storage queries.

## SQLite Archive

SQLite is the persistent archive. Never delete, drop, truncate, or recreate it
to handle a data-version change.

Use non-destructive schema migrations such as `ALTER TABLE` and `UPDATE`.

Dashboard analytics, report, and search scans, change stream reads, and other
multi-second scans over message or usage rows, take the lane; the list and
sidebar reads every page loads (session list, stats, project list) and reads
scoped to one session stay outside. The lane uses `acquireBulkRead`, using at
most `readerMaxOpenConns - 1` reader connections so quick reads always get one.
Each holder uses one reader connection at a time. Validate input before
acquiring a permit. Release it before waiting for another bulk reader or doing
host git/GitHub lookups.

The archive writer and its serving reader pool cache up to 64 prepared SQL
statements per connection. This avoids compiling repeated import, project
attribution, session, and transcript queries. The cache stays in memory and
closes with its connection. The serving pool retains at most four connections.
Cold read-only opens and standalone schema probes use their existing uncached
connections.

Reconciliation spools, Codex staging databases, and temporary parser indexes for
discovery and reconciliation also cache up to 64 prepared statements per
connection. Parser indexes use one connection. Writable vector indexes and
raw-sync checkpoints use the same limit with the cgo driver; the modernc driver
keeps its existing configuration. Checkpoint caching applies to writable opens.
These caches close with their connections and do not change the stored data.

The signature replay store caches up to 64 prepared statements on its single
writable connection. Its read-only identity probe uses an uncached connection.
Replay admission keeps its immediate transactions, rollback journal, and EXTRA
synchronous setting.

The startup worker snapshots freshness for digest-capable providers while it
holds the archive write lock, before the daemon opens the archive; later passes
query live state.

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

The parsed-output digest records each provider and corpus input under
`internal/parser/testdata/data-version-output-digests`. If output changes for a
recorded input, take a new data version and record its digest with `bump` set
to true. Keep records already on main. For a new input, append its first digest
at the current version using output from the current main parser, and leave
`bump` unset. This also applies when the input accompanies a parser change.
A new provider can append its first digest at the current version. If records
conflict, keep the record from main, remove your unmerged conflicting record,
and take the next version.

The digest covers only inputs in `internal/parser/testdata/data-version-corpus`.
Parser behavior outside that corpus is unguarded. Add a representative input
for each new stored-output behavior.

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

Codex forks keep accepting available messages when their replay parent is
unavailable. Each full fork write records parent turn membership and per-message
source origins in `codex_fork_state`, in the same transaction as the transcript
and usage. Rebuilds copy this provenance with retained orphan sessions. Forks
use full parses so optional parser checkpoints cannot leave provenance behind.

A fork whose parent was read previously uses that saved evidence to remove the
inherited prefix and keep child usage accurate. Without that evidence, the
fallback transcript stays visible and may include copied parent messages. Sync
warns about this uncertainty and withholds unverified token usage. The original
token metadata stays in local provenance for later reconciliation. Saved
messages and tool results absent from a shortened source remain archived.
Retained historical usage events also survive.

Reconciliation matches storage-normalized message content and consumes each
occurrence once, including when a TraeX record emits several messages. Restored
tool results obey the current category and archive-content policies. Retained
messages keep their activity bounds and supply the child preview; retained usage
events follow the final child identity and message ordinals. Restored metadata
continues to survive full parses until it returns in the source itself. Codex
forks with archived tool calls use staging even when the source shrinks below
the size threshold. They load message and call metadata separately, then
reconcile and spool result bodies one archived message at a time. Restored
source events keep their order around shared events. Raw summary participation
remains distinct from sanitized text, so a cleared result cannot revive older
output. Spawn links also survive when their output records disappear.
Summary-only saved results keep their text and lengths when the source still
contains the call but omits its output. Blocked results retain their permitted
length metadata across repeated staged writes, including imported events whose
raw summary participation is unknown. Local provenance records imported events
observed contributing readable text, so an update from one agent retains other
agents' length contributions. Each hint includes its archived event position,
agent, source, status, timestamp, and length. Artifact message replacements
clear these hints in the same transaction, since even unchanged event metadata
cannot prove that replacement content contributes. Other fork provenance
survives. Raw nullable metadata stays unchanged. Blank unknown content
establishes no participation. Archived calls without provider IDs use occurrence
keys to publish their staged summaries, events, findings, and failure signals.
Archive copies retain existing raw result digests and participation flags;
absent fields remain unknown. Returning native output can therefore match its
saved event after a preservation rebuild.

Each saved message keeps its original ordered turn-context prefix, including
turns that emitted no message. Prefixes share storage across snapshots and only
referenced prefixes survive a write. Replay classification uses that source
evidence rather than merged archive order. Pre-turn replay leaves the transcript
and preview when the parent returns. Known source occurrences claim their
matches before legacy rows. A matching shortened snapshot keeps the saved
context when it contains every incoming turn in order; changed contexts use the
current source. Retained context evidence keeps history preservation enabled
across resolved refreshes until the source supplies it again. One reconciliation
shares context-containment results for common prefixes. Content matches also
preserve whether a record appeared before or inside a turn. A missing turn ID
alone cannot identify pre-turn replay as an idless child. Saved prefixes claim
matching content before less specific occurrence matches, so identical idless
parent and child messages keep their separate usage. A returning longer prefix
must keep the nearest named turn to claim saved usage. A different boundary
keeps the occurrences separate until ownership can be established; identical
content and timestamps alone cannot transfer usage. When fewer identical legacy
occurrences return, their ownership remains unknown: keep those rows and their
uncounted usage separately instead of assigning parent usage to a child.
Repeating the same source does not add more occurrences.

Independently imported usage events survive resolved refreshes and rebuilds.
Their non-summary usage adds to message totals, and context uses the highest
measurement from either source. Retained history keeps chronological activity
bounds even when its only remaining rows are usage events. Ordinal-only usage
reconciliation reads message metadata without result bodies. Matched usage
references do not require archived result payloads. If a source omits a
referenced message, retain that message and load its results one message at a
time. Restored usage also follows source entries emitted before their archived
counterpart is reconciled.

Archived result events without recorded raw digests remain separate occurrences.
Sanitized or blocked display text cannot establish provider identity, including
during image projection. Returning source events with real digests still
deduplicate across retries; unknown historical occurrences remain alongside them
when their identity cannot be proved.

If the parent never appears, available messages continue updating across
retries, restarts, and rebuilds. Usage for unverified messages remains
unavailable. If the parent later appears, reconciliation removes only confirmed
inherited turns and restores child usage, including retained child messages
absent from the current source. Rows without source evidence stay preserved,
with unverifiable usage withheld until returning source records establish its
ownership. The stale parser version continues to request a parent lookup until
the source can be parsed with its parent. A successful rebuild still installs
while those retries remain queued.

When an Antigravity CLI source yields a retryable low-fidelity transcript, sync
and rebuild preserve any saved history and leave it stale for retry. A new
session can use the fallback, but later fallback changes are held until a
high-fidelity parse succeeds. A rebuild still installs, even when these
preserved sessions are its only work. The sync still reports them as failures,
so the request returns an error and attached archive sources do not drain until
a high-fidelity parse succeeds.

A daemon startup whose only unfinished work is queued fork retries still counts
as complete for draining attached archive sources.

The `raw_archive_*` tables record retained captures, root identities, accepted
sources and deletion evidence. Seed import and restore permanently enable
archive-only mode, which disables source sync and publication. These archives
store `1 << 20` plus their parser version in `PRAGMA user_version`, so older
builds refuse them. This build decodes that range and preserves pending
rebuilds; ordinary archives keep their parser version unchanged. Data version
173 requires no source reparse; it keeps older binaries from writing archives
whose image-text index they cannot maintain.

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

Each live projection row also records a fingerprint of its message's own usage
columns (`usage_key`) and the `conversation_usage_revision` value of its last
usage-only change (`usage_revision`). Triggers on `messages` advance that
counter only when the fingerprint changes, so a full rewrite that keeps usage
publishes nothing; triggers on `usage_events` mark the message at
`message_ordinal`. A projection row that is created or moves takes its
fingerprint without a revision, because it already publishes a conversation
revision. The change stream re-sends marked messages; the conversation export
ignores these columns. The writable open that adds the columns records every
live row's fingerprint first.

The change stream's replay finds a session through its projected messages or its
`usage_session_changes` row. That journal began in v0.47.0 without a backfill,
so every writable open journals each session that has no row, one fresh
`usage_change_revision` per session, so the stream can page between any two of
them. Usage cache consumers then see those sessions change once.

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
represents unclassified messages and cannot carry both meanings. Storage writes
such a copy once: `db.StoredDialogue` decides the value every backend writes.
When the dialogue equals `content` and `thinking_text` is empty, backends store
NULL, which every search already reads as `content` through
`COALESCE(dialogue_text, content)`. A message with thinking text keeps its copy,
because search treats NULL dialogue beside thinking text as a legacy row whose
content still contains the thinking.

- SQLite sets `messages.dialogue_is_content` on those rows, and message readers
  restore the projection from `content`. SQL that needs the full projection
  outside search reads `sqliteDialogueExprM` or `sqliteDialogueExprBare`. The
  writable open that adds the column moves existing copies in the same
  transaction; it leaves the full-text update triggers out of that UPDATE,
  because the indexed text does not change.
- Hosted `raw_message_dialogue` stores an empty `dialogue_text` with
  `is_content` set, and payload loads restore it from the payload's content.
- PostgreSQL, ClickHouse, and DuckDB mirrors keep no marker. PostgreSQL and
  ClickHouse rewrite existing copies once from schema setup, guarded by the
  `dialogue_dedupe_v1` and `agentsview_dialogue_deduplicated` metadata keys.
  The ClickHouse mutation must write `''` beneath the NULL; an UPDATE to plain
  NULL keeps the old string under the null mask. DuckDB schema version 24
  rebuilds the mirror.

Data version 146 reparses providers with dialogue classification in place,
including the shared Codex parser. Retained sources that cannot be reparsed keep
their content and classification gaps; export itself never reads the original
source.

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
`tool_call_occurrence_agent_state` holds per-agent result coordinates. Claude
uses the first two tables for hash-only checkpoints, trusting bytes before the
128 KiB tail anchor as append-only. The last table is populated lazily when a
call receives a late result. Other providers do not maintain Codex signal state.
Full writes commit the signal seed with the content and bind it to the stored
transcript revision inside SQLite. A failed seed rolls back the content; there
is no post-commit revision read. During a full resync, the disposable
replacement archive defers the tool-call ID and result metadata indexes until
after the bulk load. Rebuilding them must succeed before the replacement can be
installed.

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

Automatic repository project naming belongs to the writable SQLite archive.
`repository_project_names` retains choices derived from producer-recorded
remotes and source snapshots; explicit folder rules and session assignments win.
Identity writes apply names in the same transaction and retain source snapshot
labels. Metadata copies preserve the registry, and rebuilds reapply it after
restoring snapshots. The metadata-only `db repair-projects` command uses the
same resolver. Updates advance session publication timestamps and reconcile
identity observations, so replica reads consume the corrected project without
their own naming inference. Hosted raw derivation does not consume this local
archive registry.

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
- Published `hostedRead` operations keep physical queries, alias/link/pin
  hydration and cursors in one read-only repeatable-read transaction. Borrowed
  views share runtime locks and configuration, never pool ownership. Their
  pricing loads run synchronously on that transaction and stay view-local;
  detached pricing singleflight belongs only to owning stores. Source probes
  retain their separate live revision fence.
- All four backends share activity report assembly and session usage selection
  in `internal/activity`. Explicit inputs supply usage, candidate streams,
  message counts and project maps; SQL, pricing, caches and read lifetimes
  stay in each backend. Daily usage query strategies and aggregation loops
  remain per backend.
- DuckDB Activity reports select, price, and sum usage in SQL with
  `sumActivityUsage`, and derive session runs in SQL with `activitySessionRuns`,
  both in `internal/duckdb`. Go resolves each model to its rates over the range
  as a schedule of segments; SQL picks a row's segment and pricing band and does
  the integer cost math. The SQLite, PostgreSQL, and ClickHouse reports keep the
  Go rules in `internal/activity`, because the SQL was slower for them on a
  one-year archive of 30,000 sessions:
  - SQLite: the usage step took 6.2 s in SQL and 0.99 s in Go, 6.3x slower. One
    sort of the 600,000 usage rows already takes 1.1 s, and the rules need
    several. The runs SQL took 11.1 s against 1.9 s for the Go pairing, 5.8x
    slower, even with an inexact stand-in for DuckDB's
    `last_value(... IGNORE NULLS)`, which SQLite lacks.
  - PostgreSQL: the usage selection alone took 1.86 s at best, against about 1 s
    for the whole Go usage step, at least 1.9x slower.
  - ClickHouse: the SQL was up to 1.7x faster cold, but reopening a range that
    includes today was 1.2x to 2.8x slower than the kept usage ranges and
    pairing memo.

### Harness-recorded costs

A harness-recorded cost is a figure the agent computed from its own rates. It is
stored as a usage event with `cost_status = 'recorded'`
(`usagefacts.CostStatusRecorded`), or for one message under the
`recorded_cost_microdollars` key of that message's `token_usage` JSON
(`usagefacts.RecordedCostKey`). It prices a row only when
`usagefacts.ReportedCostIsPrice` allows it: the catalog has no rate for the
model and the figure is above zero. Every other status keeps the rule that a
reported cost wins.

- Decide in Go, after the catalog lookup, through `db.ReportedCostForPricing`.
  SQL cannot see the catalog, so SQL that zeroes billable tokens or sums
  explicit costs must treat recorded rows as unpriced and carry the figure in
  a separate `recorded_cost_microdollars` column (DuckDB and ClickHouse do
  this).
- Message rows select no cost in SQL. SQLite and PostgreSQL scanners fill it
  from the token JSON with `db.MessageRecordedCost`; DuckDB and ClickHouse
  extract it in SQL. ClickHouse stores it in
  `usage_messages.usage_recorded_cost` and the snapshot `usage_recorded_costs`
  array.
- A cost-only row whose recorded figure loses contributes nothing: no breakdown
  entry, no computed request count.
- `dbtest.SeedRecordedCostSessions` and `dbtest.AssertRecordedCostUsage` are the
  cross-backend acceptance check; every backend runs them.
- Validation writes a costed usage event with no model under
  `usagefacts.UnknownModel` (`<unknown>`), so usage reads keep it.

### Session usage totals

`session_usage_totals` holds one row per session with a total the agent recorded
for the whole session, such as Claude Code's `cost-state`. It overlaps the
session's usage rows, and with `subagent_scope = 'included'` its subagents'
rows, so no usage, cost, or rollup query reads it. Session usage loads it as
`SessionTotal` beside the computed totals.

- Only full-parse writes replace it: `SessionBatchWrite.ReplaceSessionTotal` and
  the staged paths that call `ReplaceSessionUsageTotal`. Other batch writes
  (repairs, replacements, artifact imports) leave it unchanged.
- Rebuilds copy it for retained orphan sessions. Artifact exports do not carry
  it yet, so a session imported from artifacts has none.
- Every backend stores `db.SessionUsageTotalColumns` and pushes on
  `SessionUsageTotalFingerprints` changes, like `session_plan_usage`.
  `dbtest.SeedSessionUsageTotal` and `AssertSessionUsageTotal` check each.

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
usage-event-derived data. Aggregate fingerprints bake session `agent`,
`started_at`, `project`, `machine`, `git_branch`, `group_key`, the cron label
derived from `session_name`, `is_automated`, and `user_message_count`. Those
fields determine deduplication, day bucketing, grouping, and the filter
dimensions of stored day totals. Journaled changes rebuild the session's rows
and totals; a separately named trigger journals `session_name` changes on
grouped sessions so older writers' trigger rebuilds leave it in place. Warm
daily reads filter totals in SQL and read per-session rows for authoritative
costs and dedup exceptions. Warm Top sessions rank session or group units in
cache SQL and use installed group labels. Only returned individual sessions load
archive names. Session counts group ordinary daily rows in SQL and add surviving
owners. Termination and `active_since` filters still use the archive snapshot.
Do not widen or narrow this live/baked boundary implicitly.

The cache format version is also the extractor compatibility version. Bump
`usageCacheFormatVersion` whenever fact extraction, `priceUsageFact`, web-search
fees, deduplication, rollup semantics, or query-time model canonicalization
change. Add a record in `internal/db/testdata/usage-cache-format-digests`
for each format bump. If this PR added the record, replace it when its output
changes; otherwise bump the format and record the new digest. The digest covers
stored facts and daily rollups for the fixture; add a case when new usage
behavior needs another input.
Each rollup install records the distinct
`(provider, reported model, canonical model)` lookups its daily rows used, and a
full-path read re-resolves only those against the current catalog. A catalog
digest change sends reads through that path until a coverage pass claims the
timezone with the new digest. The pass rebuilds only sessions whose lookups
resolve differently; `updated_at`-only refreshes rebuild nothing. Do not add a
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

SQLite Activity reports read usage from `usage_facts`. Their hourly buckets and
Copilot session-cost allocation need each row, which daily rollups drop. Before
taking its bulk read, a report captures every live session with usage inside its
padded usage bounds and fills their facts, the way the Usage page does. It then
reads the candidate sessions' token facts, plus Claude snapshot peers from other
captured sessions, through parallel cache reads, and runs the shared survivor
selection and pricing on them. `activityReportUsage`, the raw archive row path,
stays as the tests' oracle; reports must match it exactly.

Usage reads are exact. A cold aggregate request fills facts, builds the required
timezone rollups, then reads them in one pinned cache transaction. Without a
trusted timezone claim, verify every candidate session's facts fingerprint,
exact baked metadata, per-session pricing identity, resolved rate hashes, and
Cursor high-water mark. With a trusted claim, refresh journaled sessions and
invalidated installs, and verify coverage and concurrent installs in the pinned
cache transaction. Covered daily totals read ordinary usage from day totals and
load only window exception owners and matching authoritative sessions. Covered
Top sessions skip discovery; other window reads use date indexes. Without a
claim, enabled backfill starts coverage. After conflicts, daily totals and Top
sessions retry once with a fresh journal capture; further conflicts and other
reads use the full path. A result is no older than the archive snapshot captured
when the read began, and may be newer for a session whose facts were refilled
meanwhile. A session confirmed deleted during fill is dropped from the request.
`cached_at` is diagnostic only.

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

Refills retain token-message histories when the archive generation proves the
existing messages unchanged, the install revision matches, and no activity-only
facts or usage events exist. They seek past the saved message ordinal and
normalize only appended messages. Message triggers maintain
`sessions.usage_generation`. Edits and deletes rotate the generation. A
backdated fact timestamp, an appended activity-only fact, an edit, a deletion,
or a usage event falls back to complete extraction. Claude suffix replacement
and late Codex usage updates also use complete extraction.

Full resync suspends the generation insert trigger,
`messages_usage_generation_insert`, in the replacement archive during the bulk
load. Any row trigger on `messages` makes SQLite stage every multi-row insert
through a temporary table. With the search triggers already dropped, this
trigger alone added about half to message insert time on a synthetic
400,000-message resync. While it is suspended, an insert before an existing
ordinal does not rotate the generation, and new sessions get none. Before the
swap, `RebuildBulkImportIndexes` must reinstall the trigger and give every
session a new generation in one transaction. A failed restore aborts the swap
like a failed index rebuild. This discards only append prefixes cached from the
replacement during the load: the replacement has a new database ID, so it starts
with an empty usage cache. Live archives never suspend the trigger.

Full extractions compare facts in bounded batches against one read-only cache
snapshot and spool only changed or new rows. Comparison requires the expected
install revision; installation checks that revision again. Complete per-session
fact counts remove obsolete tails, and identity invalidation compares the
complete cache before and after install. Each fact kind uses its own fixed index
range, so a token-message append keeps activity and event rows at their existing
positions. Existing mixed sessions move their activity and event rows once, on
their next refill.

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

SQLite keeps each session's activity runs in memory per session and range start,
within a 128 MiB budget that also counts each entry's overhead. A run is a
stretch of touching pieces of one session with one model, each already capped at
the idle gap and trimmed against the session's earlier activity, as
`activity.SessionRunsBuilder` derives them from the pairing scan's candidates. A
report sweeps the runs through `activity.BuildRunArtifactsWithSurvivorUsage`, so
a refresh pairs only the sessions whose entries miss and sweeps far fewer
intervals than candidates. On a scratch archive of 100,000 sessions and 2.6
million candidates over seven months, the kept runs took 73 MiB, and a refresh
with ten changed sessions spent 163 ms pairing instead of 106 s and 168 ms
sweeping instead of 949 ms. The first report after a start still pairs every
session.

Each entry also keeps the window's counted user and assistant messages. A
message stored at a whole microsecond with an offset from -13:59 to +14:00 keeps
only its time, because the message counter's padded stored-string bounds pick
the same messages as the range does; any other message keeps its stored
timestamp. A report counts messages from those entries, so it never reads
messages a second time. A week report then a month report keep separate entries
for their range starts. Entries run to `max(EffectiveEnd, RangeEnd)` and remain
valid while the transcript revision and session creation time match. The
candidate stream leaves out candidates that start at or after the effective end
floored to microseconds, so runs keep one bit per piece that says whether an
earlier piece trimmed it, and clipping keeps a trimmed piece that starts in that
microsecond. Reader swaps clear the entries and reject stores from earlier
generations. A report whose new entries alone exceed the budget stores none of
them. Warm reads still load session revisions.

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

SQLite pairs activity and counts messages in one read of every timestamped
message in selected sessions through `idx_messages_velocity` on
`(session_id, ordinal, role, timestamp, content_length, model, is_system, source_subtype)`.
The index covers that read, so it never loads message rows, which carry
kilobytes of content, thinking, and dialogue text. On a 1.7 million message test
archive the index takes 99 MiB, 10 MiB more than its older five-column form, and
a busy week's pairing reads 41 MiB instead of 1.8 GiB. Writable opens widen the
older form after column migrations and log that startup is waiting; the rebuild
took about 6 s on that archive and needs no parser resync. Read-only opens use
whichever form the archive has. PostgreSQL keeps the five-column index.

The terminal candidates query and the message read need no result of each other.
When a bulk-read slot is free, SQLite runs the terminal query on it beside the
message read; otherwise it runs the two in turn. PostgreSQL runs them together
on its pool, with the message read split into one slice per 4,096 sessions, up
to two; a read snapshot shares one transaction, so there they run in turn. Both
then derive each session's runs on every available core through
`activity.DeriveSessionRunsInParallel`, which gives the runs the merged
candidate stream gives.

The terminal candidates query names `idx_messages_assistant_model` with
`INDEXED BY` for its prior-model lookup. The wider velocity index also covers
that lookup, and SQLite can otherwise choose it and step back through every
earlier message of a transcript that records no model.

SQLite keeps completed Activity reports in memory with a 128 MiB estimated-size
budget. Entries use `database_id` and `usage_pricing_revision` as their version.
The `usage_session_changes` journal invalidates an entry when a selected session
or an in-window Claude peer changes; a changed project identity map also
invalidates it. Partial reports, read-only archives, and reports filtered by
active, stale, or unclean termination status bypass this cache.

SQLite uses `usage_change_revision` and `analytics_change_revision` to invalidate
kept Tools, Velocity, and Signals reports. Tool-call updates and deletes, message
content-length repairs, and signal updates advance the analytics counter.
Read-only archives and reports filtered by active, stale, or unclean termination
status bypass this cache.

PostgreSQL keeps ongoing Activity reports' per-session activity and raw usage
inputs in memory with a 256 MiB estimated-size budget. Each session keeps its
runs and the ascending times of its counted user and assistant messages, derived
from one pairing over all time: the candidates a report range leaves out are
ones the aggregator rejects for that range anyway, so one entry serves every
effective end of the slot. The usage change journal invalidates changed sessions
and their matching snapshot peers. Tool events are not journaled; a tool event
change reaches PostgreSQL through a push that also changes the session's
`transcript_revision`, which the journal records. Each visit selects sessions,
pairs only sessions without an entry, sweeps the runs, selects usage snapshots,
and prices the surviving rows again. On a scratch database of 100,000 sessions
and 2.6 million candidates over seven months, the kept activity took 71 MiB, and
a refresh with ten changed sessions spent 90 ms pairing instead of 3.2 s and 209
ms sweeping instead of 1.0 s. Journal replacement, a changed gap cap, and
unavailable or regressed revisions rebuild the inputs. Large change sets use a
full read, and inputs above the budget aren't retained. Hosted stores and
transaction views use their existing reads.

DuckDB keeps each session's runs and counted message times in memory per session
and gap cap, within a 128 MiB budget, at the session's
`agentsview_push_fingerprint`. A push changes that fingerprint whenever a
session's messages or tool events change, so entries survive mirror replacements
and a refresh derives only changed sessions. A session without a fingerprint is
derived on every report. DuckDB derives the runs over all time in one SQL query,
`activitySessionRuns`, which pairs, caps, and trims the candidates and returns
each run's pieces, and clips them at the report's effective end. On the same
scratch data, deriving every session took 2.4 s instead of 4.8 s (2.0x), the
kept runs took 74 MiB, and a refresh with ten changed sessions spent 189 ms
instead of 315 ms on runs (1.7x), 216 ms sweeping, and 46 ms counting messages.

### PostgreSQL push preparation indexes

`idx_sessions_data_version`, `idx_sessions_parentless_workers`, and
`idx_sessions_source_missing` bound the parser-version check and empty repair
queries. `idx_sessions_id_tail` serves
[raw session ID lookups](#raw-session-id-lookups). Owner preparation installs
missing indexes automatically. Each first build scans `sessions` and blocks
writes until installation commits. Restricted publishers keep writing before
owner installation, using the existing queries until the indexes are available.

### Raw session ID lookups

`FindSessionIDsByRawSuffix` resolves a raw ID such as a bare Codex thread UUID
to stored IDs like `codex:<uuid>` or `host~codex:<uuid>`. Every match shares the
raw ID's tail, the text after its last `:` or `~` (`db.SessionIDTail`), so each
backend filters on that tail before it checks the exact suffix forms:

- SQLite and PostgreSQL index the tail expression as `idx_sessions_id_tail`. The
  query must repeat the indexed expression exactly. PostgreSQL installs it as
  a [push preparation index](#postgresql-push-preparation-indexes), so a
  restricted publisher keeps pushing and lookups scan until an owner installs
  it.
- ClickHouse keeps an `id_tail` bloom filter skipping index on `sessions`. The
  candidate subquery reads with `final = 0` because FINAL widens each matching
  granule to every overlapping key range in other parts. The outer read
  applies FINAL by primary key.
- DuckDB scans. It does not use an expression index for this filter, and a
  columnar scan of the disposable mirror's IDs takes a few milliseconds.

`internal/dbtest` `AssertRawSuffix` checks the same cases on every backend.

### Usage archive indexes

The usage cache discovers bounded-window candidates through
`idx_messages_usage_timestamp` and `idx_messages_activity_timestamp`, then
extracts each selected session through the index-only
`idx_messages_usage_session_covering` scan. The global activity index is for
usage-cache candidate discovery, not the Activity report; Activity pairing
continues to avoid a global timestamp scan. Keep these indexes narrow except for
the single session-keyed covering index that carries `token_usage`.

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
read-only opens require the indexes. The same drop and rebuild suspend and
restore the usage generation insert trigger; see
[Usage cache divergence](#usage-cache-divergence).

### Sidebar tree index

The sidebar's recursive child lookup and parent-only lookups share
`idx_sessions_parent` on `(parent_session_id, is_automated)`, excluding null
parents. Writable opens widen the parent-only form that `schema.sql` creates,
after column migrations, and startup `PRAGMA optimize` gathers its statistics.
Bulk imports retain the index, and widening it needs no parser resync. Read-only
opens use whichever form the archive has until a writable open widens it.

SQLite's recursive session-tree steps, including unpaged sidebar and session
list queries with children, put the tree first with `CROSS JOIN` so each queued
row seeks its children. When few sessions have parents, startup statistics
otherwise move a sessions scan outside that per-node lookup.

PostgreSQL keeps `idx_sessions_parent` on `parent_session_id` alone. The wider
SQLite index lets each recursive child lookup seek both `parent_session_id` and
`is_automated` instead of filtering rows found by parent alone. PostgreSQL
filters `is_automated` while it uses the parent-only index.

Hosted PostgreSQL sidebar pages resolve contextual raw parent links and legacy
parent edges once per statement. Root orphan checks and recursive child joins
use that query-local edge set without changing parent identity or visibility.

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
SQLite choosing an older index that still reads message bodies. After optimize,
the writer raises small `*_fts_data` row estimates and reloads statistics so
FTS5 segment deletes keep using rowid searches as an empty archive grows. Full
resync defers this index during import and rebuilds it before the replacement
archive is installed. Read-only opens do not create indexes or collect
statistics.

### Substring search index

Substring and regex content search narrow their message and thinking branches
with two FTS5 tables that use the built-in `trigram` tokenizer:
`message_trigram_fts` over `COALESCE(dialogue_text, content)` and
`thinking_trigram_fts` over `thinking_text`. Both read from the
`message_trigram_source` view and store `detail='none'` and `columnsize=0`, so
they hold only message IDs per three-character sequence, about a fifth of the
indexed text. Each field gets its own table because a `detail='none'` table
cannot filter by column; `detail='column'` measured nearly three times larger.

The index only narrows candidates. A search intersects the listed IDs for the
pattern's sequences, up to 32 of them, and the existing `LIKE` still decides
every row. The tokenizer folds case for every script while `LIKE` folds only
ASCII, so the index returns a superset and results stay identical to a scan.
Patterns shorter than three characters, single-session searches, and branches
where the index lists more than a quarter of the messages in the scope's
sessions keep the scan. A capped count of the listed IDs makes that choice; it
stops as soon as the share is passed. Tool input and tool result branches are
not indexed.

Terms search uses `message_trigram_fts` under the same share rule per term. When
the index lists any term, the query starts from the messages it lists for the
first such term. It finds each message's exchange from the nearest eligible user
message at or before it and the next one after, checks the other terms against
that exchange's messages, and joins bodies only for the page. When the index
lists no term, the session prefilter finds the sessions where each term occurs
and the query groups every message of those sessions into exchanges. Both
queries must return the same rows; `TestSearchContentTermsAnchorMatchesScan`
compares them.

Triggers keep both tables current on message insert, delete, and edits to
`content`, `dialogue_text`, or `thinking_text`. Bulk session deletes remove a
session's entries in one statement each and suspend `messages_trigram_ad`, as
they do for the other FTS indexes. `DropFTS` and `RebuildFTS` drop and rebuild
the index with the other FTS tables during a full resync, and searches scan
while it is absent. Claude.ai and ChatGPT imports drop it on the live archive,
so a search that chose index filters before the drop gets a missing-table error;
it reruns once and scans.

The first writable open after column migrations builds the index once for an
existing archive, before HTTP readiness, and logs that startup is waiting for
the message scan. Tokenizing costs about 2.7 times the porter FTS index, near 30
MB of message text per second. Read-only opens use the index when present and
scan otherwise. Hand-built legacy schemas in tests must drop
`message_trigram_source` and the index before rebuilding or narrowing
`messages`, because the view and triggers name its columns.

### Transcript usage identity

Token usage, Claude message/request identities, and source UUID participate in
transcript revision equality. Finalizing a streamed message can therefore bump
`transcript_revision` and `local_modified_at`, invalidate secret-scan freshness,
mark the session updated for read-progress/UI purposes, and enqueue the normal
artifact, recall, PostgreSQL, and DuckDB refreshes. Full resync reconciliation
must compare the same fields so incremental and resync paths agree. A no-op
message replacement preserves existing secret findings; changed transcript
content clears them for a fresh scan.

### PostgreSQL usage change journal

PostgreSQL's usage change journal tracks session, usage, pricing, Cursor usage,
and project identity writes, including changes to source archives. Installation
requires `CREATE` on the schema and `TRIGGER` on the watched tables in addition
to normal table access. See [PostgreSQL Sync](../pg-sync.md) for installation
locks. CockroachDB has no journal.

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

### PostgreSQL frustration marker reads

Signals counts frustration markers for sessions the in-memory memo does not
hold. PostgreSQL and CockroachDB read the session versions and those sessions'
prompts in one statement, `pgFrustrationPromptsSQL`, with the session IDs as one
array parameter (`= ANY($1)`). A prompt made of ASCII and
`signals.PlainPromptSymbols`, the alphabet the
[ClickHouse read](../internal/clickhouse-mirror.md#dashboard-facts) also uses,
is judged in SQL and comes back only when it is a marker, without its text. Go
judges the rest with `signals.IsFrustrationMarker`:

- prompts with any other rune;
- every prompt when the server encoding is not UTF8. The store reads the
  encoding first and then runs `pgFrustrationGoPromptsSQL`, which binds only
  the session IDs: PostgreSQL converts parameters to the server encoding
  before the query runs, and the alphabet pattern holds runes LATIN1 and
  similar encodings lack;
- plain prompts longer than 16 KiB after code fences are stripped, when a cheap
  bound cannot rule out the all-capitals rule.

The SQL follows `IsFrustrationMarker` step by step. Port it with these rules:

- PostgreSQL patterns are POSIX ARE and CockroachDB patterns are RE2, so use
  only syntax both read the same way. `\b` means backspace in ARE, and the ARE
  word escapes and `lower()` follow the server locale. Write word boundaries
  as `(^|[^0-9A-Za-z_])` and `($|[^0-9A-Za-z_])`, letters as ASCII case pairs,
  whitespace as the ASCII class `unicode.IsSpace` accepts, and non-ASCII runes
  as literal characters.
- Measure lengths with `octet_length`; Go compares bytes.
- Count matches with `cardinality(regexp_split_to_array(...)) - 1`. CockroachDB
  has no `regexp_count`. Cast numeric parameters, because CockroachDB does not
  infer their types.
- Prefer boolean `~` tests to `regexp_replace` over long text. On a 484 MiB
  fixture a boolean pass took about 1 s and a replacing pass 11 to 17 s, so
  counting every word of every prompt took 55 s against 6.6 s for the Go scan.
  The caps rule is therefore ruled out with a bound: all-capital words are at
  most the `[A-Z]{2,}` runs, and words of two or more letters are at least 32
  per match of 32 such words in a row and at least `FrustrationCapsMinWords`.
  The caps-run count divided by `FrustrationCapsMinWords` is tested first.
  When that bound is below the threshold, so is the exact ratio. SQL counts
  every word for unresolved prompts of at most 16 KiB, which covers typed
  prompts and short prompts after a fenced paste. Longer prompts use the
  32-word bound before falling back to Go. `translate()` is slower
  still (42 s), so the SQL never uses it.

On a fixture of 2,500 sessions with 41,162 prompts (484 MiB of prompt text, 1.9%
of it outside the plain alphabet), one Signals load with an empty memo took 4.1
s instead of 6.6 s. It sent 15.8 MiB to Go instead of 485.5 MiB, and Go
allocated 139 MiB instead of 1.62 GiB, with the same per-session counts.

`frustration_sql_pgtest_test.go` compares the counts with `IsFrustrationMarker`
on hand-labeled edge cases and on a seeded random corpus, and checks which
prompts come back with text. Run it against CockroachDB as well when changing
the SQL.

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
- When readers hold the mirror, catch up and swap a reusable standby using its
  own cursors. Bootstrap it from the main file and WAL, retry while it is
  held, and discard it after a full rebuild.
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
  `DELETE ... WHERE session_id IN (...) AND push_version < v` limited to keys
  with no row at `v` or later (`vanishedRowsDeleteSQL`), then session rows. Do
  not delete older rows whose key the push rewrote; `final=1` reads already
  collapse them, and deleting them writes patch rows. Store every ClickHouse
  push cursor and version in the mirror's `sync_metadata`. Never store
  ClickHouse sync state in SQLite.
- Every mirrored table is `ReplacingMergeTree(push_version)`. Every connection
  sets `final = 1` and, unless the URL sets it, `max_threads = 8`. Do not
  write `FINAL` in query text. One exception: a lookup may collect sort keys
  in an `IN` subquery with `SETTINGS final = 0` and let the outer read apply
  FINAL, because a skipping index loses most of its effect under FINAL (see
  [Raw session ID lookups](#raw-session-id-lookups)). `clickhouse serve` keeps
  a separate 4-connection pool for the session list, sidebar filters, and
  one-session reads (`pageQueryContext`); dashboard, Usage, report, and search
  scans use the main pool (`queryContext`), sized by the URL's
  `max_open_conns` (default 12). `OpenForAdmin` bootstraps through the server
  `default` database; do not ping a DSN-path database that does not exist yet.
- Derived tables (`usage_messages`, `terminal_event_snapshots`) are
  insert-maintained by materialized views and keyed by
  `ReplacingMergeTree(revision)`, where live rows use an odd revision above
  the backfill's even one. Readers check them against `sessions.push_version`:
  `usage_messages` rows count at or above it, because messages land before the
  session row is published, and `terminal_event_snapshots` rows match it
  exactly. A materialized view runs inside the insert and reads joined tables
  in full, so filter a joined table to the inserted block's sessions. Create
  and update long-lived views through `ensureView`, which replaces the query
  of a view an older release created. Views never see deletes;
  `deleteMirrorSessions` clears the derived tables. Startup backfills record
  completion in `sync_metadata` only after finishing, never modify source
  tables, and must stay safe to repeat. See the
  [Activity report reads](../internal/clickhouse-mirror.md#activity-report-reads)
  design section.
- `clickhouse push` stores per-session dashboard facts on `sessions`
  (`dashboard_facts_version` and the `fact_*` columns) and copies each tool
  call's message timestamp and model onto `tool_calls` (`message_facts` marks
  those rows). Reads use them only on rows at `dashboardFactsVersion` and scan
  messages for every other row, so results match the scan. A fact must count
  exactly what the scan it replaces counts; change a definition only with a
  version bump, which makes each archive's next push a full push. SQLite,
  PostgreSQL, and DuckDB still scan; this is a ClickHouse storage-layout
  difference. See
  [Dashboard facts](../internal/clickhouse-mirror.md#dashboard-facts).
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
- Do not add skipping indexes or projections for time ranges or text search on
  session-keyed tables such as `messages` and `usage_messages`. Session IDs
  are random, so each granule mixes sessions from unrelated dates, and every
  part spans the whole key range. Under `final = 1` the exact-mode expansion
  (`use_skip_indexes_if_final_exact_mode`) adds back most granules an index
  skipped, and projections are not read at all. On a 4.4M-message mirror a
  minmax on `messages.timestamp` skipped none of 541 granules, the former
  `usage_messages` minmax made the Activity usage read 1.2x to 3.4x slower,
  and a `text` index on `messages.content` made `ILIKE` search about 3x slower
  with 3.5x the memory while adding 24% to storage, 3x to insert time, and 5x
  to merge time. Before adding an index, measure the real query with
  `EXPLAIN indexes = 1` under `final = 1`, on more than one part.
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

### Image text

When `[image_text]` is enabled, the daemon extracts text from retained inline
and offloaded tool-result images using the configured local GLM-OCR service. It
caches normalized text by decoded image hash and processor identity, and tracks
current occurrences in the same transaction as tool results. The first enabled
pass builds occurrences for existing archives. Extraction passes start requests
for two minutes; each request stops after Docbank's timeout. Each image is read
from a copy whose bytes match its hash, and its type is detected from those
bytes. Failed requests and missing or corrupt copies retry with backoff;
oversized images, verified bytes that are not PNG, JPEG, or WebP, and invalid
input are terminal. A full archive rebuild preserves cache entries for retained
images and rebuilds their occurrences. Removing the last occurrence removes its
cached text during the next pass. Disabling image text and restarting removes
cached text and mirror matches; re-enabling extracts it again. Usage-only
archives do not store image text.

Successful text is published to PostgreSQL, DuckDB, and ClickHouse as
session-owned `image_text` rows keyed by the tool call or result event, image
index, and processor, with the image hash, provider, model, and revision. A new
extraction advances the session's `sync_marker`, so the next incremental push
sends it without changing the transcript revision. Global keyword search and
substring and regex `tool_result` searches match this text in every backend,
anchored to the message holding the tool call.
