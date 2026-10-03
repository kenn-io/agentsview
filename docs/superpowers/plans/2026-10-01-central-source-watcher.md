# Central source watcher implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development or superpowers:executing-plans to
> implement this plan task-by-task. Steps use checkbox syntax for tracking.
> This document does not authorize implementation, delegation, branch changes,
> pushing, or merging.

**Goal:** Centralize local source observation and recovery without allowing
watch exhaustion or persistent metadata to create unbounded background work.

**Architecture:** Providers declare physical coverage units and retain logical
source interpretation. A shared interest index, paged scanner, and bounded
disposable cache feed one coordinator per source consumer. Native events and
coverage scans use the same dispatcher; authoritative member audits remain
explicit where existing format cursors cannot detect edits and deletions.

**Tech stack:** Repository Go 1.27, standard filesystem/path packages, existing
SQLite driver, fsnotify 1.10.1, existing FSEvents bridge, testify, synctest.

**Spec:** [Central source watching design](../specs/2026-10-01-central-source-watcher-design.md).

## Global constraints

- SQLite is the persistent archive; do not change its schema for scanner state.
- Source formats, mirror schemas, archival policy, and daily audit are unchanged.
- No new dependency, brace-expansion language, native journal, or replay bridge.
- Provisional cache ceiling: 128 MiB per consumer; main file: 48 MiB; reserve: 80 MiB.
- Intern physical directory prefixes; never persist duplicate root/relative/parent paths per file.
- Qualify persistence benefits and compact representation before connecting a durable cache.
- Cache uses TRUNCATE journaling, incremental auto-vacuum, and no WAL.
- SQLite page cache: 8 MiB; metadata pages: 256 records and 2 MiB maximum.
- Retained path batches: 8,192 paths and 2 MiB; first-event window: 500 ms.
- Dispatch floor: five seconds; uncovered-unit interval: 30 seconds from completion.
- Safety sweep: one hour, jittered between 45 and 75 minutes.
- Incomplete member feeds retain 15-minute scoped audits from completion.
- Native directory targets: Linux min(65536, OS limit/2), Windows 1024.
- Failed Linux limit read uses 8192; roots and ancestors count against the target.
- Whole passive daemon: a few hundred MiB; added scanner working memory: at most 32 MiB in the 50,000-file gate.
- Keep the native intake pump separate from filesystem and Add/Remove work.
- Use CGO_ENABLED=1 and the fts5 tag for relevant Go checks.
- Before each Go commit run go fmt ./... and go vet ./... with the repository build environment.
- Preserve user changes; never bypass hooks, amend, or push without authorization.

## Review focus

1. Evicted cache entries must not become source removals; Task 5 checks preserved archive visibility and scoped completeness recovery.
2. Shared SQLite insert cursors can miss edits/deletions; Tasks 3 and 7 retain and exercise authoritative member audits.
3. Overlapping roots must route to every owner without duplicate logical writes; Tasks 2 and 6 check exact owner dispatch.
4. Changed plan inputs and live provider selection must invalidate stale cached interest; Tasks 3 and 8 check new roots and disabled providers.
5. Journal and temporary files can exceed a main-file page cap; Tasks 4 and 12 measure the complete storage envelope through failure and churn.

## Delivery stages and file responsibilities

The stages are dependent parts of one source-observation subsystem. They are not
independent features requiring separate architecture projects. Tasks 1-3 build
and qualify declarations before runtime cutover; Tasks 4-5 qualify cache and
scanner; Tasks 6-10 connect consumers and recovery; Tasks 11-12 qualify delivery.
Do not run old and new coordinators simultaneously.

| Files | Responsibility |
| --- | --- |
| `internal/pathpattern/pattern.go` | Segment matching and descent pruning |
| `internal/parser/source_coverage.go` | Provider declaration contract |
| `internal/parser/types.go` and existing provider/source-set files | Static declarations and bounded format-dependent root planning |
| `internal/watchplan/index.go` | Compile units, fingerprint plans, and route paths |
| `internal/watchscan/cache.go` | Bounded persistent observation cache |
| `internal/watchscan/scanner.go` | Paged filesystem observation and acknowledged snapshots |
| `internal/sync/watch_coordinator.go` | Scheduling, coalescing, fairness, and retries |
| `internal/sync/coverage_dispatch.go` | Engine routing and scoped verification |
| `internal/sync/watch_backend_limits_linux.go` and `watch_backend_limits_windows.go` | Platform allocation limits |
| Existing native backend files | Event intake and unit-owned lifecycle signals |
| `cmd/agentsview/source_watch.go` | Daemon/CLI assembly of the shared implementation |
| `internal/rawwatch/worker.go` | Raw capture consumer integration |
| `cmd/perfsim/watcher.go` | Isolated scale qualification |

Names and signatures below are new design decisions, not claims that those APIs
already exist. Existing APIs are cited where reused. Source locations refer to
the spec baseline; inspect the current tree before implementation.

### Task 1: Compile interest patterns

**Files:** Create `internal/pathpattern/pattern.go` and `pattern_test.go`.

**Interfaces:** Produce `Compile(patterns []string) (*Set, error)`,
`(*Set).MatchFile(rel string) bool`, and `(*Set).CanContain(relDir string) bool`.
Normalize native separators at the filesystem boundary, not by rewriting
backslashes inside the pattern language. Reject absolute paths, escaping `..`,
empty sets, invalid segment patterns, and malformed `**(N)`.

