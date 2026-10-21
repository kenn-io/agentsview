# Attached Archive Source Implementation Plan

This is the execution record for the reviewed design. The task contracts below
remain as the implementation rationale; the status section records what was
actually verified and what remains.

**Goal:** Use a Docbank-backed session archive alongside current local sessions,
with original machine attribution. The first source reader uses existing
compressed backup snapshots; no whole-vault restore is required.

**Architecture:** Bootstrap a replaceable compact index from one verified
temporary application database. Read each accepted source through Docbank,
materialize it temporarily, and use the existing sync engine. Keep ownership and
completion records in the receiving database; integrate archive work with
ordinary sync and the shared rebuild coordinator.

**Tech Stack:** Go 1.27.2, SQLite/FTS5, existing provider parsers, tagged
Docbank and Kit snapshot readers, testify.

**Spec:**
[Reviewed archive source design](../specs/2026-10-15-archive-source-design.md).
Design baseline: `3f340991`; code baseline: `ba819a4e`.

Implementation starts from `176a0a49`, with the reviewed design preserved. Kit's
reader is released and Docbank's wrapper is merged in #988. A Docbank release
containing that wrapper is still required before this PR is mergeable.

## Implementation status

A1–A5 are implemented: pinned sources, compact indexing, transactional
ownership, per-source parsing, ordinary sync scheduling, and atomic rebuild
integration. A6's local rehearsal passed against a disposable copy of a real
archive, with no restored vault or permanent extracted transcript tree. Full
rebuild kept message content, session identities, machine labels, and ownership
unchanged. API browsing, offline history, restart, relocation, source retries,
repository verification, and independently hashed files over 4 GiB also passed.
Synthetic tests cover receiver curation, ownership conflicts across import
routes, artifact export, raw capture, and a DuckDB mirror.

The final review found two issues, both covered by failing-then-passing tests:
stale pending work could repeat after a rebuild, and bootstrap accepted a
transport-qualified native identity. Completion is now checked under the sync
lock before copying a source; rebuilds still deliberately reparse. Saved native
identities are checked before publishing the compact index.

Remaining gates and limits:

- Pin a tagged Docbank release containing #988 before merging.
- The broad local suite has native watcher and script-harness failures. Focused
  archive, worker, ownership, and race checks pass. Record CI results
  separately; the full suite is not reported green.
- Network-share performance and remote database replicas were not exercised.
  Disk and heap sampling covered one large source on a local repository copy,
  not the peak of an entire initial import.
- Single-session refresh directs users to `agentsview sync --full`; scoped
  parser-version changes still drain only pending sources.

The implementation uses Docbank's merged reader commit for development. Direct
attachment also accepts a stopped archive directory, including Docbank restore
output with its application database. Both readers share parsing and ownership.
Single-machine archives accept a display name without exposing installation IDs
in configuration. No installed binary or live archive is changed by the
implementation checks.

## Delivery order

1. [Kit reader](2026-10-16-kit-snapshot-reader.md): independently tested object
   reads and short shared leases.
1. [Docbank wrapper](2026-10-16-docbank-snapshot-reader.md): public embedded API
   without a live or restored vault.
1. This plan: AgentsView configuration, indexing, transactional provenance,
   scheduling, rebuild integration, and real-backup rehearsal.

Each component can be reviewed separately. Use tagged dependencies before
downstream PRs become mergeable. Do not invent future version numbers or publish
releases without explicit authorization. Recheck the implementation checkouts
against these pinned baselines before applying tasks.

## Global constraints

- First delivery supports accepted Claude and Codex sources in an explicitly
  pinned snapshot. No latest-following, capture advancement, cross-route
  deduplication, publisher adoption, or source-management UI.
- Originals stay compressed; only one source's files are materialized at a time.
  Use the existing 16 GiB limit including companions and dependencies. Account
  separately for bootstrap SQLite, parser staging and rebuild space.
- Preserve `installation~parser-id`, foreign fork parents, machine labels,
  stable `archive://` paths, receiver curation and receiver deletion rules.
- Refuse the receiver's ID, aliases and retained artifact publication authority
  at attachment and within publication. Never adopt source credentials or
  telemetry identity. Later source-archive curation is not imported.
- Source suppression is capture-time policy applied before namespacing. Copies
  received through artifact/HTTP/SSH namespaces may coexist; report that limit
  without inventing duplicate counts.
