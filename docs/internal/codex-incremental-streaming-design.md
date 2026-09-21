# Codex incremental checkpoints and streamed full imports

## Problem

A busy Codex rollout grows to hundreds of megabytes across thousands of
tool-result events. Three operations that should be cheap are not:

- an unchanged file costs a full transcript read on every sweep;
- a small appended tail re-reads and re-sums the whole transcript;
- a cold import holds the full message slice and every tool-result body in
  memory at once (a 945 MB archive peaked above 1 GB).

## What this branch does

1. **Persistent safe-resume checkpoints.** A full parse captures a resumable
   SHA-256 state, the trailing 128 KiB anchor digest, and the file identity in
   one pass. The checkpoint row commits in the same transaction as the session
   content. An unchanged file — inode, device, size, mtime, and change-time
   all matching — is skipped without reading the transcript. Eligible appends
   resume from the stored offset after checking the trailing anchor before the
   rows commit. The earlier prefix is trusted until a full audit.

1. **Cross-sync tool results as transactional deltas.** A tool output appended
   after its call was persisted no longer forces a full re-parse. The
   incremental tail yields deferred result updates that the writer applies
   with targeted probes: event deduplication against stored rows, a per-call
   agent-state table that resolves the latest content per agent by event
   coordinates (no content copies), and signals/findings folded incrementally.
   The checkpoint and incremental signal state commit with the content. Full
   writes seed that state in the same transaction, using the new stored
   revision without a follow-up read or commit. Successful and failed output
   checks are both reused by the aggregate calculations and state seed. Other
   providers retain debounced signal recomputation so streamed assistant
   messages do not pay for transactional signal-state maintenance.

1. **Streamed cold imports.** Decoding emits through a session sink; a staging
   sink writes event rows into a scratch SQLite database while the in-memory
   model keeps only placeholders. The publish transaction attaches the scratch
   database, copies event rows and per-call summaries into the archive, and
   commits messages, events, summaries, signals, and findings atomically.
   Single-event calls retain summary metadata during staging so publication
   does not read their payload again to derive an omitted summary. Disabling
   signal recomputation also skips staged signal and secret scans. Files above
   128 MiB take this path. Ephemeral engines use system temporary storage by
   default, preserving the fixed directory layout required for capture replay.

## Correctness boundaries

- The unchanged-file gate compares identity, size, mtime, and change-time
  without loading checkpoint blobs. The append gate allows timestamps to
  advance, checks the stored hash state against the committed prefix hash, and
  verifies the trailing 128 KiB anchor. It trusts earlier prefix bytes; a full
  audit (`ResyncAll`) provides authoritative revalidation. Detected
  truncation, replacement, and same-size rewrites trigger a full parse.
- Fork and subagent replays match the parent transcript's turn ids as opaque
  membership keys; an unresolved explicit parent keeps the child visible but
  marks its data version for retry.
- A checkpoint stores at most eight unresolved tool calls. A larger pending set
  stays in temporary parse state; a checkpoint becomes available again when
  that set shrinks. Without a checkpoint, an append reconstructs its prefix
  state and can still update the archive incrementally. Every completed full
  parse records its source hash, independently of checkpoint eligibility.
  Aborted turns retain pending calls because later outputs may still refer to
  them. Reused call IDs attach to the latest occurrence, as on main.
- Staged parsing preserves the caller's cancellation and immutable-source
  settings, including malformed trailing-record counts used by capture replay.
- A scratch write failure is sticky: the parse and the publish fail and the
  archive keeps its prior content. The staging ATTACH is torn down after every
  transaction, so consecutive publishes share one writer connection safely.

## Costs

- Disk: a summary identical to its sole result event is omitted from
  `tool_calls.result_content`; the event supplies the text when messages are
  loaded. Staged imports and late-result updates preserve this archive rule.
  Multi-event summaries remain stored. Checkpoints, signal state, and
  per-agent event coordinates add storage.
- Runtime: a staged cold import holds the process GC target lower for the
  duration of the parse and returns parse-phase arenas before the publish,
  trading CPU for lower transient memory. Messages and tool metadata remain in
  memory, so this does not establish a constant RSS bound.

## Suggested PR split

The branch is intentionally one working line of history, but the mergeable
sequence is:

1. persistent safe-resume checkpoints (0-byte no-op, O(delta) appends);
1. cross-sync tool-result deltas on top of it;
1. byte-bounded bulk admission;
1. the behavior-preserving session-sink parser refactor;
1. the scratch-staging streamed import with its runtime policies.

## Where to look

- `internal/parser/codex.go`, `codex_cursor.go`, `codex_provider.go` —
  single-pass hash/anchor, cursor codec, fork replay gate.
- `internal/sync/checkpoint.go`, `internal/db/checkpoint.go` — checkpoint
  persistence and the append/no-op decision.
- `internal/db/messages.go` — transactional late-result updates and the
  agent-state table.
- `internal/sync/codex_staging.go`, `internal/db/staged_content.go` — scratch
  staging sink and the staged publish transaction.
- `internal/signals/incremental.go` — the typed incremental reducer.
