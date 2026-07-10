# Background Sync Efficiency Design

## Summary

Agentsview currently does too much work while agents are actively writing
sessions. The filesystem watcher wakes every 500 milliseconds and may dispatch
separate syncs for paths that settle at slightly different times. A single path
written more frequently than that can instead starve until it has a quiet gap.
Codex compounds both patterns: its provider migration disabled incremental
append parsing, so every rollout append reparses the complete JSONL transcript
and force-replaces all stored messages.

This change limits watcher-driven syncs to one batch per five seconds and
restores safe Codex incremental parsing. A bounded, in-memory Codex cursor cache
avoids rescanning the already-parsed JSONL prefix to reconstruct continuation
state. The cache contains no messages and holds no open files.

## Goals

- Eliminate periodic watcher wakeups while the filesystem is idle.
- Dispatch no more than one watcher-driven sync in any five-second interval.
- Continue making progress every five seconds during sustained writes rather
  than waiting indefinitely for a quiet period.
- Restore Codex append parsing without regressing `session_index.jsonl` title
  refreshes, termination status, or retroactive message updates.
- Bound all new in-memory state by both entry count and estimated bytes.
- Preserve full-parse behavior as the correctness fallback.
- Demonstrate the reduction with behavioral tests and append benchmarks.

## Non-goals

- Building a generic cursor framework for every JSONL provider.
- Changing frontend polling or SSE behavior.
- Performing a repository-wide memory-cache audit.
- Persisting parser continuation state across daemon restarts.
- Weakening the existing full-content source fingerprint checks.
- Closing the existing same-inode rewrite-plus-growth detection gap for
  append-only files.

## Watcher scheduling

The watcher will separate the short batching delay from the global dispatch
floor:

- `watcherBatchDelay` remains 500 milliseconds. The first relevant filesystem
  event starts a one-shot timer for this delay so events from one write burst
  are collected together.
- `watcherSyncMinInterval` is five seconds. After a callback begins, another
  callback cannot begin until this interval has elapsed.
- Later events add their cleaned path to the pending set but do not continually
  move the timer. This is a throttle with batching, not a trailing-edge
  debounce.
- When the timer fires, it takes every unique pending path and invokes one sync.
  If new events accumulated while the previous sync ran, the next timer
  targets the later of the batching deadline and the dispatch-floor deadline.
- When no paths are pending, there is no timer or ticker. An idle daemon
  therefore has no watcher scheduling wakeups.

The fsnotify loop will not execute `onChange` directly. It hands ready batches
to one dedicated callback worker and continues draining filesystem events and
errors while that worker runs. Only one callback may be in flight. If another
batch becomes ready while it is busy, the loop retains those paths and sends
them after the callback completes and the dispatch floor permits it. This keeps
syncs serialized without allowing a long sync to block event intake.

This gives a short response to an isolated edit, a maximum callback frequency of
once per five seconds, and bounded latency under continuous writes.

The watcher continues to discard pending paths during shutdown. The next daemon
startup performs a normal discovery sync, so shutdown does not need to block on
one final watcher callback.

## Codex incremental parsing

### Provider capability

The Codex provider will advertise `IncrementalAppend` as supported and implement
`ParseIncremental` using its existing tail parser. The sync engine retains the
database-aware gates already used for Claude:

- exactly one stored session maps to the source path;
- the stored parser data version is current;
- the file identity has not changed;
- the current file is larger than the stored safe offset; and
- the source has not been truncated.

The full provider parse remains authoritative and continues to force message
replacement when incremental parsing is unsafe.

Enabling the capability must not route Codex through helpers that derive a
Claude session ID from the filename. The incremental adapter will pass the
database-selected session ID to the provider, and the existing
`providerSingleSessionFresh` fast path will remain explicitly Claude-only. Codex
continues using its UUID/path and composite-fingerprint freshness logic.

### Sidecar safety

Codex source freshness is composite: `session_index.jsonl` can change a session
title without changing the rollout transcript. This was the reason incremental
support was disabled during the provider migration.

The engine must decline incremental parsing when a changed Codex path is a
forced provider refresh or when the current index title differs from the stored
session name. This Codex-specific gate lives inside the incremental decision,
before the later DB-freshness skip. Declining carries `forceReplace` so that
skip cannot swallow a title refresh. This deliberately differs from Claude,
where a per-file `ForceParse` does not disable incremental append.

Such a Codex source proceeds through the existing full provider parse. An index
mtime change whose title is unchanged may still use an incremental transcript
append, and the incremental write stores the same index-folded effective mtime
as a full parse.