- Provenance and content commit together. Completion advances only after every
  source session succeeds or is intentionally suppressed. No whole-database
  copy per incremental source.
- One archive drainer per receiving database. Local work precedes archive
  sources through an acknowledged watcher handoff; do not rely on mutex order.
- Release each repository lease before parsing. A busy ordinary sync defers; an
  active full rebuild waits with cancellation, progress and capped backoff.
- Keep global parser-version markers separate from attachment completion.
  Unavailable/detached history survives rebuilds. Never publish partial
  rebuilds.
- Use isolated test roots and worktree binaries. Do not open live user state or
  alter either retirement recovery copy. Commit tested tasks locally.
- Go checks use `CGO_ENABLED=1` and `-tags fts5`. Read the repository's testing,
  storage, background-work and build guides. Preserve shipped migrations.

## Review focus

- Saved SQLite may have valid archive markers but missing required columns or
  inconsistent identity records: refuse before publishing an index (A1).
- An exact foreign session ID may already belong to another source: refuse
  before changing metadata, messages, assets or ownership (A2/A3).
- Cancelled large Codex publication must not leave content without provenance or
  falsely completed source state (A2/A3).
- Watcher callbacks, retries and manual sync can overlap archive work: ingest
  each queued batch once and process each source once (A4).
- Backup contention during rebuild must preserve completed work; disconnection
  or corruption must keep the original live database intact (A5).

## File structure

New package `internal/archivesource` imports `config`, `db`, `rawderive`,
`rawsync`, `sync` and Docbank. `sync` must not import it; `cmd/agentsview` wires
the two together. This preserves the existing `rawderive -> sync` dependency.

| Paths                                                                                               | Responsibility                                                    |
| --------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------- |
| `internal/config/archive_sources.go` (new), `config.go`                                             | Decode and validate pinned archive configuration                  |
| `internal/archivesource/index.go`, `schema.go` (new)                                                | Read-only bootstrap, compact index and source-schema checks       |
| `internal/archivesource/reader.go`, `source.go` (new)                                               | Snapshot adapter, identity policy and source materialization      |
| `internal/archivesource/manager.go`, `rebuild.go` (new)                                             | Single drain, pending versions, ordinary/rebuild orchestration    |
| `internal/db/archive_attachments.go` (new), `schema.sql`, `db.go`                                   | Durable registrations, source bindings, completion and provenance |
| `internal/db/session_batch.go`, `staged_content.go`                                                 | Same-transaction archive ownership and content publication        |
| `internal/sync/archive_source.go` (new), `engine.go`                                                | Prepared source execution through normal/staged writers           |
| `internal/sync/watcher.go`, `rebuild_contributor.go`                                                | Acknowledged handoff and custom rebuild execution                 |
| `internal/db/orphaned.go`                                                                           | Explicit-column state and detached-history carry-forward          |
| `cmd/agentsview/archive_source.go` (new), `main.go`, `sync.go`, `sync_worker.go`, `session_sync.go` | Common wiring for daemon, CLI and worker                          |
| New adjacent `*_test.go` files                                                                      | Production-path regressions for each task                         |
| `docs/configuration.md`, `docs/usage.md`, `docs/agents/storage.md`                                  | Setup, limits, status and state ownership                         |

### Task A1: Register a snapshot and bootstrap its accepted sources

**Files:** Config, index/schema and database files in the map; new
`internal/config/archive_sources_test.go`,
`internal/archivesource/index_test.go` and
`internal/db/archive_attachments_test.go`; `go.mod`/`go.sum`.

**Interfaces:** Define:

```go
// config
type ArchiveSource struct { Name, Repository, Snapshot string }
func (c Config) ValidateArchiveSources() error // Config.ArchiveSources []ArchiveSource

// db
type ArchiveAttachmentKey struct { RepositoryID, SnapshotID string }
type ArchiveSourceIdentity struct { ArchiveID, DeviceID, RootID, SourceKey string }
type ArchiveSourceProvenance struct {
    Attachment ArchiveAttachmentKey
    Source ArchiveSourceIdentity
    ManifestID string
}
type ArchiveSessionProvenance struct {
    ArchiveSourceProvenance
    NativeSessionID string
}
func (d *DB) RegisterArchiveAttachment(ctx context.Context, key ArchiveAttachmentKey, archiveID string) error
func (d *DB) CompleteArchiveSource(ctx context.Context, source ArchiveSourceProvenance, target string) error
```

