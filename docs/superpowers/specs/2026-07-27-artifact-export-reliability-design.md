# Artifact Export Reliability Design

## Status

Approved design for follow-up reliability work on the artifact publication
ledger.

## Problem

Artifact export currently has three related consistency gaps:

1. Publication changes accept any syntactically valid origin. A stale exporter
   can consume the global queue under a namespace that no longer matches the
   database's persisted `artifact_origin_id`.
1. Export reads all messages, tool calls, result events, and usage events before
   enforcing cardinality and decoded-size limits. The limits protect the wire
   format but do not bound memory used while constructing the source object
   graph.
1. One deterministically unexportable FIFO claim aborts its whole batch. The
   claim remains oldest and is retried forever, so unrelated later claims do
   not make progress.

These are one design problem: a claimed generation needs a bounded read,
origin-consistent publication decision, and a durable terminal outcome.

## Goals

- Prevent a claim from changing publication authority under any origin other
  than the database's persisted artifact origin.
- Bound export-side object construction before loading a complete session.
- Let successful claims publish even when another claim in the same batch is
  deterministically unexportable.
- Remove a rejected session's stale publication from the next checkpoint.
- Record the rejected generation and reason durably.
- Retry a rejected session automatically when a later session mutation advances
  its queue generation.
- Preserve generation guards across origin adoption, resync, concurrent writes,
  checkpoint creation, and acknowledgement.

## Non-goals

- Retrying transient database, context, or artifact-store failures within one
  export call.
- Adding an operator-facing rejection UI or CLI command in this change.
- Replacing the current immutable segment and manifest formats.
- Rewriting the exporter as a fully streaming wire encoder.

## Chosen Approach

Use a transactional bounded loader followed by capped materialization, and add
durable rejection state to the existing export queue.

This is deliberately smaller than a fully streaming exporter. The database first
examines a session through bounded pages inside one read snapshot, stopping as
soon as a cardinality or raw-byte budget is exceeded. Only a session proven to
fit those budgets is materialized through the existing message and usage models.
Existing canonical wire-size checks remain the final, exact format guard.

Deterministic failures become per-claim rejected outcomes. Other claims in the
same bounded batch continue. Successful and rejected publication changes are
checkpointed together, after which their exact generations are finalized
atomically.

## Consistency Invariants

1. A publication mutation is valid only when its origin equals the non-empty
   persisted `artifact_origin_id`.
1. Origin validation, claim-generation validation, and publication mutation
   occur under the same SQLite writer reservation.
1. No claim becomes clean until its resulting publication map has a durable
   checkpoint, or an unchanged recorded checkpoint has been verified.
1. A deterministic rejection deletes any publication for that session under the
   active origin.
1. Rejection state belongs to one queue generation. A later generation is new
   work and must be eligible for export.
1. Transient failures do not acknowledge, reject, or otherwise consume claims.
1. A stale claim cannot finalize either success or rejection after a concurrent
   mutation or origin adoption advances its generation.

## Origin Validation

`ApplyArtifactPublicationChanges` will read `artifact_origin_id` after acquiring
the existing artifact-publication writer reservation and before validating or
mutating claims.

- Missing or empty persisted origin: return an origin-mismatch error.
- Persisted origin differs from the requested origin: return an origin-mismatch
  error.
- Matching origin: continue with claim validation and publication changes.

The check remains inside the same transaction as the authoritative mutation. An
optional earlier read may avoid creating immutable dependencies for an obviously
stale exporter, but it is only an optimization; the locked check is the
correctness boundary. Immutable objects created before a failed locked check are
harmless unreferenced content and never become checkpoint authority.

## Bounded Session Loading

### API boundary

The database will expose an artifact-specific snapshot loader accepting a
database-owned limits struct. Keeping the struct in `internal/db` avoids an
import cycle while making every bound explicit:

- 32,768 messages per session;
- 32,768 usage events per manifest;
- 256 tool calls per message and 65,536 per session;
- 1,024 result events per tool call and 262,144 per session;
- 256 MiB of raw export-visible message and nested-collection bytes before
  materialization;
- 16 MiB of raw export-visible usage-event bytes before materialization.

These values match the existing artifact cardinality, session decoded-size, and
manifest decoded-size limits. The artifact package maps them to the
database-owned structure rather than defining a second policy. Test-only small
limits exercise every boundary.

### Read algorithm

The loader owns one read-only SQLite transaction so preflight and
materialization observe the same snapshot.

1. Read messages in bounded ordinal pages. Count rows and the byte lengths of
   export-visible scalar and JSON columns without retaining complete message
   objects.
1. Read tool calls and result events in bounded pages, enforcing per-parent and
   per-session cardinality while accumulating export-visible raw bytes.
1. Read usage events in bounded pages, enforcing count and raw-byte budgets.
1. Stop immediately when any count or byte budget is exceeded and return a typed
   deterministic limit error.
1. If preflight succeeds, materialize messages with nested collections and usage
   events from the same snapshot. Every materializing query retains a
   `limit + 1` defense so an implementation regression cannot silently bypass
   preflight.
1. Commit or roll back the read transaction before artifact-store I/O.

Paging keeps preflight memory bounded by a small page, not by session size. The
raw-byte budgets bound the object graph that may be materialized. Existing
canonical segment, session decoded-byte, manifest decoded-byte, and nested
collection checks remain authoritative for exact wire encoding, including JSON
escaping overhead.

