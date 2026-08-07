# Bun Sync Writer Unification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan directly in the current agent, task-by-task. Never use
> subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete the Bun storage convergence by routing ordinary sync,
incremental append, parse-diff repair, standalone accounting/findings writes,
and canonical orphan-copy projections through guarded Bun transactions.

**Approved spec/design:**
`docs/superpowers/specs/2026-08-02-unified-bun-storage-design.md`

**Architecture:** Reuse `SessionBatchWrite` and `writeOneSessionBatchTx` as the
single complete-session ingestion core. The SQLite adapter keeps archive-only
orchestration, but canonical rows are built once and written with
transaction-bound Bun helpers; the embedded `*sql.Tx` is retained only for FTS5,
pins, ATTACH/temp-table lifecycle, legacy probes, revision bookkeeping, and
other documented SQLite operations.

**Tech Stack:** Go, Uptrace Bun, SQLite/FTS5, Kata, `gh stack`.

## Global Constraints

- SQLite remains the persistent archive and must never be dropped, truncated, or
  recreated for a data-version change.
- No compatibility shim, dual writer, or fallback path may survive the cutover.
- Existing append, replacement, pin, FTS, recall, source-revival, artifact,
  signal-freshness, and rollback behavior must remain observable.
- Canonical writes use Bun placeholders and model-derived column lists.
- ATTACH, DETACH, temporary tables, legacy-schema probes, and FTS5 maintenance
  remain narrow SQLite adapter seams.
- Each new behavior test must fail for the expected reason before production
  code changes and assert persisted behavior rather than source shape.
- RoboRev remains snoozed during implementation and is resumed only after the
  sixth PR is verified and published.

______________________________________________________________________

### Task 1: Canonical message graph and scoped repair primitives (`1yq3`, `k919`)

**Files:**

- Modify: `internal/db/bun_write.go`
- Modify: `internal/db/messages_diff.go`
- Test: `internal/db/bun_write_test.go`
- Test: `internal/db/messages_diff_test.go`

**Interfaces:**

- Consumes: `CanonicalMessageRows`, `AppendMessageRows`, `AppendToolRows`, and a
  transaction-bound `bun.IDB`.

- Produces:

    ```go
    func appendCanonicalMessageGraph(
        ctx context.Context, tx bun.IDB, sessionID string, messages []Message,
    ) error

    func RepairMessageRows(
        ctx context.Context,
        tx bun.IDB,
        sessionID string,
        affectedOrdinals []int,
        messages []bunmodel.Message,
        calls []bunmodel.ToolCall,
        results []bunmodel.ToolResultEvent,
    ) error
    ```

- [x] Add a failing Bun writer test with two message ordinals where only one is
  repaired. Assert the untouched row ID/tool graph remains unchanged, the
  changed row ID remains stable, stale changed-row tools disappear, a unique
  result `event_index = 7` survives, and timestamps persist at canonical UTC
  microsecond precision.

- [x] Run
  `CGO_ENABLED=1 go test -tags fts5 ./internal/db -run 'Canonical.*Repair|DiffRepairsOnlyChangedOrdinals' -count=1`
  and confirm failure because `RepairMessageRows` does not exist.

- [x] Implement `appendCanonicalMessageGraph` by validating one session,
  converting once with `CanonicalMessageRows`, then calling
  `AppendMessageRows` and `AppendToolRows`.

- [x] Implement `RepairMessageRows` with message upserts on
  `(session_id, ordinal)`, conflict updates excluding `id` and key columns,
  Bun deletes limited to `affectedOrdinals`, and append of only the supplied
  tool graph. Reject message/tool rows outside the requested session or
  ordinal set.

- [x] Change `messageRowEqual` to compare canonical persisted projections so
  normalized timestamps and nullable optional tool fields do not cause
  perpetual parse-diff repairs.

- [x] Run the focused test and the existing
  `TestReplaceSessionMessagesDiffMatchesFullReplace` suite; confirm pass.