Use distinct `archive_attachments`, `archive_source_bindings`,
`archive_attachment_sources` and `archive_session_provenance` tables. The stable
source binding is original archive/device/root/source identity plus the accepted
manifest. Snapshot membership and completion are keyed by attachment and source;
session provenance is keyed by resulting session ID. A second different manifest
for a bound source is an error. Identical source/manifest observations may be
reused; repository mount paths and configuration labels are not keys.

Store a per-attachment pending target and count. A source's completed target is
a deterministic digest of its manifest ID, sorted provider data versions and
normalized receiver content/image/blocked-category policy. Persist errors
separately; they never advance completion. Source ownership is durable even if
the compact index is deleted.

- Write `TestArchiveSourceBootstrapFromRepository`. Create a synthetic archive
  using production capture/import/backup with one Claude source and one Codex
  source. Make capture/vault paths unavailable. Bootstrap only from the
  backup, then assert two accepted sources, original root/device IDs and
  suppression records. Include an unaccepted manifest in the vault and an
  unsupported provider's originals; neither becomes an accepted source. Report
  unsupported coverage separately. Do not copy messages into the compact
  index. Add schema cases: inventory version other than 1, missing required
  table or column, inconsistent archive-only markers, bad canonical manifest
  and missing/incorrect-sized object membership. Assert no published index.

    ```go
    assert.Equal(t, 2, acceptedSources)
    assert.Equal(t, originalInstallID, indexedDeviceID)
    assert.NoFileExists(t, temporaryDatabase)
    // Each rejected fixture leaves both receiver sessions and index unchanged.
    assert.Equal(t, beforeSessions, afterSessions)
    ```

- Write configuration and DB tests for an empty snapshot, repeated labels,
  equivalent copied repository paths, changed repository UUID, immutable
  source bindings and completion targets. Verify archive-only installations
  reject attachments. Verify schema additions preserve existing sessions and
  never rewrite a shipped migration or increment parser versions needlessly.

- Run
  `CGO_ENABLED=1 go test -tags fts5 ./internal/config ./internal/db ./internal/archivesource -run 'TestArchiveSource|TestArchiveAttachment'`
  and confirm the missing feature fails.

- Implement bootstrap with D1's reader. Verify `application/inventory.json`,
  `application/sessions.db`, saved identity and recovery settings as needed;
  use schema capability checks from the spec. Close the reader before
  read-only immutable SQLite inspection; its retained membership metadata can
  validate every referenced object. Use explicit column lists. Validate all
  accepted manifests before publishing the index. Reject receiver
  identity/alias/publication-authority matches.

- Keep the compact SQLite index under the receiving data directory, keyed by
  repository UUID, snapshot and index version 1. Close/checkpoint it before
  atomic publication. Under the existing data-directory owner, remove only
  this feature's abandoned bootstrap temporaries on restart. Report expected
  database bytes before copying and stream progress; cleanup failure is an
  error. An unchanged attachment uses registry/index metadata, not
  transcripts.

- Run focused tests including cancellation and interrupted bootstrap cleanup,
  formatting, vet and required lint. Commit with the tagged Docbank
  dependency; no application feature is claimed usable yet.

### Task A2: Publish session ownership with its content

**Files:** `internal/db/session_batch.go`, `staged_content.go`,
`archive_attachments.go`, `internal/sync/engine.go`, new
`internal/db/archive_provenance_test.go` and sync tests.

**Interfaces:** Add `ArchiveProvenance *ArchiveSessionProvenance` to
`db.SessionBatchWrite`. Add this producer to `sync.EngineConfig`:

```go
ArchiveWriteProvenance func(*db.Session, string) (db.ArchiveSessionProvenance, error)
```

For staged writes add:

```go
func (d *DB) WriteSessionBatchStaged(ctx context.Context, write SessionBatchWrite,
    staged StagedToolResults, blocked map[string]bool, signals StagedSignalsFunc) error
```

Share the existing staged transaction primitive. This entry point includes the
session-row update, ownership check, provenance, checkpoint and content in one
transaction; do not call `UpsertSessionPendingContent` first for this path. Keep
`ArchiveSessionPolicy` for suppression decisions, without provenance writes.

