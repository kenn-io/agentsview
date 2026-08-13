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
results are the exception: the cursor tracks pending tool calls (bounded), so
a `function_call_output` / `custom_tool_call_output` that refers to a call
committed in an earlier batch is applied as an idempotent point update
(`ToolCallResultUpdates`) instead of a full reparse. Agent-scoped and unknown
calls still fall back. Safe incremental writes preserve the index-folded mtime
and lifecycle-derived termination status alongside message and token
aggregates.

## Append-only limitation

Cursor correctness assumes that growth is append-only. A same-inode file can
grow after bytes inside its already-committed prefix have been rewritten. Size,
identity, and boundary checks cannot prove the prefix was never modified.

Persisted checkpoints close most of the gap under the documented append-trust
mode:

- The checkpoint stores a 128 KiB tail anchor of the committed prefix, the
  file identity, the committed offset, the parser cursor, and a resumable
  SHA-256 state over the committed prefix.
- An append is only resumed when the identity matches, the size only grew,
  and the current bytes at the anchor region match the stored anchor; the
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
sync is O(d): the engine resumes the SHA-256 state over only the appended
bytes, verifies the 128 KiB tail anchor by digest, and parses only the new
tail. The append path reads the source roughly three times the delta
(fingerprint resume, parser tail, final checkpoint resume) plus the 128 KiB
anchor — for a 1 KiB append that is well inside the 256 KiB source-read gate.
An unchanged checkpointed source costs a stat plus the small checkpoint
metadata row read (the cursor and hash-state blobs live in a separate table
and are never loaded on the stat-only path) and reads 0 transcript bytes.
A full parse captures the resumable hash state and anchor digest on its own
read pass, so persisting the checkpoint adds no second source read. Without
a checkpoint (legacy sessions, first sync after upgrade) the previous
O(file) fingerprint and prefix-hash reads still apply until the next full
parse persists one.

The daily archive audit (`sync_worker` audit mode) bypasses the checkpoint
stat-trust gate so the provider's full-source fingerprint verifies content and
repairs same-stat in-place rewrites that append-trust would otherwise keep
stale.

- `BenchmarkCodexIncrementalCursor` in `internal/parser` compares cold prefix
  reconstruction with the exact warm cursor. It is diagnostic because
  `internal/parser` is not in `BENCH_GATE_PACKAGES`.
- `BenchmarkCodexCheckpointAppendResume` in `internal/sync` measures the
  checkpoint-resumed append's source pipeline (checkpoint gate, seeded tail
  parse, resume hash, next anchor digest, checkpoint assembly), bounded by
  the anchor window plus the tail. It is PR-gated because `internal/sync` is
  in `BENCH_GATE_PACKAGES`.
- `BenchmarkCodexQuietAppendSignals500/5000/15000` in `internal/sync` measure
  the non-amortized quiet-session append (call + late output) with full
  inline signal/secret maintenance; the three sizes gate the
  history-independent latency slope.
- `BenchmarkCodexLateToolOutputDebouncedBurst` in `internal/sync` measures a
  debounced stream where every appended batch carries the output for the
  previous batch's call: only the first iteration pays the O(history)
  signal recompute, so it guards per-append cost without pretending to be
  the quiet-session gate.

The maintained behavioral gate inventory is in
[Performance Gates](performance-gates.md).