### Termination status parity

Codex task lifecycle records are authoritative for status: `task_complete` means
awaiting user input, while `task_started` and `turn_aborted` mean a tool call is
pending. The continuation seed and cursor will therefore retain the most recent
lifecycle marker. Cold prefix reconstruction observes the same records as a full
parse, and the tail parser updates the marker from appended records.

`IncrementalOutcome`, the engine's incremental update, and
`db.IncrementalSessionUpdate` will carry an optional authoritative termination
status. Codex supplies it from the updated marker; the incremental transaction
writes it alongside counts and offsets. A nil value preserves the existing
Claude behavior of clearing termination status until a later full parse. Tests
cover both `task_started` and `task_complete` transitions so Codex remains
behaviorally identical to today's full-parse path.

### Full-parse fallbacks

The existing Codex tail parser already recognizes appended records that can
modify earlier normalized rows, including late token counts, tool results,
subagent notifications, and wait/spawn lifecycle records. These cases continue
to return `IncrementalNeedsFullParse`; the engine then performs a full parse and
replaces stored messages.

Before any Codex incremental read from a nonzero stored offset, the provider
performs an O(1) boundary probe: the byte at `offset-1` must be `\n`. If not,
the offset may sit inside an incomplete record (or after a newline-less final
record), so the provider requests a force-replacing full parse. This is
deliberately conservative.

A full parse continues storing raw file size so the existing size-equality
freshness gates do not enter a reparse loop. It seeds a reusable cursor only
when EOF is empty or newline-terminated. A later watcher pass full-parses until
the file again ends at that safe boundary. Incremental writes advance the stored
offset and stage a cursor only through the last complete valid JSON record.

The full parser's line-reader loop will retain the final byte-boundary status
and derive the continuation seed from its completed builder. That lets it seed
the cache at safe EOF without rereading the prefix.

An incremental parse failure advances neither the persisted offset nor the
cache. A database-write failure leaves the persisted offset unchanged, so the
same bytes are retried on the next batch. A cursor staged at the proposed new
offset is not eligible until a later request presents that exact,
database-committed offset.

## In-memory cursor cache

### Ownership and lifetime

The sync engine constructs a Codex provider factory once, but creates provider
instances for individual files. The factory will own a shared cursor cache and
pass it to each provider instance. Its lifetime is therefore the engine's
lifetime, and a daemon restart naturally starts with an empty cache.

### Cursor contents

Each entry is keyed by the cleaned physical transcript path, file identity, and
safe byte offset. It records:

- the exact safe byte offset;
- file inode and device when the platform exposes them;
- the current model and working directory;
- prompt-replay deduplication flags and a SHA-256 digest of the first genuine
  user prompt rather than the prompt body;
- the fork replay gate; and
- the most recent task-lifecycle marker needed by the continuation parser.

It does not retain parsed messages, tool-call maps, raw JSON lines, file
contents, or open file descriptors.

On a cache hit, the provider initializes the tail parser directly from this
state. On a miss, it reconstructs the same state by scanning the prefix once,
then stores the resulting cursor. A miss or eviction changes performance only;
it cannot change parsed output while the source obeys the append-only invariant.

The existing builder compares complete first-prompt strings. To keep cursor
state bounded, full parsing, cold seed reconstruction, and warm tail parsing
will all use one digest-based comparison helper. This is a behavior-preserving
hot-path refactor, and warm/cold parity tests cover the special initial-content
extraction and replay-dedup cases.

After a successful full parse at a safe record boundary, the provider seeds a
cursor without a second prefix scan. After a successful incremental parse, it
stages another entry at the proposed new safe offset without deleting the
old-offset entry.

The database offset is the cursor commit token. The engine's next incremental
request contains the last offset committed by the database transaction, and the
cache returns only an exact path, identity, and offset match. If the write
succeeds, the next request selects the new entry. If it fails, the next request
still selects the old entry; the uncommitted new-offset entry is unreachable and
is eventually removed by LRU eviction. This avoids adding a provider
commit/rollback callback while making it impossible to skip uncommitted bytes.

### Validation and eviction

A cursor is usable only when its offset exactly matches the database request,
the offset passes the newline-boundary probe, and its file identity matches the
current source when both identities are known. Replacement, truncation, an
offset mismatch, or an explicit full-parse request discards or bypasses it.
Older and uncommitted cursor versions for one path count against the same cache
limits and are reclaimed normally by the global LRU.