- Write `TestArchiveProvenanceCommitsWithContent`, table-driven across the
  ordinary batch writer and real staged Codex writer. Force a conflicting
  resulting ID and a transaction failure after the provenance write. Query
  persisted rows to show that neither content nor provenance was partially
  committed. Also test a pre-existing local/remote/artifact row with the same
  ID, a changed accepted manifest, and a receiver alias/publication-authority
  change between preparation and the transaction.

    ```go
    require.Error(t, publishErr)
    assert.Equal(t, originalMessages, storedMessages)
    assert.Equal(t, originalSession, storedSession)
    assert.Equal(t, originalProvenance, storedProvenance)
    ```

- Run the focused tests and confirm the missing transactional behavior fails.
  Trace every caller of the changed writers before editing signatures.

- Bind provenance inside each session savepoint/transaction and check receiver
  ownership there before any update. Wire the engine producer through ordinary
  bulk and staged paths. `PrepareLocalSource` already requests
  cancellation-aware bulk writes; do not add unrelated hooks to every local
  incremental path. Check staged no-change fast paths still bind or verify
  provenance. Preserve curation, message IDs, export-index writes, policy,
  images, usage and secret scanning through the existing writer.

- Run
  `CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/sync -run 'ArchiveProvenance|Staged|SessionBatch'`,
  then formatting, vet and required lint. Commit.

### Task A3: Materialize and sync one accepted source

**Files:** `internal/archivesource/reader.go`, `source.go`, adjacent tests,
`internal/sync/archive_source.go` and adjacent tests.

**Interfaces:** `snapshotObjects` implements `rawderive.ObjectStore.CopyObject`
using D1's reader. Add:

```go
func (e *Engine) SyncAttachedSource(ctx context.Context,
    source db.ArchiveSourceProvenance, target string,
    prepare func() (EngineConfig, string, func() error, error),
    progress ProgressFunc) (SyncStats, error)
```

This operation holds the receiving engine's `syncMu` for one source, creates an
ephemeral parser engine with the supplied verified paths, uses
`ReparsePathsContext`, and records completion before releasing the mutex only
after checking all expected sessions and intentional suppressions and removing
the temporary source tree. Preparation runs under this same lock after checking
that the source is still pending; deliberate rebuilds bypass that check. A
rebuild parent supplies the old-archive reconciliation context to the child
engine.

- Write `TestAttachedSourceMatchesProviderHistory`: real Claude
  continuation/subagent files and a staged Codex source, including a parent in
  another captured root. Assert exact transcript values, original machine,
  prefixed parent IDs, stable message IDs on retry, suppression, retained
  receiver curation, automation flags and policy-projected content. Include
  all-suppressed and partially failing multi-session sources.

    ```go
    assert.Equal(t, originalInstallID+"~"+nativeID, stored.ID)
    assert.Equal(t, originalInstallID, stored.Machine)
    assert.Equal(t, firstMessageIDs, retryMessageIDs)
    assert.Equal(t, receiverPinsBefore, receiverPinsAfter)
    ```

- Add missing/corrupt-object cases with existing receiver content. A failed
  materialization must leave it unchanged. Test the 16 GiB aggregate limit
  through declared manifests before allocation; companions count toward it.
  Add parse cancellation and cleanup failure cases. The source stays pending
  on either failure; successful earlier sessions can remain committed.

- Run
  `CGO_ENABLED=1 go test -tags fts5 ./internal/archivesource ./internal/sync -run 'TestAttachedSource'`
  and observe the missing path.

- Implement one-source copy/verify using `Materializer`, release the reader,
  then call `PrepareLocalSource` and the new engine operation. Use captured
  installation ID and `installation~` prefix, including fork parents. Apply
  source suppressions before prefixing and receiver suppressions afterward.
  Persist `archive://` paths using the existing rewriting rules. Include
  original cross-root dependencies; never scan the receiver's provider homes.

- Require every accepted session to be written or intentionally suppressed
  before `CompleteArchiveSource`. Close parser/scratch handles, then clean the
  source tree before another source starts. Close errors remain errors.

- Run focused tests and the existing rawarchive/rawderive tests, formatting, vet
  and required lint. Commit.

### Task A4: Give local sync priority and make archive draining single-flight

**Files:** `internal/archivesource/manager.go`, tests,
`internal/sync/watcher.go`, watcher tests, and command wiring from the file map.

