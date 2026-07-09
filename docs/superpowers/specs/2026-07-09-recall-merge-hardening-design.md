# Recall merge hardening and research preview

- Date: 2026-07-09
- Branch context: `memory-substrate` (PR #1046)
- Status: approved design

## Summary

The recall substrate is close to mergeable, but its trust defaults, evidence
reconciliation, import idempotency, and experimental test coverage need a
focused hardening pass first. This design fixes those pre-release contracts,
adds a short public Zensical page that labels Recall as active research, and
updates the population roadmap from a primarily hand-labeled calibration corpus
to model-assisted evaluation.

The current branch remains a substrate change. It does not gain a model runner,
automatic write-through, or LongMemEval-v2 implementation. Those experiments
build on the merged substrate in later work.

## Goals

- Make every implicit or empty review state fail closed.
- Preserve idempotent imports even when the cited transcript has changed since
  the original import.
- Make contradictory trusted-status queries fail explicitly.
- Reconcile every stable evidence endpoint independently and report why trust
  was revoked.
- Reuse the existing source-UUID index and add the insertion invariant needed by
  those evidence checks.
- Batch query-exposure inserts without weakening durable query recording.
- Exercise the lab-only eval-ingest surface in CI.
- Publish a concise experimental Recall page that accurately describes current
  behavior and the rebuildable nature of the recall corpus.
- Reframe calibration around local model extraction and independent
  model-assisted labeling, with human review used for auditing and explicit
  promotion rather than primary corpus construction.

## Non-goals

- Building the local distillation runner.
- Implementing recall write-through or bulk population.
- Porting or reproducing LongMemEval-v2 on this branch.
- Adding semantic retrieval over recall entries.
- Adding PostgreSQL or DuckDB recall support.
- Optimizing the query ledger or evidence reconciler for a corpus that does not
  yet exist.
- Broad decomposition of existing large recall functions while their behavior is
  still changing.
- Adding recall-table migrations. These tables are new and unreleased, so their
  canonical schema is edited directly.

## Trust-state contract

Recall has four host-controlled review states:

| State             | Meaning                                                                     | Eligible for `trusted_only`          |
| ----------------- | --------------------------------------------------------------------------- | ------------------------------------ |
| `human_reviewed`  | Explicitly accepted by a human-controlled review surface                    | Yes, when all other trust gates pass |
| `unreviewed_auto` | Produced automatically without calibrated evidence                          | No                                   |
| `calibrated_auto` | Produced automatically by a configuration that cleared an evaluation policy | No in the current milestone          |
| `eval_raw`        | Raw evaluation material, not a durable recall claim                         | No                                   |

An empty review state normalizes to `unreviewed_auto`, and the schema default is
also `unreviewed_auto`. Human-reviewed import remains explicit: the reviewed
JSONL importer assigns `human_reviewed` after rejecting any caller-supplied
`review_state`. Eval ingest continues to assign `eval_raw`.

Automated evaluator labels never confer `human_reviewed`. They may support a
future decision to classify an extractor's output as `calibrated_auto`, but an
entry reaches `human_reviewed` only through an explicit human promotion action.
The current trusted predicate continues to require all of:

- `status = accepted`
- `review_state = human_reviewed`
- `transferable = true`
- `provenance_ok = true`

Because archived entries cannot satisfy that definition, a query that combines
`trusted_only` with an explicit status other than `accepted` returns a
validation error instead of an unexplained empty result.

## Import idempotency

The accepted JSONL importer treats an existing `candidate_id` as an immutable,
already-processed identity. Within both real and dry-run transactions, it checks
for that duplicate before validating the current session, evidence range, tool
uses, or supersession target. A duplicate is reported as skipped even if the
transcript has since been rewritten or removed.

Parsing, host-controlled-field rejection, candidate shape validation, and
same-stream duplicate handling still happen before the database transaction.
They protect the input contract and do not depend on mutable archive state.
Duplicate-first applies to archive-dependent validation only; it cannot mutate
or legitimize the already-stored entry.

## Evidence reconciliation

Evidence remains fail closed. Transcript changes never cause the host to
silently bless new content merely because source-message identities survived.

For each stored evidence window:

1. Resolve every nonempty endpoint `source_uuid` independently.
1. Revoke provenance if an anchored endpoint is missing or ambiguous.
1. Use the resolved ordinal for each anchored endpoint and the stored ordinal
   only for an endpoint that never had a source UUID.
1. Reject a reversed or otherwise invalid resolved range.
1. Rebuild the model-visible window and selected tool-use set.
1. Preserve provenance only when the recomputed content digest exactly matches
   the stored digest.
1. Update ordinals and endpoint metadata after a successful match.

Every transition from trusted to revoked logs the entry, source session, and
exactly one stable reason code:

| Reason                           | Condition                                                       |
| -------------------------------- | --------------------------------------------------------------- |
| `start_endpoint_unresolved`      | A nonempty start `source_uuid` is missing or ambiguous          |
| `end_endpoint_unresolved`        | A nonempty end `source_uuid` is missing or ambiguous            |
| `invalid_resolved_range`         | Resolved endpoints produce a negative or reversed range         |
| `window_invalid`                 | The resolved message window cannot be rebuilt                   |
| `selection_invalid`              | The stored tool-use selection cannot be rebound to the window   |
| `missing_digest`                 | Trusted evidence has no stored content digest                   |
| `content_digest_mismatch`        | Recomputed model-visible content differs from the stored digest |
| `evidence_dropped_during_resync` | Not all historical evidence rows survive full-resync copy       |

Logging is diagnostic; the durable state remains `provenance_ok = false` plus
the preserved historical evidence. Revocation is sticky: routine reconciliation
only examines entries whose provenance is currently valid and never changes
`provenance_ok` from false back to true, even if later parser output happens to
match the old digest again. Restoring trust requires a reviewed import with a
new candidate ID, optionally superseding the revoked entry; an explicit future
promotion workflow; or regeneration of the experimental recall corpus.

Reason selection is deterministic. During full resync, dropped evidence is
checked first; an entry revoked as `evidence_dropped_during_resync` is excluded
from later reconciliation. Otherwise evidence groups are processed in entry and
evidence-row order, and checks follow the table order from start endpoint
through content digest. The first failing check that changes an entry from
`provenance_ok = true` to false supplies its single log reason. Later failures
for another evidence group on the already-revoked entry do not emit additional
revocation logs.

Parser improvements can legitimately alter model-visible content and therefore
revoke previously trusted recall. The public docs make this behavior explicit
and instruct experimental users to treat the recall corpus as rebuildable. The
design deliberately does not re-stamp digests during resync, because doing so
would convert changed parser output into trusted evidence without review.

## Evidence invariants and index

The shared insertion gate rejects a recall entry when any evidence row names a
session other than the entry's `source_session_id`. Current import paths already
construct matching identities; enforcing the invariant at the database boundary
protects future population paths from creating an entry that survives or
cascades differently from its evidence.

SQLite already has the partial `idx_messages_source_uuid` index on
`messages(source_uuid)` for nonempty values. Evidence endpoint lookups add the
logically redundant `source_uuid != ''` predicate so SQLite can prove the
partial-index condition. An `EXPLAIN QUERY PLAN` check confirms that this
changes the lookup from a table scan to the existing index. Do not add a
composite `(session_id, source_uuid)` index unless later measurements against a
populated archive show that filtering same-UUID rows by session is material.

## Eval-ingest coverage

The `evalingest` endpoint remains excluded from normal binaries and available
only under its build tag. A dedicated Make target runs the relevant packages
with `fts5,evalingest`, and Linux CI invokes that target. This protects the
quarantine contract without exposing the endpoint in production builds.

Raw eval chunks are stored as `eval_raw`. Harnesses that inspect them must query
with `trusted_only=false`; trusted queries intentionally return no raw eval
rows. The public experimental page and PR description call out this inversion
from the older harness behavior.

## Public Zensical documentation

Add a short `docs/recall.md` page and a `Recall (Experimental)` navigation
entry. Add a compact `agentsview recall` section to the CLI reference that links
to the page rather than duplicating its details.

The page covers:

- Recall versus semantic transcript search.
- The current SQLite-only lexical read path, evidence windows, review states,
  query ledger, dry-run extraction, and reviewed JSONL import.
- The absence of automatic distillation, semantic recall-entry retrieval, web
  UI, and remote-store support.
- A prominent warning that Recall is active research and its schema, scoring,
  and workflows can change.
- A data-lifecycle warning: treat recall entries and measurement rows as a
  rebuildable corpus during this phase. The session archive remains the source
  of truth and must not be deleted to reset Recall.
- Trusted-only semantics and the `eval_raw` quarantine.
- The planned local-model extraction and independent model-assisted evaluation
  direction.
- The calibration privacy boundary: judging is local by default, while any
  remote frontier judge receives transcript-derived material only after an
  explicit per-run opt-in.

The page avoids presenting the current import-driven workflow as a stable or
recommended end-user feature.

## Model-assisted calibration direction

Update the existing recall population spec so hand-labeling a fixed corpus is
not the primary path or a merge requirement.

The revised Milestone 2 direction is:

1. Select deterministic real-session windows and demand-triggered windows from
   recorded misses.
1. Run one frozen, tools-disabled local extractor configuration over them.
1. Ask one or more independent local judge models to label correctness, semantic
   provenance support, scope, transferability, harmfulness, and candidate-pair
   duplication by default.
1. Optionally run a frontier judge only after an explicit per-calibration-run
   opt-in that identifies the endpoint and model and states that candidate
   text plus supporting transcript windows will leave the machine. There is no
   automatic remote fallback. Operators can restrict remote judging to
   synthetic or otherwise non-sensitive sessions.
1. Keep generator and judge model families separate when practical and record
   exact model IDs, prompts, schemas, decoding settings, and input digests.
1. Adjudicate judge disagreements automatically where possible and use small,
   sampled human audits to estimate judge error instead of requiring the user
   to construct the primary label corpus manually.
1. Report yield and abstention alongside the existing precision, provenance,
   harmfulness, transferability, and duplicate metrics.

Model labels are evaluation evidence, not trust promotion. They can justify a
later extractor-policy decision, but generated entries remain `unreviewed_auto`
or `calibrated_auto` and outside trusted recall.

The population spec's privacy invariant is amended narrowly to reflect this
choice: extraction and population stay local, local judging is the default, and
only an explicitly authorized calibration run may disclose its selected
candidate and evidence material to a named remote judge.

LongMemEval-v2 is a later, complementary benchmark. It evaluates whether the
populated and retrieved corpus supports long-horizon questions; it does not
replace candidate-level provenance and harmfulness evaluation. Its existing work
can be ported after the local extraction and population machinery has a stable
interface.

## Deferred efficiency and cleanup work

The query ledger synchronously acquires the database writer lock because a
successful response returns the durable query ID used by calibration and later
proposal lookup. This pass keeps that contract but replaces one statement per
exposure with bounded multi-row inserts of at most 100 exposure rows per
statement. The bound stays below SQLite parameter limits and reduces work inside
the writer critical section without making recording asynchronous.

Milestone 1 keeps its append-only ledger without pruning. Milestone 2 must set
request-size limits and a retention/export policy before calibration runs at
volume; broader deployment does not inherit unlimited retention by accident.

`StrictRecording`, `RecallQuerySurfaceCalibration`, `BuildRecallEvidenceWindow`,
and `GetRecallQueryEvent` remain exported as intentional Milestone 2 harness
contracts. They are not removed merely because production write-through does not
call them yet.

Reconciliation currently revisits a session whenever messages are replaced and
can hash the same window for multiple entries. The indexed empty-session query
makes this cheap before a corpus exists. Window caching, grouped hashing, and
more selective triggers should be measured against a populated archive rather
than added speculatively.

Large-function decomposition and dry-run/real-import refactoring are similarly
deferred until behavior is stable. The merge-hardening pass changes contracts
with focused tests and avoids mixing those changes with broad cleanup.

## Verification and merge gate

Implementation follows red-green TDD with observable SQLite, service, CLI, and
build-tag behavior. Required regression coverage includes:

- Empty review state persists as `unreviewed_auto`, while reviewed import and
  eval ingest retain their explicit states.
- Trusted-only plus archived status fails through direct, HTTP, and CLI-facing
  paths rather than returning an unexplained empty set.
- A duplicate import skips before archive-dependent evidence validation in both
  real and dry-run modes.
- Mixed anchored/legacy evidence endpoints remap or revoke correctly.
- Each provenance-revocation branch emits its stable reason.
- Mismatched entry/evidence session IDs are rejected without partial writes.
- `EXPLAIN QUERY PLAN` shows the source-UUID lookup using the existing partial
  index after adding its predicate.
- An event with more than one exposure batch persists every exposure in rank
  order within the same transaction.
- The `evalingest` tagged suite is exercised by its Make target and CI.
- Zensical builds and validates the new experimental page and navigation.

Before declaring the branch merge-ready, run the focused recall tests, the
tagged eval-ingest target, `go fmt ./...`, `go vet ./...`, the repository test
suite, lint, and the docs build/check workflow in isolated scratch state. Review
the final branch diff, including the explicit-server credential isolation in
`ad644c28`, and resolve or explicitly dismiss the remaining stale review
findings. Updating or merging the pull request remains a separate user decision.