The loader scans only rows belonging to the claimed session and stops at the
first exceeded limit. Work therefore scales with the configured cap, not with
the total archive or an arbitrarily oversized session.

## Failure Classification

Only failures that are deterministic for the claimed generation are rejectable:

- bounded-loader cardinality or raw-byte limit violations;
- existing artifact nested-collection limits;
- exact generated session, segment-reference, or manifest decoded-size limits;
- other explicitly typed validation failures that cannot succeed without a
  session mutation or a future format-limit change.

Context cancellation, SQLite errors, stale claims, origin mismatch, temporary
filesystem failures, and artifact-store errors remain fatal for the export call.
They leave every unfinalized claim pending.

Deterministic errors use a typed sentinel so the batch loop never classifies an
error by matching text.

## Durable Rejection State

Add non-destructive columns to `artifact_export_queue`:

- `rejected_generation INTEGER`;
- `last_error TEXT NOT NULL DEFAULT ''`;
- `rejected_at TEXT`.

Schema initialization and column migrations add the fields without rebuilding or
deleting the persistent archive. Resync preserves the columns as part of the
ledger schema, but clears their values when it advances copied generations for
mandatory re-export.

Every enqueue path advances `generation`, sets `pending = 1`, and clears stale
rejection fields. Thus a content mutation automatically retries a previously
rejected session. Existing FIFO timestamp behavior remains unchanged.

Checkpoint finalization accepts per-claim outcomes rather than only a list of
successful acknowledgements:

- success: mark the exact generation clean and clear rejection fields;
- rejection: mark the exact generation clean and store its generation, stable
  error text, and timestamp.

Finalization validates every generation before updating any row. Recording a new
checkpoint head and finalizing all outcomes remain one transaction. The
verified-unchanged-head path uses the same outcome finalizer without rewriting
the head.

## Batch Export Flow

For each selected claim:

1. Load the session metadata.
1. Treat missing, deleted, or foreign sessions as publication deletions.
1. Bounded-load export data for a live local session.
1. On success, create immutable dependencies and collect an upsert change.
1. On a deterministic failure, collect a deletion change and rejected outcome,
   then continue to the next claim.
1. On a transient failure, abort and leave all unfinalized claims pending.

After the loop, apply all successful upserts and deletion changes together.
Build the checkpoint from the resulting publication ledger, then finalize
successful, deleted, and rejected outcomes atomically with the checkpoint head.

`ExportResult` gains a rejected-session count. Rejections are not returned as a
fatal error because that would stop full-export pagination after a batch that
was durably handled. Durable queue state retains the detailed reasons.

If a rejected session had a prior publication, its deletion advances the
publication revision and the new checkpoint omits it. If it had no publication,
the verified-unchanged-head path can finalize the rejection without creating a
redundant checkpoint.

The full-export dependency-recovery pass remains fail-fast for sessions that
have no claim because it cannot safely consume unclaimed authority. Dirty
claimed sessions, including deterministic rejections, are handled by the bounded
queue-drain phases before and after recovery.

## Resync and Origin Adoption

Resync copies queue generation authority with the rest of the artifact ledger,
then advances every copied generation and forces it pending as it does today.
That new generation clears the prior rejection state. Rebuilt-only sessions are
still enqueued after origin restore; their fresh generation likewise has no
rejection state.

Origin adoption continues to requeue live sessions and target-origin publication
IDs. Requeueing clears rejection state because every adopted claim must be
evaluated under the newly active namespace.

## Testing

Tests use real SQLite databases and artifact stores where observable behavior
matters.

### Origin tests

- Applying changes under an origin different from persisted `artifact_origin_id`
  returns the typed mismatch error.
- No publication, revision, queue acknowledgement, or checkpoint-head state
  changes.
- Matching-origin behavior remains unchanged.

### Bounded loading tests

- Message, usage-event, tool-call, result-event, and raw-byte limits fail at
  `limit + 1`.
- Exact boundaries succeed.
- A large fixture proves preflight stops after bounded rows rather than loading
  the entire session graph.
- Existing segment and manifest exact-size tests remain green.

### Rejection and progress tests

- An oversized oldest claim is removed from publication authority, recorded as
  rejected, and marked clean only after checkpoint finalization.
- A valid later claim in the same batch publishes and is acknowledged.
- A transient store failure does not reject or acknowledge either claim.
- Mutating a rejected session advances its generation, clears stale rejection
  state, and makes it pending again.
- A stale rejection outcome cannot consume a newer generation.
- A crash/error before checkpoint finalization leaves the rejected claim
  pending.

### Lifecycle tests

- Resync advances copied and rebuilt generations, clears prior rejection state,
  and leaves every affected row pending.
- Divergent origin adoption clears rejection state and requeues the relevant
  claims.

## Trade-offs

- Preflight adds a second read pass for sessions that fit. Both passes are
  bounded by the same per-session caps, and avoiding unbounded allocation is
  worth the extra local SQLite work.
- Raw-byte preflight is a memory-safety bound, not a replacement for canonical
  encoded-size validation. Exact wire checks remain after materialization.
- Rejected sessions disappear from checkpoint authority until their content
  changes and exports successfully. This is preferable to serving stale data
  as if it represented the current archive.
- Rejection diagnostics are durable but intentionally have no UI in this change.
  A later status surface can read the recorded queue fields without a schema
  redesign.
- A future release that tightens export limits must include a migration that
  requeues existing publications. Otherwise the unclaimed full-export
  dependency-recovery pass remains intentionally fail-fast and cannot convert
  an old clean publication into a rejected claim.