**Interfaces:** Add `func (w *Watcher) FlushPending(ctx context.Context) error`.
It sends a request to the dispatch loop, waits for any in-flight callback,
processes a snapshot of fresh pending work, and acknowledges its result. Keep
retained retries under existing backoff. Add:

```go
func NewManager(ownerCtx context.Context, cfg config.Config, database *db.DB,
    engine *sync.Engine, beforeSource func(context.Context) error) (*Manager, error)
func (m *Manager) Drain(ctx context.Context, progress sync.ProgressFunc) (sync.SyncStats, error)
```

The active drain belongs to `ownerCtx`; concurrent callers join its result.
Cancelling a joining caller ends only that wait. A direct CLI uses its command
context as owner. Single-flight ownership is distinct from `syncMu` and does not
add a polling service. Durable pending state remains authoritative.

- Write `TestArchiveDrainYieldsToPendingLocalWork` with real watcher batches and
  at least three archive sources. Queue a local update between sources and
  record persisted-write order, not elapsed speed. Assert the local update is
  visible before the second archive source starts. Cover an in-flight
  callback, retained retry, new arrivals after the handoff snapshot and stop.

    ```go
    assert.Equal(t, []string{"archive-1", "local", "archive-2", "archive-3"}, publicationOrder)
    ```

- Write `TestArchiveDrainSingleFlight`: overlap periodic/requested calls, cancel
  one joiner, and assert each source is read once and the owner can finish.
  Compare unchanged passes with small/large source counts: zero transcript
  reads and no source enumeration per watcher event.

- Run focused tests before implementation. Implement the dispatch request
  without a second queue consumer. Never wait for the handoff while holding
  `syncMu`. Open watcher dispatch after local startup reconciliation, before
  starting the archive drain. Preserve callback error/retry ownership.

- Wire one manager into daemon initial, periodic and requested sync and direct
  CLI sync after local work. Named `sync --host` remains remote-only. Run the
  drain outside `SyncThenRun` and other pass-wide mutex holders. Reconcile
  pending targets using `ProviderStoredAgents`/`AgentDataVersion` and receiver
  policy independently of global resync markers. On a busy ordinary read,
  retain pending work and report unavailable; retry next normal sync.

- Add command tests for archive-only refusal, no configured attachment, named
  remote scope and offline browsing. Run
  `CGO_ENABLED=1 go test -tags fts5 ./internal/archivesource ./internal/sync ./cmd/agentsview -run 'ArchiveDrain|ArchiveSource|Watcher'`,
  focused race tests, formatting, vet and required lint. Commit.

### Task A5: Rebuild available archives and preserve offline history

**Files:** `internal/archivesource/rebuild.go`, tests,
`internal/sync/rebuild_contributor.go`, `engine.go`, rebuild tests,
`internal/db/orphaned.go`, and CLI/worker wiring.

**Interfaces:** Extend `sync.RebuildContributor` with:

```go
Run func(context.Context, *Engine, *db.DB, ProgressFunc) (SyncStats, error)
IDPrefixes []string
```

The coordinator supplies the contributor engine with the replacement database
and original archive reconciliation context. An archive `Run` uses A3 source
execution against that engine; existing filesystem contributors keep their
current path. `IDPrefixes` supplies original-session safety accounting for an
archive whose paths do not exist locally. Add
`func RebuildContributors(ctx context.Context, cfg config.Config) (sync.RebuildOptions, error)`.

- Write `TestAttachedArchiveRebuild`: trigger a full parser upgrade through the
  real worker, then a scoped provider upgrade. Assert exact IDs, curation,
  policy and pending fingerprints. Repeat offline and detached; reconnect
  after global markers clear and prove only pending sources are reparsed.
  Explicit full resync must reparse all available selected sources.

- Write `TestAttachedArchiveRebuildWaitsForBackup`. Complete one source, start a
  real backup before the next reader, observe waiting progress, then release
  the backup and prove the same replacement finishes without rereading the
  first source. Cancel a separate wait and compare the untouched live DB.
  Remove the pinned snapshot before retry; it must fail, not wait forever.

    ```go
    assert.Equal(t, 1, firstSourceReads)
    assert.Equal(t, expectedSessionIDs, rebuiltSessionIDs)
    assert.Equal(t, beforeCancellationDigest, liveDigestAfterCancellation)
    ```