- [x] Commit the primitive and tests as
  `refactor(storage): add canonical message repair writes`.

### Task 2: Port archive message writers to Bun (`1yq3`, `k919`)

**Files:**

- Modify: `internal/db/messages.go`
- Modify: `internal/db/messages_diff.go`
- Test: `internal/db/messages_test.go`
- Test: `internal/db/db_test.go`
- Test: `internal/db/messages_diff_test.go`

**Interfaces:**

- Consumes: `beginBunWriteTx`, `appendCanonicalMessageGraph`, and
  `RepairMessageRows`.

- Produces: Bun-backed `InsertMessages`, `WriteSessionIncremental`,
  `replaceArchiveSessionMessages`, and `ReplaceSessionContent` without the raw
  message/tool insert helpers.

- [x] Add `TestWriteSessionIncrementalUsesCanonicalMessageGraph` with a
  nanosecond offset timestamp and a tool result whose unique event index is 7.
  Assert canonical persisted timestamps, retained event index, correct legacy
  `message_id`, one transcript revision bump, incremental marker, automation
  state, metadata update, and signal invalidation.

- [x] Run that test and confirm the current manual writer fails by preserving
  source timestamp text and rewriting the unique event index.

- [x] Convert each public writer to begin one `bun.Tx`; pass `bunTx` to
  canonical row helpers and `bunTx.Tx` to SQLite-only revision, FTS, pins,
  recall, links, signals, and artifact helpers.

- [x] Replace full-reinsert logic with the existing FTS/pin deletion sequence
  followed by `appendCanonicalMessageGraph`.

- [x] Replace diff writes with `RepairMessageRows`; never call `ReplaceToolRows`
  for a partial diff because it owns the complete session tool set.

- [x] Preserve multi-session `InsertMessages` behavior by grouping inputs in
  first-seen session order, applying one graph append per session, and running
  revision/automation/signal effects once per distinct session.

- [x] Delete `insertMessagesTx`, `nextMessageIDTx`, `messageInsertArgs`,
  `messageUpdateSetClause`, `insertToolCallsChunkTx`,
  `insertToolResultEventsChunkTx`, and their batching/placeholder constants
  after all callers are gone.

- [x] Run focused message, diff, pin, FTS, incremental rollback, recall, and
  artifact tests.

- [ ] Commit as `refactor(storage): route archive message writes through Bun`.

### Task 3: Converge ordinary and full sync on the session-batch core

**Files:**

- Modify: `internal/db/session_batch.go`
- Modify: `internal/db/bun_backend.go`
- Modify: `internal/sync/engine.go`
- Test: `internal/db/messages_test.go`
- Test: `internal/sync/engine_test.go`
- Test: `internal/sync/engine_integration_test.go`
- Benchmark: `internal/db/messages_bench_test.go`
- Benchmark: `internal/sync/engine_bench_test.go`

**Interfaces:**

- Consumes: `SessionBatchWrite` and `writeOneSessionBatchTx`.

- Produces one adapter method:

    ```go
    WriteSessionAtomic(
        SessionBatchWrite, ...func() error,
    ) (SessionBatchResult, error)
    ```

    implemented only as a one-item call into the existing atomic batch core.

- [ ] Add a late-failure test that seeds an existing session, transcript, usage,
  findings, signals, pin/recall evidence, source tombstone, artifact
  generation, and data version; inject failure after canonical dependent rows
  are attempted; assert every pre-write value survives.

- [ ] Run the focused test and confirm the current split ordinary-sync sequence
  exposes a partially committed state.

- [ ] Reuse the transactionally loaded stored transcript in
  `writeOneSessionBatchTx` to choose safe in-place diff versus full FTS
  replacement, preserving the O(changed rows) streaming-tail path.

- [ ] Add `WriteSessionAtomic` to `ArchiveWriteAdapter` and implement it as a
  one-element `writeArchiveSessionBatchAtomic` call.

