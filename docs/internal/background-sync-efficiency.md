# Background Sync Efficiency

This document records the runtime and cost-model contracts that keep active
session writers from turning into unbounded background work. The implementation
lives primarily in `internal/sync/watcher.go`,
`internal/parser/codex_cursor.go`, and the provider incremental path in
`internal/sync/engine.go`.

## Watcher runtime contract

- Production watcher events use a 500 ms first-event batching window. Later
  events join the pending set without postponing its deadline.
- Watcher callback start times are at least five seconds apart. One worker runs
  callbacks serially while the fsnotify loop continues draining events and
  errors, so a long sync cannot block event intake or overlap another sync.
- An idle watcher has no running timer or ticker. The first relevant event
  creates the next one-shot timer.
- Each pending or in-flight batch retains at most 8,192 unique paths and 2 MiB
  of path-string bytes. At most one batch is in flight while one more
  accumulates. Entry count separately bounds map and slice overhead.
- Exceeding either batch limit replaces its individual paths with one explicit
  full-sync marker. The worker clears event-sensitive freshness caches and
  force-verifies every discovered file under the same serialization,
  dispatch-floor, cancellation, and shutdown rules, so overflow bounds memory
  without losing same-stat source changes.
- Shutdown discards pending paths and waits only for an already-running
  callback. Normal discovery on the next startup recovers discarded changes.

## HTTP mirror journal contract

Persistent HTTP mirrors use the same 8,192-entry and 2 MiB path-byte limits for
their crash-recovery journal. Crossing either limit collapses the path set to a
full-import marker instead of retaining unbounded path data. Unlike an in-memory
watch batch, the marker is durable and remains until database processing and
remote skip-cache persistence complete.

For a proven changed source, classification, fingerprinting, parsing, and
database writes are independent of unrelated mirror cardinality. An unproven
path may widen work only to its owning provider's configured roots. Full archive
transfers do not widen this import scope. Transfer selection requests changed
files unless every manifest file needs fetching or the server rejects a delta.

## Codex append cursor contract

The Codex provider factory owns one in-memory cursor cache shared by its
per-source provider instances. Its lifetime is the sync engine's lifetime; it is
not persisted across daemon restarts.

Each cursor is keyed by the cleaned physical path, exact safe byte offset, and
inode/device identity where the platform exposes them. The cache is an LRU
bounded to 256 entries and 2 MiB of estimated retained data. It contains compact
continuation state only: never parsed messages, raw JSON lines, complete prompt
bodies, file contents, or open file descriptors.

The database's committed source offset is the cursor commit token. A parse may
stage old- and new-offset entries, but only an exact offset from the next
database request is eligible. A failed database write therefore retries from the
old cursor; an unreachable staged entry is eventually evicted.

Every nonzero resume offset must immediately follow a newline. Incremental
parsing commits only complete, valid, newline-terminated JSONL records. Partial
records and valid JSON at a newline-less EOF are retried or force a full parse;
they are never published as safe cursor boundaries.

Truncation, known file-identity replacement, manual or project refreshes,
`session_index.jsonl` title changes, and records that retroactively update
stored messages all fall back to an authoritative full replacement. Late tool
results are the exception: the cursor tracks pending tool calls (bounded), so a
`function_call_output` / `custom_tool_call_output` that refers to a call
committed in an earlier batch is applied as an idempotent point update
(`ToolCallResultUpdates`) instead of a full reparse. Agent-scoped and unknown
calls still fall back. Safe incremental writes preserve the index-folded mtime
and lifecycle-derived termination status alongside message and token aggregates.

## Append-only limitation

Cursor correctness assumes that growth is append-only. A same-inode file can
grow after bytes inside its already-committed prefix have been rewritten. Size,
identity, and boundary checks cannot prove the prefix was never modified.

Persisted checkpoints close most of the gap under the documented append-trust
mode:

- The checkpoint stores a 128 KiB tail anchor of the committed prefix, the file
  identity, the committed offset, the parser cursor, and a resumable SHA-256
  state over the committed prefix.