- Run these tests before implementing the hook. Preserve coordinator safety
  counts and failure checks for custom contributors. Copy registration,
  provenance and completion with explicit columns; never use `SELECT *`. Keep
  old completion when copying unavailable/orphaned rows. Publish new
  completion only with the replacement swap.

- Prepare available contributors without a pass-wide repository lease. Initial
  unavailability permits carry-forward. After a contributor starts, retry only
  `docbank.ErrBackupRepositoryLocked` through the existing backoff dependency,
  bounded by rebuild cancellation rather than elapsed time. Report attachment,
  completed sources and elapsed wait; hold no SQLite transaction or reader
  while waiting. Reopen and revalidate placement on each attempt. Integrity,
  missing snapshot and other errors abort replacement.

- Wire daemon startup and the separate worker, including target policy and
  snapshot config. Suspend ordinary draining during the existing rebuild/write
  barrier and resume it after that lifecycle ends. Assert old history remains
  browsable and writes return 503 during rebuild/wait, then resume after
  success or cancellation cleanup. Run focused rebuild/worker and archive
  suites, formatting, vet and lint. Commit.

### Task A6: Prove the feature from a backup alone and document usage

**Files:** New `internal/archivesource/acceptance_test.go`,
`cmd/agentsview/archive_source_test.go`, and the owning user guides in the map.
Private real-data commands, identifiers and results stay outside Git.

**Interfaces:** Use production backup creation, D1, the real CLI/daemon, and
existing browse/export APIs. No new migration command or source-management API.

- Add a synthetic end-to-end test that creates a backup, makes capture and vault
  inputs inaccessible, attaches it to an ordinary installation with local
  sessions, syncs, browses and reparses. Assert transcript and machine
  identity, receiver telemetry/config unchanged, no permanent transcript tree,
  and unchanged-sync zero transcript reads. Include assets and curation.

    ```go
    assert.Equal(t, expectedMessages, browsedMessages)
    assert.Equal(t, receiverIdentityBefore, receiverIdentityAfter)
    assert.Zero(t, unchangedPassTranscriptReads)
    ```

- Exercise artifact/HTTP imports in both orders: separate namespaces may
  coexist; exact-ID conflicts fail unchanged. Verify raw-sync and artifact
  export do not publish attached originals. Exercise configured parsed-data
  mirror paths with original attribution and receiver policy intact.

- Run the relevant package suites and race tests, repository `make vet` and
  `make lint-ci`. Run the standard broader suite once the connected paths
  pass. Resolve failures before the real-data rehearsal.

- Rehearse on a private disposable copy of the preserved backup, using a
  worktree binary and isolated ordinary data directory. Record exact binary,
  repository UUID and snapshot ID privately. Make original capture/restored
  vault paths unavailable. First copy and independently hash the saved
  application database and a retained original larger than 4 GiB through D1;
  remove those temporary files after comparison. In a separate disposable
  repository, also capture and read a generated `4 GiB + 1 byte` host extra,
  streaming generation and comparison. Keep this large check out of default
  unit tests. Attach, sync and compare supported-source coverage, transcripts
  and machine attribution against the preserved evidence. Browse
  representative large sessions through the API.

- Repeat unchanged sync, restart, repository relocation, disconnection,
  reconnect, selective/scoped reparse and a full rebuild. Measure first/warm
  duration, peak disk, live heap and network-share reads. Include a concurrent
  local update and backup. Check temporary cleanup and repository verification
  afterward. Do not treat synthetic success as real-data acceptance.

- Document the three-line `[[archive_sources]]` configuration and existing
  `agentsview sync` workflow. Explain reader releases, temporary bootstrap
  space, offline browsing, delayed local updates during one large parse,
  rebuild write pauses, duplicate namespaces and capture-time deletions. State
  unsupported originals separately. Update the spec's implementation status
  only after these checks pass. Commit docs/tests and prepare a concise
  user-facing PR; do not merge or replace the user's installed binary.

## Plan self-review

The bootstrap/schema requirements map to A1; identity and atomic writes to
A2/A3; lease integrity to K1/K2/D1/A3; version/policy freshness and local
priority to A4; replacement publication and offline history to A5; all transport
boundaries and the repository-only rehearsal to A6. The five review-focus cases
have explicit owners and observable assertions. No new backup format, live vault
dependency, duplicate-merging policy or polling service is introduced.