- [ ] Factor one engine-side constructor that fills `SessionBatchWrite` with
  session, messages, usage, identity observation/snapshot, signals, findings,
  data version, and replacement mode.

- [ ] Route ordinary and explicit full sync through `WriteSessionAtomic`; remove
  their separate message, usage, signal, finding, data-version, and revival
  commits. Preserve failed-write stale marking and post-batch relationship
  repair queues.

- [ ] Retarget the streaming-tail benchmark to the production unified writer and
  assert work remains proportional to changed rows.

- [ ] Run focused ordinary/full sync, source-revival, marker, and benchmark
  coverage.

- [ ] Commit as
  `refactor(sync): write complete sessions atomically through Bun`.

### Task 4: Consolidate standalone usage and secret writers (`zj81`)

**Files:**

- Modify: `internal/db/usage_events.go`
- Modify: `internal/db/secret_findings.go`
- Modify: `internal/db/messages.go`
- Modify: `internal/db/session_batch.go`
- Test: `internal/db/usage_test.go`
- Test: `internal/db/secret_findings_test.go`
- Test: `internal/db/sessions_sync_marker_test.go`
- Test: `internal/db/artifact_publication_test.go`
- Test: `internal/db/session_content_test.go`

**Interfaces:**

- Consumes: `CanonicalUsageEventRows`, `ReplaceUsageEventRows`,
  `CanonicalSecretFindingRows`, and `ReplaceSecretFindingRows`.

- Produces private transaction helpers that add SQLite side effects around the
  canonical row writers.

- [ ] Retarget the usage replacement tests through the Bun-backed public path
  and add a canonical timestamp case; confirm the current manual writer keeps
  the noncanonical text.

- [ ] Retarget the secret replacement test through the Bun-backed public path.
  Pass findings with mismatched embedded session/rules values and assert the
  method arguments remain authoritative, including nil versus pointer-to-zero
  coordinates.

- [ ] Normalize copied input events/findings before canonical conversion.

- [ ] Begin `bun.Tx` in standalone methods, call canonical replace helpers, then
  use `bunTx.Tx` for session touch, summary columns, sync-marker advancement,
  and standalone usage artifact enqueue in the same transaction.

- [ ] Reuse the secret helper from message replacement, full content
  replacement, and session batch; remove the raw insert implementation and its
  obsolete boolean flag.

- [ ] Keep usage reads/fingerprints on the existing adapter timestamp-order
  expression; this task changes writes only.

- [ ] Run usage duplicate-rollback, exact-money, timestamp-order, sync-marker,
  artifact, secret reset, and content atomicity tests.

- [ ] Commit as
  `refactor(storage): unify accounting and finding writes through Bun`.

### Task 5: Derive attached orphan-copy projections from the registry (`wrc0`)

**Files:**

- Modify: `internal/db/bun_write.go`
- Modify: `internal/db/orphaned.go`
- Test: `internal/db/orphaned_test.go`
- Test: `internal/db/db_test.go`
- Test: `internal/db/secret_findings_test.go`

**Interfaces:**

- Consumes: a pinned `bun.Conn` with `old_db` attached and its `bun.Tx`.

- Produces a registry-derived `INSERT ... SELECT` helper:

    ```go
    func copyCanonicalRowsFromAttached(
        ctx context.Context,
        tx bun.IDB,
        model any,
        sourceTable string,
        idsTable string,
        excludedColumns ...string,
    ) error
    ```

    for direct natural-key child copies. Sessions, tool calls, pins, revision
    comparisons, provenance, and sanitization retain explicit transforms.

- [ ] Add an orphan-copy test with colliding source/destination child row IDs,
  canonical usage/findings/tool results, a preserved source finding
  `created_at`, file inode/device and termination status, and a searchable
  copied message token. Assert destination IDs are assigned, content survives,
  canonical session fields survive, and FTS finds the copied token.

- [ ] Run the test and confirm failure on the currently omitted session fields.