- An append is only resumed when the identity matches, the size only grew, and
  the current bytes at the anchor region match the stored anchor; the
  full-file fingerprint is then derived by hashing only the appended bytes.
- An unchanged checkpointed source is skipped on stat alone (no transcript
  read). An anchor mismatch, identity change, truncation, or undecodable
  checkpoint forces an authoritative full parse and checkpoint rebuild.
- A same-size, same-mtime in-place rewrite that preserves the anchor region is
  trusted (append-trust). Periodic full audits (`ResyncAll`, `--full`,
  force-reverification passes) still hash the whole source and repair such
  rewrites. Strict verification remains available by bypassing the checkpoint
  gate.

## Cost model and regression evidence

A warm Codex cursor makes continuation-state parsing scale with appended records
rather than transcript history. With a persisted checkpoint, end-to-end append
sync is O(d): the engine resumes the SHA-256 state over only the appended bytes,
verifies the 128 KiB tail anchor by digest, and parses only the new tail. The
append path reads the source roughly three times the delta (fingerprint resume,
parser tail, final checkpoint resume) plus the 128 KiB anchor. This is a
read-cost estimate, not an enforced source-read limit. An unchanged checkpointed
source costs a stat plus the small checkpoint metadata row read (the cursor and
hash-state blobs live in a separate table and are never loaded on the stat-only
path) and reads 0 transcript bytes. A full parse captures the resumable hash
state and anchor digest on its own read pass, so persisting the checkpoint adds
no second source read. Without a checkpoint (legacy sessions, first sync after
upgrade, or more than eight unresolved calls) the O(file) fingerprint and prefix
reconstruction still apply. Such sessions can append without replacing their
stored messages; checkpoint eligibility is independent of whether the completed
parse records a source hash.

The daily archive audit (`sync_worker` audit mode) bypasses the checkpoint
stat-trust gate so the provider's full-source fingerprint verifies content and
repairs same-stat in-place rewrites that append-trust would otherwise keep
stale.

- `BenchmarkCodexIncrementalCursor` in `internal/parser` compares cold prefix
  reconstruction with the exact warm cursor. It is diagnostic because
  `internal/parser` is not in `BENCH_GATE_PACKAGES`.
- `BenchmarkCodexCheckpointAppendResume` in `internal/sync` measures the
  checkpoint-resumed append's source pipeline (checkpoint gate, seeded tail
  parse, resume hash, next anchor digest, checkpoint assembly), bounded by the
  anchor window plus the tail. It is PR-gated because `internal/sync` is in
  `BENCH_GATE_PACKAGES`.
- `BenchmarkCodexQuietAppendSignals500/5000/15000` in `internal/sync` measure
  the non-amortized quiet-session append (call + late output) with full inline
  signal/secret maintenance; the three sizes gate the history-independent
  latency slope.
- `BenchmarkCodexLateToolOutputDebouncedBurst` in `internal/sync` measures a
  debounced stream where every appended batch carries the output for the
  previous batch's call: only the first iteration pays the O(history) signal
  recompute, so it guards per-append cost without pretending to be the
  quiet-session gate.

The maintained behavioral gate inventory is in
[Performance Gates](performance-gates.md).

## Scoped PostgreSQL push batches

`agentsview pg push --watch` preserves the watcher batch that triggered a push
instead of discarding it. Before this, a daemon-delegated push always ran an
unscoped sync first, rediscovering every configured session provider and walking
the existing archive even when only a few sessions changed.

- `WatchBatchAccumulator` (`internal/sync/watch_batch_accumulator.go`) coalesces
  public watcher work — paths, renames, reconciliation roots, `LostEvents`,
  and `FullSync` — under the same entry-count and byte bounds as the watcher
  itself. Backend lifecycle tokens never enter the accumulated or transported
  batch. Exceeding a bound promotes the pending batch to `FullSync` with
  `LostEvents: true`, logged with a `WatchBatchPromotionReason` (`entry_limit`
  or `byte_limit`) and an aggregate count, never individual paths.
