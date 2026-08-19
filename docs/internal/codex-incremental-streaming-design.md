# Codex incremental checkpoints and streamed full imports

## Problem

A busy Codex rollout grows to hundreds of megabytes across thousands of
tool-result events. Three operations that should be cheap are not:

- an unchanged file costs a full transcript read on every sweep;
- a small appended tail re-reads and re-sums the whole transcript;
- a cold import holds the full message slice and every tool-result body
  in memory at once (a 945 MB archive peaked above 1 GB).

## What this branch does

1. **Persistent safe-resume checkpoints.** A full parse captures a
   resumable SHA-256 state, the trailing 128 KiB anchor digest, and the
   file identity in one pass. The checkpoint row commits in the same
   transaction as the session content. An unchanged file — inode, device,
   size, mtime, and change-time all matching — is skipped without reading
   the transcript. Appends resume from the stored offset and are proven
   against the tail anchor before the rows commit.

2. **Cross-sync tool results as transactional deltas.** A tool output
   appended after its call was persisted no longer forces a full re-parse.
   The incremental tail yields deferred result updates that the writer
   applies with targeted probes: event deduplication against stored rows,
   a per-call agent-state table that resolves the latest content per agent
   by event coordinates (no content copies), and signals/findings folded
   incrementally. The checkpoint advances atomically with the write.

3. **Streamed cold imports.** Decoding emits through a session sink; a
   staging sink writes event rows into a scratch SQLite database while the
   in-memory model keeps only placeholders. The publish transaction
   attaches the scratch database, copies event rows and per-call summaries
   into the archive, and commits messages, events, summaries, signals, and
   findings atomically. Files above 128 MB take this path.

## Correctness boundaries

- The checkpoint is trusted only when the stored hash state matches the
  committed prefix hash and the file identity (including change-time)
  matches. Truncation, replacement, and same-size same-mtime rewrites
  rebuild authoritatively. A periodic audit (`ResyncAll`) remains the
  backstop for anything the stat gate cannot see.
- Fork and subagent replays match the parent transcript's turn ids as
  opaque membership keys; an unresolved explicit parent keeps the child
  visible but marks its data version for retry.
- A scratch write failure is sticky: the parse and the publish fail and
  the archive keeps its prior content. The staging ATTACH is torn down
  after every transaction, so consecutive publishes share one writer
  connection safely.

## Costs

- Disk: tool-result content is still stored twice by the existing
  archive layout (`tool_result_events.content` and
  `tool_calls.result_content`). This branch does not change that. The
  agent-state table adds only integer coordinate rows.
- Runtime: a staged cold import holds the process GC target lower for the
  duration of the parse and returns parse-phase arenas before the
  publish, trading CPU for a bounded RSS.

## Suggested PR split

The branch is intentionally one working line of history, but the
mergeable sequence is:

1. persistent safe-resume checkpoints (0-byte no-op, O(delta) appends);
2. cross-sync tool-result deltas on top of it;
3. byte-bounded bulk admission;
4. the behavior-preserving session-sink parser refactor;
5. the scratch-staging streamed import with its runtime policies.

## Where to look

- `internal/parser/codex.go`, `codex_cursor.go`, `codex_provider.go` —
  single-pass hash/anchor, cursor codec, fork replay gate.
- `internal/sync/checkpoint.go`, `internal/db/checkpoint.go` — checkpoint
  persistence and the append/no-op decision.
- `internal/db/messages.go` — transactional late-result updates and the
  agent-state table.
- `internal/sync/codex_staging.go`, `internal/db/staged_content.go` —
  scratch staging sink and the staged publish transaction.
- `internal/signals/incremental.go` — the typed incremental reducer.