- [ ] Add a guarded Bun connection acquisition helper, perform ATTACH/DETACH and
  temp-table lifecycle on that connection, and begin the copy as `bun.Tx`.

- [ ] Derive direct-copy columns from `bunmodel.ModelColumns`, intersect with
  the attached source schema, exclude destination IDs, and execute the
  `INSERT ... SELECT` through `bun.IDB.NewRaw`.

- [ ] Use the helper for usage events, tool result events, and secret findings.
  Preserve finding `created_at`; do not use `CanonicalSecretFindingRows` for
  archive copy.

- [ ] Derive compatible message columns from the registry while still omitting
  `id`; extend the curated session copy with registry-owned fields that are
  safe to preserve, specifically `termination_status`, `file_inode`, and
  `file_device`.

- [ ] Keep explicit transforms for tool-call message-ID remapping, pins, session
  provenance, identity placeholders, transcript comparison, and sanitization.

- [ ] Add a plausible late-copy rollback regression using conflicting legacy
  usage dedup rows; assert no session/message/usage/finding copy survives.

- [ ] Run orphan, trash, resync, FTS, and legacy-schema tests.

- [ ] Commit as `refactor(storage): derive orphan copies from Bun models`.

### Task 6: Remove the old sync writer surface and verify (`a403`)

**Files:**

- Modify: `docs/agents/storage.md`
- Modify:
  `docs/superpowers/plans/2026-08-07-bun-storage-stack-landing-cleanup.md`
- Modify: this plan as checkboxes complete
- External: Kata `h381` and children; sixth stacked PR body.

**Interfaces:**

- Consumes: all completed canonical writer tasks.

- Produces a sixth stacked PR with no live hand-maintained canonical writer
  projection.

- [ ] Search production code for the removed raw writer helpers and for manual
  canonical INSERT column lists in the scoped files. Inspect each remaining
  hit and document why it is schema, FTS, connection-local, compatibility, or
  operational metadata SQL.

- [ ] Update the storage guide to state that attached archive copy may use
  model-derived `INSERT ... SELECT` through Bun while connection-local
  ATTACH/temp-table control remains adapter-owned.

- [ ] Run `go fmt ./...` and `go vet ./...`.

- [ ] Run focused `internal/db`, `internal/sync`, `internal/importer`, and
  `scripts` tests with `CGO_ENABLED=1` and `-tags fts5`.

- [ ] Run the streaming-tail and sync benchmarks with `-benchtime=1x`.

- [ ] Run the full DuckDB suite and dedicated PostgreSQL/activity integration
  suite to prove the new tip does not disturb lower-layer parity.

- [ ] Run `make test-short`; report unchanged macOS watcher delivery failures
  separately without weakening them.

- [ ] Commit the completed plan/docs, create a recovery ref, submit the sixth
  branch with `gh stack submit --auto --open --remote origin`, and replace the
  generated PR body with a summary-only description ending in
  `<sup>generated by a clanker</sup>`.

- [ ] Close each Kata child and `h381` with commit/test evidence.

- [ ] Resume RoboRev reminders on the sixth branch and any lower branch snoozed
  during investigation.

## Self-review

- Spec coverage: parser ingestion, ordinary/full sync, incremental append,
  parse-diff repair, standalone dependent writers, and orphan recovery are all
  assigned to executable tasks.
- Transaction coverage: canonical Bun rows and SQLite-only side effects share
  one `bun.Tx`/`*sql.Tx`; post-commit recall and debounced analysis remain
  outside only where their existing semantics require it.
- Scope exclusions: no schema migration, Store interface decomposition,
  generated domain converters, package split, or new compatibility layer.
- Test quality: regressions assert canonical persisted values, atomic rollback,
  stable IDs/pins/FTS, and bounded streaming behavior; no source-grep
  assertion is encoded as a test.
- Placeholder scan: no `TBD`, `TODO`, or unowned implementation step remains.