- [ ] Write `TestPatternsMatchAndPrune` with these literal cases:

  ```go
  assert.True(t, subagents.MatchFile("p/s/subagents/deep/agent-a.jsonl"))
  assert.False(t, subagents.MatchFile("p/s/subagents/deep/cache.bin"))
  assert.False(t, subagents.CanContain("p/s/scratch"))
  assert.False(t, bounded.MatchFile("a/b/c/d/e/.aider.chat.history.md"))
  ```

  Here subagents is compiled from `*/*/subagents/**/agent-*.jsonl` and
  bounded from `**(4)/.aider.chat.history.md`. Include zero-directory `**`,
  character classes, trailing separators, and malformed-depth inputs.
- [ ] Run `CGO_ENABLED=1 go test -tags fts5 ./internal/pathpattern -run TestPatterns`; require a missing implementation or behavioral failure.
- [ ] Implement segment-state traversal using `path.Match` for ordinary segments; keep the set immutable and do not add a custom regex language.
- [ ] Repeat that command; require PASS for matching and pruning assertions.
- [ ] Format/vet, stage only these files, and commit `feat(watch): compile physical source interest patterns` using the repository commit workflow.

### Task 2: Declare and index coverage units

**Files:** Create `internal/parser/source_coverage.go`,
`internal/watchplan/index.go`, and `index_test.go`; modify
`internal/parser/provider.go`, `source_set.go`, and `types.go`.

**Interfaces:** Declare the following shapes in parser:

```go
type CoverageInputKind uint8 // Files, SQLiteContainer, OtherContainer, PlanInput
type CoverageInputGroup struct {
    Name string
    Kind CoverageInputKind
    Patterns, CompanionPatterns []string
}
type SourceCoverage struct {
    Key, Path, ConfiguredRoot string
    Groups []CoverageInputGroup
    MemberAudit bool
}
type SourceCoverageProvider interface {
    CoveragePlan(context.Context) ([]SourceCoverage, error)
}
```

The named kinds are `CoverageFiles`, `CoverageSQLiteContainer`,
`CoverageOtherContainer`, and `CoveragePlanInput`. Add declarative
`AgentDef.SourceCoverage` templates for static layouts. ProviderBase resolves
templates against configured roots; dynamic providers implement the contract.
SourceSetProvider forwards the contract when its source set owns normalized
roots. A provider advertising local watch support without a declaration is an
installation error, not permission to silently stop watching.

In watchplan define `Unit{ID string; Agent parser.AgentType;
Coverage parser.SourceCoverage; Fingerprint string}`, `Match{UnitID, Group string;
Kind parser.CoverageInputKind}`, and `Compile(units []Unit, excludes []string)
(*Index, error)`. Produce `(*Index).Route(absPath string) []Match`,
`(*Index).Unit(id string) (Unit, bool)`, and `(*Index).CanContain(absDir string) bool`.
The ID derives from provider/configured-root/key; the fingerprint also includes
paths, groups, patterns, and exclusions. Do not persist device numbers.

- [ ] Write `TestIndexRoutesEveryOwner` using a parent root and nested root, plus two same-path providers. Assert the literal owner/group results, sibling prefix rejection, and no routing outside an excluded subtree. Assert an explicit root below an excluded ancestor remains usable.

  ```go
  assert.ElementsMatch(t, []string{"provider-a/files", "provider-b/files"}, owners)
  assert.Empty(t, index.Route(outsideSibling))
  assert.NotEqual(t, first.Fingerprint, changedPatterns.Fingerprint)
  ```

- [ ] Run `CGO_ENABLED=1 go test -tags fts5 ./internal/watchplan ./internal/parser -run 'TestIndex|TestCoverage'`; require the new behavior to fail before implementation.
- [ ] Implement declaration validation, stable identity, immutable index compilation, and all-owner routing. Retain the old runtime until migration and scanner tests pass; do not create a legacy-to-new declaration adapter.
- [ ] Repeat the command and require PASS. Check factory construction counts through the real index consumer, not constructor-field assertions.
- [ ] Format/vet and commit `feat(watch): index provider coverage declarations`.

### Task 3: Migrate provider layouts with behavioral evidence

**Files:** Modify `internal/parser/types.go`, `jsonl_source_set.go`,
`single_file_source_set.go`, `multi_session_container.go`, `db_backed_provider.go`,
`claude_provider.go`, `codex_provider.go`, `antigravity_provider.go`,
`antigravity_cli_provider.go`, `cowork_provider.go`, `hermes_provider.go`,
`crush_provider.go`, `visualstudio_copilot_provider.go`,
`sqlite_change_tracker.go`, and `source_set.go`. Create
`internal/parser/source_coverage_test.go`. Update
`docs/internal/session-format-sources.md` where format evidence changes.

**Interfaces:** Consume Task 2 declarations. Produce a validated plan for every
local provider, with explicitly empty plans only for import-only/remote inputs.
Static registry declarations cover providers without dynamic root logic; do
not guess narrower patterns from filenames alone. Per-provider root overrides
live in the existing format-owned provider file.

