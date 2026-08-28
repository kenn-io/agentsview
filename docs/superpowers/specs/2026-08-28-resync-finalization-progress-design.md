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

Add a finalizing sync phase and emit progress immediately before each distinct
epilogue operation:

- `Finalizing session writes` before the final pending write batch is committed;
- `Finalizing session source state` before remaining source baselines are
  persisted;
- `Linking subagent sessions` before the archive-wide linking pass;
- `Repairing subagent relationships` before queued parent repairs are applied;
  and
- `Releasing parsed-session memory` immediately before the end-of-bulk-pass
  memory scavenge, but only when a scavenge is pending.

These events replace the counted in-place session line with an uncounted, named
step. The full-resync progress wrapper will continue to mark the events as
resync work, and existing command-line, daemon-stream, startup-state, and
desktop consumers will render their detail without a new transport shape.

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
scavenge, so early returns cannot skip the cleanup or mislabel later work.

## Verification

Add a focused sync test that leaves one session in the final write batch, blocks
that batch through the existing write seam, and observes progress while the
write is blocked. The visible status must change from the completed session
counter to `Finalizing session writes` before the blocking work starts. This
test fails if the stale 100% interval returns.

Add a focused bulk-retention test that blocks the injected memory scavenge and
observes `Releasing parsed-session memory` before the scavenge starts. Assert
the ordered finalization details with literal expected values so removal,
misordering, or accidental reuse of the session counter is detectable.

Run the focused sync tests, then `go fmt ./...`, `go vet ./...`, and the
relevant broader Go test package before committing the implementation.
