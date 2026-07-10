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
stored messages all fall back to an authoritative full replacement. Safe
incremental writes preserve the index-folded mtime and lifecycle-derived
termination status alongside message and token aggregates.

## Append-only limitation

Cursor correctness assumes that growth is append-only. A same-inode file can
grow after bytes inside its already-committed prefix have been rewritten. Size,
identity, and boundary checks do not detect that case, and the current
full-source fingerprint is not compared with a separately verified stored prefix
before incremental parsing. Closing this gap would require rolling hash state or
explicit prefix verification and remains deferred.

## Cost model and regression evidence

A warm Codex cursor makes continuation-state parsing scale with appended records
rather than transcript history. End-to-end append sync is still O(file): the
provider's `Fingerprint` hashes the complete source and the engine's
`ComputeFileHashPrefix` hashes through the newly committed offset.

- `BenchmarkCodexIncrementalCursor` in `internal/parser` compares cold prefix
  reconstruction with the exact warm cursor. It is diagnostic because
  `internal/parser` is not in `BENCH_GATE_PACKAGES`.
- `BenchmarkCodexIncrementalSyncReads` in `internal/sync` measures the warm tail
  between the two remaining linear reads. It is PR-gated because
  `internal/sync` is in `BENCH_GATE_PACKAGES`.

The maintained behavioral gate inventory is in
[Performance Gates](performance-gates.md).