- [ ] Write `TestCoverageAdmitsDiscoveredInputs` against existing real-format fixtures, including companions and physical inputs of virtual sources. Compare hand-identified parsed inputs with index admission; do not generate expected paths with the matcher under test.

  ```go
  assert.True(t, admits("brain/session-a/.system_generated/logs/transcript.jsonl"))
  assert.False(t, admits("brain/session-a/scratch/generated.bin"))
  assert.True(t, admitsCodexIndex("session_index.jsonl"))
  assert.True(t, needsGooseMemberAudit)
  ```

  Add empty-at-start roots, newly created companions, Hermes profile markers,
  Crush registry changes, SQLite WAL data and SHM noise, Claude nested subagents,
  and Aider depth limits. Keep CodeBuddy's current broad admission until anchored
  layout evidence exists. Do not assert OMP's depth from the speculative appendix.
- [ ] Run `CGO_ENABLED=1 go test -tags fts5 ./internal/parser -run 'TestCoverage|TestAntigravity|TestGoose|TestHermes|TestCrush'`; require a declaration/parity failure.
- [ ] Implement static declarations in registry data and bounded dynamic planners in the listed provider files. Preserve currently parsed shapes, including alternate homes and out-of-root companions. Use explicit conservative patterns when narrowing lacks evidence; record that limit next to the declaration. Reuse `sqliteChangeTracker` decoding; declare member audits because it currently covers inserts, not edits/deletions.
- [ ] Repeat focused tests, then run `CGO_ENABLED=1 go test -tags fts5 ./internal/parser`; require PASS and fixture admission parity for every migrated family. Family migrations may use separate focused commits, each with its own fixture gate.
- [ ] Format/vet and commit the qualified declarations as `refactor(parser): describe physical source coverage`.

### Task 4: Implement the bounded disposable cache

**Files:** Create `internal/watchscan/cache.go`, `cache_test.go`, and
`types.go`. Reuse the existing SQLite driver; do not modify `internal/db/schema.sql`.

**Interfaces:** Define `FileIdentity{VolumeID uint64; FileID [16]byte; Known bool}`
and `Signature{Size, MtimeNS, ChangeTimeNS int64; Identity FileIdentity;
ChangeTimeKnown bool}`, `Entry{RelPath string; Directory bool; Signature Signature}`,
`DirectoryState{Signature Signature; Trusted bool}`, and
`CacheOptions{MaxBytes int64}`.
Produce `OpenCache(ctx context.Context, path string, options CacheOptions) (*Cache, error)`,
`(*Cache).Directory(ctx, unitID, fingerprint, relDir string)
(DirectoryState, bool, error)`, `(*Cache).Children(ctx, unitID, relDir, after string,
limit int) ([]Entry, string, error)`, `(*Cache).UpdateSignatures(ctx context.Context, unitID, fingerprint, relDir string,
entries []Entry) (bool, error)`, `(*Cache).BeginDirectory(ctx context.Context, unitID, fingerprint, relDir string,
state DirectoryState) (*DirectoryWrite, error)`, `(*DirectoryWrite).Append(ctx
context.Context, entries []Entry) error`, `(*DirectoryWrite).Commit(ctx
context.Context) (bool, error)`, `(*DirectoryWrite).Abort(ctx context.Context) error`,
`(*Cache).InvalidateUnit(ctx, unitID string) error`, and `(*Cache).Close() error`.
Commit returns false when admission is declined, without granting empty state.
New listing pages remain untrusted until Commit; crash-left provisional pages
are discarded on reopen. Append pages each obey the 256-record/2 MiB bound.
Keep old acknowledged pages readable until the new listing is committed, or
explicitly invalidate the directory when capacity cannot retain both.
Root parent is NULL; never encode the root as its own child.
Store each physical directory as a parent ID and basename, including metadata.
File rows reference directory IDs; overlapping providers share this inventory.
Directory-prefix records are covered by the cap and bounded eviction cleanup.
Full paths exist only in the bounded API page returned to the caller.
UpdateSignatures applies at most 256 existing file members/2 MiB after downstream
acknowledgement. It preserves listing membership and trust. Return false on
unknown directory, fingerprint mismatch, missing member, or declined admission;
do not insert new members or grant completeness. Row layouts update only changed
rows; coded layouts rewrite only affected pages. Directory membership changes
still use BeginDirectory/Commit. File IDs are opaque bytes, include all native
Windows bits, and are paired with volume identity. Unknown identity is explicit;
do not encode it as a matching zero identity.