Existing Codex rows have no stored identity until a post-change full parse
stamps them. Windows reports zero identity, so the identity gate remains
unavailable there. S3 sources stay on their materialized remote-sync path and do
not reuse local Codex cursors. In all three cases the remaining size, boundary,
fingerprint, and full-parse fallbacks remain authoritative.

The cache uses least-recently-used eviction with two hard limits:

- at most 256 entries; and
- at most 2 MiB of estimated entry data.

An individual entry larger than the byte budget is not cached. The estimate
includes retained string lengths and fixed cursor fields. These limits are large
enough for many concurrently active agents while preventing the cache from
becoming a new source of unbounded memory growth.

The provider continues computing the existing full source content fingerprint
before the incremental decision. That hash describes the current complete file;
the incremental path does not compare it with a separately hashed stored prefix,
so it does not close the known same-inode rewrite-plus-growth gap. Cursor parity
therefore depends on the provider's append-only source contract. Rolling hash
state or explicit prefix verification can be considered separately.

## Data flow

1. Filesystem events add paths to the watcher's pending set.
1. The one-shot scheduler sends one unique path batch to the serialized callback
   worker after the batching and global-floor deadlines allow it, while the
   fsnotify loop continues draining events.
1. `SyncPathsContext` classifies the paths and obtains each provider's source
   fingerprint.
1. For a growing Codex rollout, the engine verifies database, identity, and
   sidecar gates before calling `ParseIncremental`.
1. The provider resumes from a matching cached cursor or reconstructs the cursor
   from the stored prefix, verifies the record boundary, then parses only
   complete appended records and derives the current termination status.
1. A safe tail stages a cursor at the proposed offset and produces an
   incremental database write. Committing the write makes that exact-offset
   cursor eligible for the next append. An unsafe tail falls through to a full
   parse and message replacement.
1. The existing broadcaster emits after a successful sync. Fewer sync passes
   naturally produce fewer SSE notifications and status-driven API refreshes.

## Testing

### Watcher behavior

Behavioral watcher tests will assert that:

- a burst of different paths produces one callback containing unique paths;
- a second burst cannot dispatch before the five-second floor;
- pending paths dispatch at the floor without waiting for sustained writes to
  stop;
- later events do not postpone a scheduled throttled callback indefinitely;
- stopping with a pending timer returns cleanly and triggers no callback after
  shutdown;
- callback serialization remains intact when a sync runs longer than the
  scheduling interval; and
- filesystem events received during a blocked callback are retained and appear
  in the next serialized batch.

Tests will use short injected durations while asserting observable callback
timing and payloads. They will not inspect the source for timer implementation
details.

### Codex behavior

Provider and sync integration tests will assert that:

- Codex advertises incremental append support;
- a normal append writes only new messages, advances `next_ordinal`, and sets
  `last_write_incremental` without replacing existing message rows;
- for append-only growth, a warm cursor and a reconstructed cold cursor produce
  the same literal messages and metadata;
- index-title changes bypass incremental parsing and refresh the stored title;
- unchanged index metadata does not prevent a transcript append from using the
  incremental path;
- inode replacement, truncation, unsafe offsets, and partial trailing records
  fall back without losing data;
- `task_started` and `task_complete` appends update `termination_status` through
  the incremental transaction;
- retroactive token/tool/subagent records still full-parse and replace earlier
  rows correctly; and
- failed parses do not stage a cursor, and failed database writes leave the old
  stored offset selecting the old cursor.

Cache tests will exercise observable hit/miss and eviction behavior by looking
up old and new keys after exceeding each limit. They will not mirror private LRU
bookkeeping.

### Performance evidence

A benchmark will repeatedly append a small Codex tail to a large transcript. It
will report allocations and elapsed time for a cold cursor and a warm cursor.
The expected improvement is that warm continuation-state parsing scales with the
appended records rather than the number of prior JSONL records. End-to-end
source hashing remains linear in file bytes by design and will be visible in a
separate sync benchmark. That benchmark will account for both remaining linear
reads: provider full-file fingerprinting and the engine's post-parse
`ComputeFileHashPrefix` refresh through the committed offset.

Validation will run focused parser, sync, watcher, and database tests; the
watcher tests under the race detector; `go fmt ./...`; the normal Go test suite;
and `go vet ./...` before the implementation commit.

## Expected outcome

Idle watchers stop waking twice per second. Active agents produce at most one
watcher sync batch every five seconds. Normal Codex appends stop materializing
and rewriting the complete message history, while bounded cursors remove the
remaining repeated prefix-state parse for recently active sessions. Correctness
continues to favor full parsing whenever source identity, sidecar metadata, or
tail semantics make an append unsafe.
