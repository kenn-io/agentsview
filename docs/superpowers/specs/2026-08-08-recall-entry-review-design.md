# Recall Entry Review Design

## Summary

The Recall Corpus will let a user make a durable disposition on one accepted,
machine-extracted entry at a time. An expanded `unreviewed_auto` row offers two
actions: approve the entry as written or archive it as rejected. Approval is
available only while provenance remains valid; archive remains available when
provenance has been revoked.

The decision is stored in the existing entry row. Approval produces an
accepted `human_reviewed` entry. Rejection produces an archived
`human_rejected` entry. Both review states are outside extraction lifecycle
management, so later generation activation, retirement, reconciliation, or
re-extraction cannot silently undo a human decision.

## Goals

- Review individual accepted machine-extracted entries from the Corpus table.
- Make approval and rejection durable across extraction lifecycle changes.
- Prevent approval when the cited provenance has been revoked.
- Keep the interaction local to the expanded row and preserve scroll position.
- Keep Recall query revisions and vector freshness correct after a decision.
- Replace the SQLite review-state constraint with Go-owned validation.

## Non-goals

- Editing entry titles, bodies, triggers, evidence, or transferability.
- Reviewer notes, identity, history, or a separate audit log.
- Bulk actions, undo, or restoring rejected entries.
- Reviewing staged entries from building or retired generations.
- A CLI or MCP review surface.
- Recall mutation support for PostgreSQL or DuckDB stores.
- General entry-status administration beyond this two-action workflow.

## Approaches Considered

### Dedicated review action with durable review states

This is the selected approach. A purpose-built API accepts only `approve` or
`archive` and delegates to one storage transition. The entry's status records
whether it is served, while its review state records the human disposition.
Extraction already treats every state other than `unreviewed_auto` as
human-touched, so the decision remains stable without special lifecycle
exceptions.

### Reuse reviewed import

The import surface creates or supersedes entries and validates external
evidence payloads. Reusing it for an in-place decision would broaden a narrow
state transition into a second ingestion path and obscure conflict handling.
It is rejected.

### Generic entry patch endpoint

A generic patch could cover future editing, but it would expose fields and
transition combinations that are deliberately out of scope. The review action
keeps the public contract small and can be extended only when another workflow
is designed.

## Review-State Model

The allowed review states are defined and validated in Go:

| Review state      | Meaning                                      |
| ----------------- | -------------------------------------------- |
| `human_reviewed`  | A human approved the entry for serving       |
| `human_rejected`  | A human rejected and archived the entry      |
| `unreviewed_auto` | Generated or omitted review decision         |
| `calibrated_auto` | Automated output from a calibrated policy    |
| `eval_raw`        | Quarantined evaluation material              |

Approval changes only `review_state` from `unreviewed_auto` to
`human_reviewed`; status remains `accepted`. Archive changes status from
`accepted` to `archived` and review state from `unreviewed_auto` to
`human_rejected`. Both transitions update `updated_at` in the same transaction.
Entry content, evidence, provenance, transferability, source identity, and
extraction metadata do not change.

Approval requires all of the following at commit time:

- the entry exists;
- status is `accepted`;
- review state is `unreviewed_auto`; and
- `provenance_ok` is true.

Archive requires the first three conditions but deliberately permits revoked
provenance. A repeated decision or any stale transition is a conflict rather
than an idempotent success. Trusted Recall remains defined as accepted,
`human_reviewed`, transferable, provenance-valid material; a rejected entry can
never become trusted because it is archived.

## SQLite Schema Migration

The `recall_entries.review_state` SQL `CHECK` constraint is removed from the
canonical schema. Business rules for allowed review states move to shared Go
validation used by every Recall insertion and mutation boundary. New databases
therefore have no review-state business rule embedded in SQLite.

Existing archives receive one narrowly scoped, transactional migration of the
`recall_entries` table. It is implemented as a single immutable migration
guarded by schema inspection. The migration:

1. detects the legacy constrained table and is a no-op for the new shape;
2. takes exclusive writer ownership during normal writable startup;
3. creates the unconstrained replacement with the canonical column shape;
4. copies every entry while preserving `rowid`, IDs, timestamps, source links,
   supersession links, and all other values;
5. swaps the table, recreates its indexes and Recall entry triggers, and keeps
   the external-content FTS index attached to the preserved rowids;
6. verifies row counts and `PRAGMA foreign_key_check` before commit; and
7. restores foreign-key enforcement on every success or failure path.

No session, evidence, query measurement, extraction progress, or vector data is
discarded. The migration is not reused as an evolving compatibility path.
Read-only opens do not attempt it; the writable daemon or a normal writable
command must upgrade the archive first.

## Storage API

The shared store contract gains a typed review operation, conceptually:

```go
ReviewRecallEntry(
    ctx context.Context,
    id string,
    action RecallReviewAction,
) (RecallEntry, error)
```

