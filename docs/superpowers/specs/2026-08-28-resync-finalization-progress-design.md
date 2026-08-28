# Full Resync Finalization Progress

## Goal

Replace the stale 100% session counter shown during full-resync finalization
with status that names the work still in progress. Large archives can spend
minutes in this interval even though every discovered session has been counted.

## Observed Behavior

The file-sync progress counter reaches its total before `syncAllLocked` finishes
its epilogue. The epilogue can still commit the final write batch, persist
source state, link and repair subagent relationships, and release memory
retained by the bulk parse. Until that function returns, the resync coordinator
cannot emit its existing archived-session, metadata, classification, index, and
swap phases. The progress heartbeat therefore repeats the completed session
counter while unrelated finalization work continues.

## Design

Add a finalizing sync phase. Emit its events only for `syncWriteBulk` passes,
which are the local and contributor passes that build a replacement archive.
Ordinary full, cutoff, scoped, watcher, and reconciliation passes continue to
use their existing progress. Each contributor emits its own finalization
sequence through the contributor's existing progress wrapper. Finalization
events carry no session counters; the contributor wrapper must not add its
aggregate counters to this phase.

Emit progress immediately before each distinct file-sync epilogue operation:

- `Finalizing sync: committing session writes` before the terminal pending write
  batch is committed. Emit this only when the batch is non-empty, at the final
  `flushPending` call site rather than inside `flushPending`, so mid-loop
  pressure and batch-boundary flushes remain counted session work;
- `Finalizing sync: saving session source state` before remaining source
  baselines and exact ownership exceptions are persisted;
- `Finalizing sync: linking file-backed subagent sessions` before the
  archive-wide linking pass;
- `Finalizing sync: repairing subagent relationships` before queued parent
  repairs are applied; and
- `Finalizing sync: releasing parsed-session memory` immediately before the
  end-of-bulk-pass memory scavenge, but only when a scavenge is pending.

After the file-sync function returns, the resync can still inspect and sync
database-backed providers, run a second linking pass over all sessions, and
persist the skip cache. Emit these finalizing details before that tail work:

- `Finalizing sync: checking database-backed sessions` before the provider
  checks begin;
- `Finalizing sync: linking all subagent sessions` before the post-provider
  linking pass; and
- `Finalizing sync: saving the skip cache` before skip-cache persistence.

These events replace the counted in-place session line with an uncounted, named
step. The full-resync progress wrapper will continue to mark the events as
resync work. Every detail starts with `Finalizing sync:`, so it passes the
desktop startup filter's existing `ync` keyword check. Command-line,
daemon-stream, startup-state, frontend, and desktop consumers can therefore
render the detail without a new transport shape or consumer change.

Each finalization event also refreshes the progress `UpdatedAt` timestamp. A
finalization operation starts with a fresh stall timer rather than inheriting
the completed session counter's stale timestamp. A phase that emits no further
updates can still become stalled after the normal threshold.

Keep the later resync phases unchanged. The change does not alter write order,
transactions, cancellation, memory policy, database contents, or the database
swap.

## Error Handling

Progress reporting remains advisory. Existing operation failures keep their
current logging, statistics, and abort behavior. A finalization label describes
the operation that is about to run; it does not imply that the operation
completed successfully.

Cancellation paths retain the current cleanup and memory-release guarantees. The
memory status is emitted from the same deferred cleanup path that owns the
scavenge, so early returns cannot skip the cleanup. The explicit post-scavenge
tail events replace that memory status before the later database-backed and
persistence operations begin.

## Verification

Add a focused sync test that leaves one session in the final write batch, blocks
that batch through the existing write seam, and observes progress while the
write is blocked. The visible status must change from the completed session
counter to `Finalizing sync: committing session writes` before the blocking work
starts. Repeat the same setup with the default write mode and assert that
routine passes do not emit finalization events. This test fails if the stale
100% interval returns or the events leak into background sync status.

Add a focused bulk-retention test that blocks the injected memory scavenge and
observes `Finalizing sync: releasing parsed-session memory` before the scavenge
starts. Assert the ordered file-sync details with literal expected values so
removal, misordering, or accidental reuse of the session counter is detectable.

Exercise the post-scavenge tail and assert that checking database-backed
sessions, linking all subagent sessions, and saving the skip cache replace the
memory detail in order. Extend the desktop startup-status test with the
finalization labels to protect the existing filter contract.

Run the focused sync tests, then `go fmt ./...`, `go vet ./...`, and the
relevant broader Go test package before committing the implementation.