- The push loop (`cmd/agentsview/pg_watch_loop.go`) owns one queue of
  `queuedPushTask` values. A watcher-triggered task carries bounded scope; an
  interval or dirty-notification task is unscoped and absorbs older queued
  watcher scope (and its waiters) so a periodic catch-up scan cannot be
  starved. Watcher work that arrives after an unscoped task is claimed stays
  queued and runs afterward. The loop is the sole retry owner: a failed task
  returns to the head of the queue with the same scope, and waiters complete
  only after eventual success, cancellation, or final shutdown.
- `sync.ApplyWatchBatch` and `Engine.SyncWatchBatchThenRun`
  (`internal/sync/watch_batch_sync.go`) apply one bounded batch and run the
  push under one engine lock. For ordinary file batches, only the changed
  sources are classified, parsed, and committed; full sync, unresolved lost
  events, overflow, renames, a stale archive, or an explicit full push retain
  authoritative archive-scale reconciliation instead. Local push callbacks use
  this path directly; a daemon-delegated push transports the batch (and, for
  full/rename cases, a probed `WatchRecoveryScope` of available/deferred
  provider roots) so the daemon performs the same scoped operation.
- The daemon's push route accepts the batch as an optional field
  (`internal/server/huma_routes_push.go`). An older daemon that predates
  scoped batches is detected by its reported API version; a newer client
  retries the push without the optional fields rather than wedging watch mode.

[PostgreSQL Sync](../pg-sync.md#automatic-push-watcher) documents the resulting
user-visible behavior: ordinary changes carry a bounded path set, while
overflow, lost events, and ambiguous renames fall back to authoritative
reconciliation.

## Local session-provider disabling

`disabled_agents` (`internal/config/config.go`) excludes named session providers
from local filesystem ingestion only: discovery, targeted local file sync, watch
plans, and polling. The local sync engine (`internal/sync/engine.go`) omits a
disabled provider's factory once while building its provider-factory set;
watcher coverage and polling derive from that same selection. HTTP and SSH
remote import, and every export path (HTTP/SSH source export, PostgreSQL,
DuckDB, archive export), stay independent of this setting and are never passed a
disabled-provider list, so archived sessions from a disabled provider remain
visible, exportable, and unaffected by rebuilds. `RemoteSyncExcluded` remains
the separate, permanent capability that keeps unsafe source trees out of remote
export; disabling a provider does not change it.

A change saved through the Settings API applies to the running daemon's file
watching and polling immediately. A separate `pg push --watch`,
`clickhouse push --watch`, or `duckdb push --watch` process still needs a
restart to pick up the new selection, because it owns its own engine instance.
See
[Disabling Session Providers](../configuration.md#disabling-session-providers)
for the user-facing contract.

## Gemini zero-session fast path

Both slice-based (`geminiSourceSet.discoverRoot`) and streaming Gemini discovery
in `internal/parser/gemini_provider.go` enumerate session file paths under a
root before resolving project metadata, and return immediately when no session
file is found. An existing Gemini root, project metadata, or project directory
without a matching session file therefore costs no `projects.json` hash, no
missing-Git-root resolution, and no sibling-worktree scan. When sessions do
exist, the project map is still built once per root (or lazy discovery map) and
reused for every source in that pass; no persistent cross-sync cache is added,
since correct invalidation would span project metadata, repository layout, and
worktree changes.

## Adding a cardinality-scaling regression

When fixing a bug in this area, prefer a deterministic work-count assertion over
a wall-clock benchmark: compare the recorded work (`SyncStats`, `PhaseStats`, a
discovery call counter) at a small and a large unrelated cardinality and assert
it stays equal. See [Performance Gates](performance-gates.md) for the maintained
inventory and `internal/sync/watch_batch_sync_test.go` for the scoped-batch
cardinality tests (one changed source against one versus 10,000 unrelated stored
sessions).