`RecallReviewAction` accepts only `approve` and `archive`. The SQLite
implementation validates the action before opening a transaction, performs a
conditional transition, classifies a zero-row update by inspecting the current
entry inside the transaction, and returns the committed entry. PostgreSQL and
DuckDB implementations return their existing read-only error because those
stores do not own Recall mutations.

Existing Recall entry and evidence triggers advance the ranked-query revision.
Archiving also advances the served-corpus revision and emits the existing vector
change journal entry because the entry leaves the accepted corpus. The server
notifies the Recall embedding scheduler only after a successful commit. A
failed or conflicting decision produces no scheduler notification.

## HTTP API

The writable server exposes:

```text
POST /api/v1/recall/entries/{id}/review
Content-Type: application/json

{"action":"approve"}
```

The only accepted action values are `approve` and `archive`. A successful
response contains the updated `RecallEntry`, allowing the UI to update the row
without refetching the current page.

Errors follow the existing JSON API conventions:

- `400 Bad Request` for malformed JSON or an unknown action;
- `404 Not Found` when the entry does not exist;
- `409 Conflict` when the entry is not accepted, has already received a human
  disposition, or approval encounters revoked provenance;
- the existing read-only response for stores that cannot mutate Recall; and
- `503 Service Unavailable` with `Retry-After` for a closed writer or transient
  maintenance condition.

The handler does not make stale decisions appear successful. Error messages
identify the failed precondition without exposing transcript content.

## Corpus UI

Review controls live in the existing expanded table row. They appear only for
accepted `unreviewed_auto` entries, keeping the collapsed table dense and
leaving staged, reviewed, rejected, imported, and evaluation rows read-only.

The expanded row presents:

- **Approve** as the primary action;
- **Archive** as the secondary destructive action; and
- a short provenance warning when approval is disabled.

Approve submits immediately. Archive opens the shared confirmation modal and
submits only after confirmation. While a request is in flight, both controls
are disabled and the row remains expanded. A mutation error is shown inline in
that row so other entries remain usable.

On success, the returned entry replaces the local row. If the new status or
review state no longer matches the active filters, the row is removed locally.
The page is not reloaded, pagination is not reset, and scroll position is
preserved. The review-state filter includes localized labels for
`human_rejected` and every existing state.

All new labels, confirmation copy, disabled explanations, busy text, and error
copy use the Paraglide message catalogues. Existing kit-ui buttons and modal
components provide interaction and styling; the feature adds no one-off control
chrome.

## Concurrency and Failure Handling

The database transaction is the source of truth. The UI's disabled state avoids
duplicate clicks from one browser, while the conditional update detects another
client or background mutation that wins the race. A conflict leaves the local
row unchanged and gives the user a refreshable explanation.

Writer shutdown, maintenance, or commit failure must not report an uncommitted
decision as successful. Scheduler notification is best effort after commit: if
notification fails or the scheduler is absent, the durable mutation and its
change journal remain correct for startup or backstop reconciliation.

## Documentation

The Recall guide will describe the individual review workflow, the
`human_rejected` state, provenance gating, and the distinction between status
and review state. Internal extraction documentation will state explicitly that
both approved and rejected human dispositions are outside generation lifecycle
management.

## Testing

Tests exercise behavior rather than implementation text:

- database tests cover approve, archive, revoked-provenance archive, rejected
  approval, missing entries, repeated and stale decisions, timestamp changes,
  query/corpus revision effects, and generation activation preserving both
  human dispositions;
- migration tests open a legacy constrained archive containing entries,
  evidence, supersession links, and Recall FTS data, then verify preservation,
  foreign-key integrity, searchability, idempotence, and insertion of
  `human_rejected` through the Go boundary;
- server tests cover response bodies, status mapping, `Retry-After`, read-only
  behavior, and scheduler notification only after committed changes; and
- frontend component tests cover action visibility, revoked-provenance approval
  gating, archive confirmation, busy and inline-error states, local row update
  or removal under filters, and retained expansion/scroll behavior.

Implementation follows red-first tests. Focused database, server, and frontend
tests run before repository formatting, linting, Go formatting/vetting, and the
broader relevant suites.

## Acceptance Criteria

- An accepted `unreviewed_auto` entry with valid provenance can be approved from
  its expanded Corpus row and immediately becomes accepted `human_reviewed`.
- The same kind of entry can be archived after confirmation and immediately
  becomes archived `human_rejected`.
- Revoked provenance disables approval but does not prevent archive.
- Later extraction activation, retirement, reconciliation, or re-extraction
  does not move or delete either human-dispositioned entry.
- Concurrent or repeated decisions fail with a conflict and never overwrite the
  first committed decision.
- Existing archives upgrade without losing Recall rows, evidence, FTS linkage,
  source relationships, or revision integrity.
- Allowed review states are enforced in Go rather than by a SQLite `CHECK`.
- Successful mutations keep lexical, vector, hybrid, and paginated Recall reads
  fresh through the existing revision and scheduler mechanisms.
- Read-only deployments do not expose enabled review controls.