- [ ] Run the [portable comparison](2026-10-01-source-watcher-mac-measurements.md) across platforms. The [returned Mac report](2026-10-01-source-watcher-mac-measurements.md#returned-mac-results) supplies three full APFS runs; the [returned Windows report](2026-10-01-source-watcher-windows-measurements.md#returned-windows-results) includes checked raw JSON and signature canaries, while Mac raw JSON remains outstanding. Start qualification with directory-ID/basename rows, which filled faster; compare prefix-coded pages where their additional size savings justify codec and replacement costs. Reject the duplicated full-path baseline. Then qualify both compact candidates with production generation metadata, sparse and nested trees, less repetitive basenames, seek-by-basename reads, and changed-page transactions using the repository driver. Measure durable reopen versus uncached startup before connecting persistence. The API works without durable storage if savings do not justify it.
- [ ] Write `TestCacheSelectiveAcknowledgedSignatures` and `TestCacheFullFileIdentity`. Exercise one changed signature in small/large inventories, untouched-record fidelity, unchanged listing generation, failed acknowledgement/reopen, unknown-member refusal, and file IDs with nonzero high 64 bits. Check the selected format modifies only its owned row/page, not the complete inventory.
- [ ] Write `TestCachePrefixStorageAndPageSeeking`. Verify byte-exact names, signatures, rename/move, overlapping-provider reuse, eviction of unused prefixes, and paged seeks across prefix boundaries. Require decoded pages to stay within 256 records/2 MiB; do not materialize full directory listings for seeks. Select the representation from the measurements rather than retaining both in production.
- [ ] Write `TestCacheAdmissionAndEvictionPreserveUnknownState`, `TestCacheReopenAndFingerprintChange`, and `TestCacheStorageEnvelope`. Fill through the actual cache API with long paths and changed listings; measure all cache-owned files during commits, evictions, rollback, and reopen.

  ```go
  assert.LessOrEqual(t, peakOwnedDiskBytes, int64(128<<20))
  assert.LessOrEqual(t, mainBytes, int64(48<<20))
  assert.False(t, evictedFound) // Unknown, not an empty trusted listing.
  assert.False(t, changedPlanFound)
  ```

- [ ] Run `CGO_ENABLED=1 go test -tags fts5 ./internal/watchscan -run TestCache`; require the cache's owned behavior to fail.
- [ ] Implement application ID 0x41565743, cache kind source-watch-cache, format version 1, plan fingerprints, indexed child paging, TRUNCATE journal, 48 MiB page cap, 8 MiB page cache, complete-directory eviction, and bounded incremental vacuum. Transactions admit at most 256 records/2 MiB. Preserve complete-listing atomicity with provisional pages and a committed listing generation; a directory larger than the available disk budget declines admission. SQLITE_FULL disables admission after bounded eviction attempts rather than spinning. Cache reset never touches unrecognized files or the archive.
- [ ] Repeat tests and qualify the 128 MiB total envelope with the repository driver. Do not treat journal_size_limit as a peak cap. If auxiliary-file bounds cannot be established, leave persistence disconnected and implement the same unknown-state API without a durable cache.
- [ ] Format/vet and commit `feat(watch): bound disposable scan cache storage`.

### Task 5: Scan physical inputs without acknowledging failed work

**Files:** Create `internal/watchscan/scanner.go`, `scanner_test.go`, and
platform-specific `signature_linux.go`, `signature_darwin.go`,
`signature_windows.go`, and `signature_other.go`.

**Interfaces:** Consume watchplan.Unit and Task 4 cache types. Define
`Change{Path string; Removed bool; Group string; VerifyContent bool}`, `ChangePage{UnitID string;
Changes []Change; CompletenessRoots []string}`, `ScanRequest{Roots []string; DirtyPaths []string;
Sweep bool}`, and `ScanStats{Listings, Entries, Stats, CacheHits, AdmissionRefusals
int64}`. Produce `NewScanner(cache *Cache) *Scanner` and
`(*Scanner).Scan(ctx context.Context, unit watchplan.Unit, request ScanRequest,
apply func(context.Context, ChangePage) error) (ScanStats, error)`.
A nil cache uses paged uncached enumeration. It never creates an unbounded
in-memory substitute. The scanner calls apply before committing observations. Call apply at every
bounded metadata work page, including an empty-change page, so unchanged full
scans also provide coordinator scheduling checkpoints. DirtyPaths is a bounded
set of explicitly notified physical paths, not a list of all root members.
Emit VerifyContent for owned native dirty paths even when a fresh signature is
unchanged. Persist changed signatures selectively after success; retain failed
verification intent for retry. Directory discovery cannot replace fresh
known-file checks on Windows.

- [ ] Write `TestScanCreateAppendRemove`, `TestScanFailureSurvivesReopen`, `TestScanUnreadableAndMissingRoot`, and `TestScanCacheMissRequestsCompleteness`. Use real scratch files and a specific apply seam that fails only the intended page. Assert literal delivered paths and successful archive visibility after retry through the real engine in the integration case.

  ```go
  assert.Equal(t, []string{"session-a.jsonl"}, deliveredRelativePaths)
  assert.Zero(t, unchanged.Listings)
  assert.NotEmpty(t, coldPage.CompletenessRoots)
  assert.False(t, incompleteRemovalDelivered)
  ```

  Exercise a same-tick directory mutation, identity replacement, removed
  directory, appends with unchanged parent mtime, and a listing over 2 MiB.
  Cached unchanged-listing assertions apply only after the two-second racy
  window; use a controllable clock/stat seam rather than sleeping.
- [ ] Write Windows scratch tests `TestScanOpenWriterAppend` and `TestScanNativeDirtySameSignature`. Keep a writer open, list before fresh individual queries, and verify the actual appended content is eventually stored. Exercise a controlled unchanged-signature dirty overwrite through the real owning engine, and lost-history recovery separately. Native query checks retain full IDs and actual change time; unsupported queries remain incomplete rather than zero-valued trusted proof. Test full-ID cache roundtrips independently of which IDs the scratch filesystem happens to allocate.
- [ ] Run `CGO_ENABLED=1 go test -tags fts5 ./internal/watchscan -run TestScan`; require observable delivery/acknowledgement failure.
- [ ] Implement pre/post listing signatures, two-second distrust, paged reads, post-apply commits, conservative partial-subtree handling, and SQLite-group physical probes using existing header logic. On Windows query known-file signatures from a fresh handle with FileBasicInfo,
FileStandardInfo, and FileIdInfo; avoid redundant per-field opens. Use attribute
read access with sharing for normal writers and deletion/rename, and close every
handle. Directory bulk queries serve discovery only unless freshness for the
specific use has been established. Unsupported change time/identity remains explicit. Eviction emits no removal. Cold/evicted scopes request coalesced completeness before absence claims.
- [ ] Repeat tests and run `CGO_ENABLED=1 go test -tags fts5 -race ./internal/watchscan`; require PASS with no leaked scan work after cancellation.
- [ ] Format/vet and commit `feat(watch): scan and acknowledge source observations`.

### Task 6: Route coordinator pages into existing engine semantics

**Files:** Create `internal/sync/coverage_dispatch.go` and
`coverage_dispatch_test.go`; modify `engine.go`, `watch_batch_sync.go`, and
`verified_source_gate.go` only at routing/trust boundaries.

**Interfaces:** Produce `(*Engine).SetCoverageIndex(index *watchplan.Index)` and
`(*Engine).ApplyCoveragePage(ctx context.Context, page watchscan.ChangePage) error`.
For plain changes reuse existing changed-path parsing/tombstoning and
`SyncWatchBatchThenRun`. For completeness use existing provider-issued scope
plans and `ReconcileProviderRootsGrouped`. Do not pass an untyped root list that
reconstructs every possible provider. Index selection occurs before provider
construction, stored-hint queries, and source interpretation.

- [ ] Write `TestCoverageDispatchRoutesBeforeConstruction` and `TestCoverageDispatchPreservesDeletionAuthority`, extending the real archive cardinality fixtures in `watch_batch_sync_test.go` and `stored_source_hints_test.go`.

  ```go
  assert.Equal(t, 1, matchingFactoryCalls)
  assert.Zero(t, unrelatedFactoryCalls)
  assert.Equal(t, "new message", messages[0].Content)
  assert.NotNil(t, retainedPersistentSession)
  ```

  Include both matching overlapping providers, deduplicated logical writes,
  aggregate-member deletion, and Claude/Codex cross-root moves. Assert stored
  output and visibility, not only the dispatcher call counts.
- [ ] Run `CGO_ENABLED=1 go test -tags fts5 ./internal/sync -run 'TestCoverageDispatch|TestSyncWatchBatchThenRun.*Cardinality|TestSyncWatchBatchThenRunMissing'`; require routing or persisted-output failures.
- [ ] Implement exact unit-owned dispatch. Keep canonical-source replacement, persistent archive, ExplicitDeletionOnly, and existing proof scopes. Honor VerifyContent through the owning engine's content verification and
verified-source trust boundaries, even for an equal metadata signature. Do not
invalidate unrelated providers. A completeness request does not itself supply proof. If existing scope resolution needs wider traversal, retain that cost explicitly rather than inventing a narrow proof.
- [ ] Repeat the command and require PASS at both archive cardinalities.
- [ ] Format/vet and commit `refactor(sync): dispatch changes by source coverage unit`.

### Task 7: Own scheduling and retries in one coordinator

**Files:** Create `internal/sync/watch_coordinator.go` and
`watch_coordinator_test.go`; modify `watcher.go` and
`watch_batch_accumulator.go` at their ownership boundaries.

**Interfaces:** Define `CoverageReason` with `CoverageNativeChange`,
`CoverageScan`, `CoverageSweep`, `CoverageLostHistory`, `CoverageMemberAudit`,
and `CoveragePlanChanged`. Define `CoverageWork{UnitID string; Reason
CoverageReason; Paths []string; Roots []string}`. Produce
`NewSourceCoordinator(index *watchplan.Index, scanner *watchscan.Scanner,
dispatch func(context.Context, watchscan.ChangePage) error) *SourceCoordinator`,
`(*SourceCoordinator).Run(ctx context.Context) error`,
`(*SourceCoordinator).Notify(work CoverageWork) error`, and
`(*SourceCoordinator).UpdatePlan(index *watchplan.Index) error`.
Scheduling uses an injectable clock seam in tests. Audit/verification execution
is bound to the owning consumer in assembly, not implemented by the scanner.
Run at most one background scan producer. Its apply callback sends one bounded
page and blocks for the coordinator's success acknowledgement before further
scan work or cache acknowledgement. Between pages the coordinator can service
pending scoped owners while that producer remains paused. No buffered whole-scan
output or concurrent archive dispatch is allowed. Cancellation unblocks and
joins the owned producer. Retain dirty work for the same owner until its content
verification succeeds; an earlier scan-page acknowledgement cannot clear it.

- [ ] Write `TestCoordinatorCompletionCadence`, `TestCoordinatorPendingWorkAndBackoff`, `TestCoordinatorOverflowKeepsOwners`, and `TestCoordinatorMemberAudit`. Use synctest or the existing deterministic clock style.

  ```go
  assert.Equal(t, 1, maximumConcurrentDispatches)
  assert.Equal(t, 30*time.Second, nextCoverage.Sub(previousCompletion))
  assert.ElementsMatch(t, []string{"unit-a", "unit-b"}, overflowOwners)
  assert.True(t, editedMemberUpdated)
  assert.True(t, removedMemberHandledByExistingPolicy)
  ```

  Exercise a pass longer than its interval, a failing page while another unit
  progresses, a safety sweep arriving during pending events, and native events
  during scans. Member tests use real Goose/container fixtures.
- [ ] Run `CGO_ENABLED=1 go test -tags fts5 ./internal/sync -run TestCoordinator`; require timing/ownership behavior to fail.
- [ ] Implement serial bounded dispatch, first-event deadlines, completion-based coverage scans, jittered sweeps, member-audit wakes, and per-unit retry ownership. Carry lifecycle acknowledgements through success. Preserve five-second spacing between logical passes, not scan pages, and existing bounded backoff; timers never erase work. Resolve group kinds through Task 2 rather than switches on agent names.
- [ ] Write `TestCoordinatorCoveragePagesYieldToPendingOwners`. Hold a large fallback scan across many pages, notify an unrelated unit, and require that owner to progress before fallback completion without overlapping archive dispatch or losing acknowledgements. Native dirty paths survive retry and coalescing even when metadata matches. Cancellation drains owned work; no unbounded page or stat queue is introduced.
- [ ] Repeat the command, then `CGO_ENABLED=1 go test -tags fts5 -race ./internal/sync -run 'TestCoordinator|TestCoverageDispatch'`; require PASS.
- [ ] Format/vet and commit `feat(sync): centralize source coverage scheduling`.

### Task 8: Connect daemon startup and runtime plan changes

**Files:** Create `cmd/agentsview/source_watch.go` and
`source_watch_test.go`; modify `main.go`, `sync_lifecycle.go`,
`unwatched_poll.go`, and `internal/config/config.go`.

**Interfaces:** Produce `startSourceWatch(ctx context.Context, cfg config.Config,
engine *sync.Engine) (*sync.SourceCoordinator, error)` in source_watch.go.
Assembly owns cache close and waits for Run termination on cancellation.
Add configuration keys `watch_cache_max_bytes` = 134217728,
`watch_scan_interval` = "30s", `watch_sweep_interval` = "1h", and
`watch_pool_max` = platform default. A zero cache budget disables persistence;
negative budgets and nonpositive intervals are invalid. The 48 MiB main cap
scales as 3/8 of a nonzero budget; the balance reserves auxiliaries.

- [ ] Write `TestSourceWatchStartupClosesEventGap`, `TestSourceWatchPlanInputReplansOwner`, and `TestSourceWatchProviderSelection`. Exercise a file written between startup scan and dispatch opening, registry-added roots, missing-root reappearance, and an already queued unit disabled during work.

  ```go
  assert.Equal(t, "gap write", storedMessage.Content)
  assert.Equal(t, 1, replannedProviderCount)
  assert.Zero(t, disabledProviderParses)
  assert.NotNil(t, previouslyArchivedDisabledSession)
  ```

- [ ] Run `CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview ./internal/config -run 'TestSourceWatch|TestWatchConfig'`; require startup/replan/validation failures.
- [ ] Install the coordinator after declaration parity is complete. Queue events during startup; preserve existing startup worker acknowledgement. Replace the warm-gap reconciliation with scanner/completeness requests. Retire file-unit degraded polling and coordinator-owned member timers; retain missing-root and non-file obligations only where not represented by coverage units. Live settings update the index and native desired plan together; active old work cannot acknowledge a new plan fingerprint.
- [ ] Repeat focused tests, then run `CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview ./internal/config`; require PASS and cancellation cleanup.
- [ ] Format/vet and commit `refactor(cli): assemble central source observation`.

### Task 9: Allocate native hints fairly within platform limits

**Files:** Create `internal/sync/watch_backend_limits_linux.go`,
`watch_backend_limits_windows.go`, `watch_backend_limits_other.go`, and
`watch_backend_limits_test.go`; modify `watch_backend.go` and
`watch_backend_fsnotify.go`.

**Interfaces:** Produce private `nativeWatchCapacity(configured int) int` in
platform files. Extend native root registration with unit identity and the
compiled interest index; preserve shared-path watch ownership. Coverage loss
notifies Task 7 with unit-owned scan work. Native admission uses the existing
watchOps seam and runs outside the event pump.

- [ ] Write `TestNativeAllocationSharesBudgetAcrossUnits`, `TestNativeAllocationCountsAncestors`, and `TestNativeExhaustionTransfersToScanner` with exact scripted Add results. Exercise more mandatory roots than capacity, a noisy first tree, and runtime ENOSPC.

  ```go
  assert.LessOrEqual(t, peakNativeWatches, 4)
  assert.ElementsMatch(t, []string{"unit-a", "unit-b"}, rootsReceivingCoverage)
  assert.Equal(t, "appended text", storedMessage.Content)
  assert.Zero(t, allProviderRecoveryCalls)
  ```

- [ ] Run `CGO_ENABLED=1 go test -tags fts5 ./internal/sync -run 'TestNativeAllocation|TestNativeExhaustion|TestFSNotifyBackendRemoveDuringBatch'`; require allocation or scanner-delivery failure.
- [ ] Implement Linux limit reading, platform defaults, round-robin unit allocation, prefix pruning, and ancestor/root accounting. Stop admission on exhaustion and schedule uncovered units. Do not implement LRU watch promotion, minimum residency, or global mtime ranking in this delivery.
- [ ] Repeat focused tests; build affected Linux/Windows paths using the repository's existing platform checks and verify Windows pump separation remains intact.
- [ ] Format/vet and commit `feat(watch): allocate native hints within platform limits`.

### Task 10: Recover lost history without widening provider ownership

**Files:** Modify `internal/sync/watch_backend_fsnotify.go`,
`watch_backend_factory_darwin.go`, `coverage_dispatch.go`,
`verified_source_gate.go`, `opencode_container_gate.go`, and
`watch_batch_sync.go`; create `internal/sync/coverage_recovery_test.go`.

**Interfaces:** Produce `(*Engine).VerifyCoverageUnit(ctx context.Context,
unit watchplan.Unit, roots []string) error` in coverage_dispatch.go.
The coordinator binds CoverageLostHistory to this method. Invalidate each
affected freshness gate by unit ownership, not by every configured provider.
Reuse existing source verification/parsing rather than adding a second parser.

- [ ] Write `TestLostHistoryRepairsSameStatSource`, `TestLostHistoryPreservesUnrelatedTrust`, and `TestDarwinDropRetainsValidStream`. Preserve the existing same-stat fixtures and add unit ownership to their expected output.

  ```go
  assert.Equal(t, "rewritten prompt", recoveredMessage.Content)
  assert.Zero(t, unrelatedFingerprintCalls)
  assert.Zero(t, validStreamCloseCalls)
  assert.Equal(t, 1, successfulLifecycleAcknowledgements)
  ```

  Test unknown-owner overflow, created-subtree truncation, native-queue overflow,
  missing roots, and a failed recovery before lifecycle acknowledgement.
- [ ] Run `CGO_ENABLED=1 go test -tags fts5 ./internal/sync -run 'TestLostHistory|TestVerifiedSourceGate|TestDarwinDrop'` on supported hosts; require output/trust failure before changing gates.
- [ ] Inventory skip/failure caches, verified-source records, SQLite trust, Codex indexes, and checkpoint trust. Scope invalidation where ownership exists; retain documented wider local verification when it does not. On Darwin keep valid streams and target paths/stream roots; retain RootChanged availability handling. Ordinary coverage loss does not use this verification path.
- [ ] Repeat Linux checks and macOS native checks on scratch data. If no Mac is available, leave native Darwin cutover unshipped; do not report platform recovery as verified.
- [ ] Format/vet and commit `fix(watch): recover event history within owned source scopes`.

### Task 11: Connect raw capture and remove replaced planning paths

**Files:** Modify `internal/rawwatch/worker.go`, `worker_test.go`,
`cmd/agentsview/raw_sync.go`, `internal/parser/provider.go`,
`source_set.go`, and `jsonl_source_set.go`.

**Interfaces:** Produce `(*rawwatch.Worker).HandleCoveragePage(ctx context.Context,
page watchscan.ChangePage) error`. Bind the same index/scanner/coordinator to raw
capture with its own cache under its existing data directory. The raw dispatcher
uses RawCaptureSourcesForChangedPath and existing capture/outbox semantics;
archive deletion callbacks are not reused.

- [ ] Write `TestRawCoverageRoutesAndRetainsBackpressure`. Use the existing real capture/outbox fixture, two providers, a companion-only change, a removal, and an outbox-full result.

  ```go
  assert.Equal(t, 1, matchingCapturedGenerations)
  assert.Zero(t, unrelatedProviderAudits)
  assert.Equal(t, expectedPhysicalBytes, capturedBytes)
  assert.True(t, failedPageDeliveredAgain)
  ```

- [ ] Run `CGO_ENABLED=1 go test -tags fts5 ./internal/rawwatch -run TestRawCoverage`; require capture or retry behavior to fail.
- [ ] Route raw pages directly and scope completeness audits to matching providers. Connect the shared coordinator assembly. Remove replaced WatchPlan/WatchRoots scheduling APIs and registry imperative watch fields once every consumer uses declarations. Preserve any independent format discovery API still consumed by manual imports. Do not add aliases or a dual runtime path.
- [ ] Repeat raw tests, then `CGO_ENABLED=1 go test -tags fts5 ./internal/parser ./internal/watchplan ./internal/watchscan ./internal/sync ./internal/rawwatch ./cmd/agentsview`. Review the diff and compile callers; do not write negative-existence tests for deleted symbols.
- [ ] Format/vet and commit `refactor(raw-sync): share central source observation`.

### Task 12: Qualify scale, storage, platform behavior, and documentation

**Files:** Create `cmd/perfsim/watcher.go`; modify `cmd/perfsim/main.go`,
`run.go`, `Makefile`, `docs/configuration.md`,
`docs/internal/background-sync-efficiency.md`, and
`docs/internal/performance-gates.md`. Create
`internal/sync/coverage_cardinality_test.go` and
`coverage_scanner_benchmark_test.go`.

**Interfaces:** Add scanner scenarios to perfsim's existing scenario selection.
Produce `BenchmarkScannerWarmNoop` and `BenchmarkScannerAppend` in internal/sync,
already a bench-gate package. Report listings, entries, stats, DB reads, parse
counts, retained bytes, native allocations, and all cache-owned file bytes.
Extend existing SyncStats diagnostics without new frontend flows.

- [ ] Write `TestCoverageCardinalityAndNoise` with 10 versus 5,000 archived sessions and the same changed batch. Include removed files, aggregate membership, and persistent containers. Add 16,000 irrelevant descendants beneath one excluded child; distinguish parent entry examination from subtree visits.

  ```go
  assert.Equal(t, small.ParsedSources, large.ParsedSources)
  assert.Equal(t, small.StoredHintRows, large.StoredHintRows)
  assert.Equal(t, int64(0), noop.TranscriptBytesRead)
  assert.Equal(t, int64(0), noise.DescendantVisits)
  assert.LessOrEqual(t, peakCacheDiskBytes, int64(128<<20))
  ```

  Limits apply to warm, cache-admitted input inventories. Cold/evicted scope
  completeness has a separate measured cost and must not be disguised as a
  constant-cost event path. An incomplete shared-container feed may enumerate
  members on its declared audit cadence; pin that scope and parse count.
- [ ] Run `CGO_ENABLED=1 go test -tags fts5 ./internal/sync -run TestCoverageCardinality`; require cost/output failures before qualifying the implementation.
- [ ] Add perfsim fixtures for 50,000 physical files, append, overflow, cache pressure, churn, and restart. Keep scratch data isolated. Test peak disk during transactions and rollback with the repository driver; verify the 32 MiB added working-memory gate and overall daemon memory with Go metrics and OS physical-memory measurements.
- [ ] Qualify the same 50,000-file scenarios under representative concurrent builds, containers, browser activity, and active agent work on ordinary developer Macs, Windows, and Linux. Repeat loaded runs and record workload context, completion and dispatch latency distributions, CPU time, retained/peak memory, retries, and backlog. Check completion-based scans do not overlap or build an unbounded queue. Do not shrink inventories or require idle hardware to satisfy gates; revise the design if the actual implementation fails its requirements. An idle baseline is supplementary.
- [ ] Use the [Windows supplemental evidence](2026-10-01-source-watcher-windows-measurements.md#returned-windows-results) to qualify full fresh signatures versus fixed owner-scoped batches in Go. Verify open-writer appends and same-stat notified/lost-history changes with listing-before-query order. Keep high-entropy filename fixtures for final schema size and selective writes. If serial stats fail latency gates, compare one/two bounded workers for CPU, allocations, cancellation, fairness, and memory before selecting a pool; do not assume Python speedups transfer or enable four workers by default.
- [ ] Repeat correctness tests, run `make bench-gate`, `make check-timing-budgets`, and the repository lint checks. Run macOS FSEvents integration and a multi-hour isolated APFS retention observation when access is provided. Report absent host access as unverified, not as a passing platform gate.
- [ ] Document defaults, cache eviction costs, scoped audits, and measured platform limits. Update the old configuration advice and background overflow contract. Record Linux/macOS results as observed versus estimated. No new changelog or screenshot work is needed for this backend change.
- [ ] Format/vet, scrub outgoing documents and fixtures, review the whole diff, and commit `test(watch): qualify centralized source observation at scale`.

## Self-review and execution boundary

Every spec section maps to a task: matching/routing 1-3, storage 4, scanning 5,
proof-preserving dispatch 6, scheduling 7, startup/replans 8, native allocation 9,
loss recovery 10, raw capture 11, and performance/docs 12. The five Review Focus
conditions have owning behavioral tests. Optional FSEvents replay and adaptive
watch eviction are deliberately excluded, not unresolved implementation tasks.

The Linux evidence shows why duplicate full paths are unsuitable: compact rows
reduce 50,000-file storage from 16.1 MiB to 5.6 MiB, and prefix-coded pages to
4.0 MiB. The ceiling remains provisional; qualify the final schema and restart
benefits before choosing persistent storage. Explicit admission control is still
required. These experiments do not verify production restart savings, the total
auxiliary-file bound, or native macOS watcher behavior. The returned APFS
metadata results match compact disk sizes and show short warm passes under
concurrent work. Windows raw results confirm those sizes but expose much slower
fresh individual checks and stale open-writer listing metadata. Known-file
freshness, full identity, bounded page fairness, and selective writes are explicit
qualification requirements. Neither platform selects durable caching or qualifies
sustained production load. Those are executable qualification
steps, not prerequisites for finishing this plan.

Recommended execution is native, in dependency order, with review at each
delivery stage. The shared declaration and acknowledgement contracts make
parallel implementation risky before those interfaces have passed their tests.
The user has requested planning and measurement, not product implementation.


## Returned Go diagnostic evidence

The [Go platform results](../../internal/watchprobe-platform-results.md) refine
Tasks 7, 9, 10, and 12. Preserve independent native wakeups, checkpoints between
pages, physical descriptor/handle measurement, and scoped recovery during
operation. The Mac's final correctness checks passed despite substantial loss
and shutdown-only recovery; include oldest pending-loss age in qualification.
Windows competing-load full scans remain seconds of work. Persistence needs
measured downstream savings, since metadata lookup alone added overhead on
all hosts. The tester's kqueue observations do not qualify FSEvents.
