# Recall population: measured local write-through distillation

- Status: accepted design, staged implementation
- Date: 2026-07-09
- Branch context: `memory-substrate` (PR #1046, recall substrate)

## Problem

The recall substrate in PR #1046 provides local SQLite storage (`recall_entries`
and `recall_evidence`), lexical ranking, bounded context packing, CLI and HTTP
reads, and a gated JSONL import. It deliberately does not decide which
transcript material is durable enough to become recall. The existing
`recall extract` command only previews chunks.

Embeddings do not close that gap. Semantic and hybrid archive search find likely
transcript regions; they do not decide which facts, procedures, warnings,
preferences, and decisions should persist. That distillation step is also the
part that has never been validated: the extraction go/no-go experiment from the
original memory design was prepared but never run.

The current substrate has three safety and measurement gaps that must be fixed
before model-authored population:

1. `trusted_only` means only `transferable && provenance_ok`; it does not encode
   whether a human reviewed an entry.
1. Evidence verification proves that an ordinal range exists, not that the range
   was inside the window shown to the distiller or survived a later reparse.
1. Query results expose packed entry IDs but do not record demand, misses, or
   exposure. A counter increment would record retrieval, not downstream use.

## Direction

Build local, evidence-grounded distillation in four earned milestones:

1. **Population foundation** — trustworthy review-state semantics, append-only
   query/exposure measurement, and host-authorized evidence windows.
1. **Extractor calibration** — measurement only in an isolated copy of real
   session rows, with preregistered gates and blind human labels.
1. **Explicit write-through pilot** — an agent or user deliberately submits the
   query and transcript windows that answered a real miss; output remains
   quarantined until the calibrated policy permits promotion.
1. **Earned automation and backfill** — automatic promotion, demand-driven
   backfill, and only then end-of-session extraction.

This initiative is local-SQLite-first. SQLite remains the population authority
for these milestones. PostgreSQL propagation and read parity are required before
recall is described as available on shared/remote storage, but they are a
separate milestone rather than an implicit property of this design.

## Invariants

The following rules apply to every milestone:

- Transcript content never leaves the machine. The distiller is a local,
  tools-disabled model.
- Model output cannot grant itself trust, review status, or an evidence window.
- A model-authored candidate is not an accepted recall entry merely because it
  is valid JSON or cites real ordinals.
- Every accepted entry continues through shared type, scope, transferability,
  provenance, evidence, and supersession validation. A new population feature
  must not call `InsertRecallEntry` directly.
- Mechanical provenance means both existence and authorization: cited evidence
  exists in the source archive and is contained within the exact input window.
- Query instrumentation distinguishes returned results, packed exposure, and
  downstream outcome. It never labels exposure as usefulness.
- Any transcript rewrite that can alter existing content or ordinals reconciles
  affected recall evidence in the same transaction. It either remaps evidence
  to stable source identities or revokes provenance when the evidence no
  longer matches.
- Full-database resync preserves both recall and its append-only query ledger.
- New write paths are opt-in until calibration clears the registered gates.

## Milestone 1 — population foundation

Milestone 1 produces useful software without running a generative model. It
makes current recall auditable and creates the contracts later population uses.

### Review state and trusted recall

Add a write-path-controlled `review_state` to recall entries:

- `human_reviewed` — entered through the reviewed JSONL import after a human
  applied a keeper label.
- `unreviewed_auto` — model-authored and not approved for default context.
- `calibrated_auto` — reserved for a later policy that has cleared the automatic
  promotion gate.
- `eval_raw` — raw benchmark or harness material, never a distilled lesson.

The accepted JSONL input does not expose `review_state`. The reviewed importer
sets `human_reviewed`; eval trajectory ingest sets `eval_raw`; a future
write-through path sets `unreviewed_auto`. Low-level fixtures may specify the
field through `RecallEntry`, but user- or model-supplied JSON cannot spoof it.
The import operation itself is the authorization boundary: an unknown
`review_state` input member is rejected rather than copied, and a future model
output path cannot invoke the reviewed-import policy internally.

For Milestone 1, `trusted_only` requires all of:

- `status = accepted`
- `review_state = human_reviewed`
- `transferable = true`
- `provenance_ok = true`

`calibrated_auto` does not enter trusted recall until a later design explicitly
defines a versioned allow policy. `extractor_method` remains diagnostic metadata
and an exact query filter; it is not a trust boundary.

The recall tables are new in this unreleased PR, so Milestone 1 defines their
canonical schema directly and adds no recall-table migration or legacy backfill.
Each write surface assigns its state at creation: reviewed import uses
`human_reviewed`, eval ingest uses `eval_raw`, and later automatic paths use an
automatic state. Imports that create a `recall-import` placeholder session are
human-reviewed but have `provenance_ok` forced false because a zero-message
placeholder cannot mechanically support evidence. They remain visible in
untrusted inspection and cannot enter trusted recall. A later sync of the real
source session does not silently promote them; an explicit revalidation may
restore provenance only after the evidence range is verified and fingerprinted.

### Append-only demand and exposure ledger

Add two SQLite tables:

- `recall_query_events` — one completed recall request: opaque query ID, query
  text, surface, scope filters, trusted-only setting, score-policy version,
  result count, top score, packed count, miss reason, and timestamp.
- `recall_query_exposures` — one ranked result per event: entry ID, rank, score,
  and whether it was packed into returned context.

Exposure entry IDs are immutable snapshots, not cascading foreign keys. Deleting
or losing a later recall row must not rewrite measurement history. Both tables
are copied during full-database resync in the same preservation phase as recall
entries, including exposures for entries no longer present.

The query ID is returned in CLI/HTTP JSON when recording succeeds. Recording is
best-effort for ordinary reads: a ledger failure is logged but does not suppress
recall output. Calibration callers request strict recording and treat a missing
query ID as a failed measurement.

Initial miss reasons are:

- `no_results` — ranking returned no entries.
- `context_empty` — entries existed, but none fit in requested packed context.
- empty — at least one result was returned and, when context was requested, at
  least one entry was packed.

`below_threshold` is reserved until Milestone 2 calibrates a versioned score
policy. Current recall scores combine corpus-dependent IDF and discrete boosts,
so this design does not invent an arbitrary threshold.

Returned-but-unpacked rows are recorded because they explain ranking and packing
loss. Packed rows are exposures, not proof that a later answer used them. A
future explicit outcome event may record `helpful`, `not_helpful`, or `unknown`
against the query ID without rewriting the original event.

Raw query text is stored locally because the later write-through trigger needs
the exact missed query. Export, PostgreSQL propagation, and retention policy for
the ledger remain out of scope for Milestone 1.

### Host-authorized evidence windows

Archive search uses the exact AgentsView interfaces:

- MCP `search_content` in semantic or hybrid mode returns `session_id`, anchor
  `ordinal`, and `ordinal_range`.
- MCP `get_messages`, or the equivalent CLI/HTTP message endpoint, reads the
  surrounding transcript.

These coordinates are discovery input, not authority granted to the model. The
host builds a canonical `RecallEvidenceWindow` from the local database. It
contains:

- session ID and inclusive start/end ordinals
- the ordered messages actually supplied to the model
- stable message `source_uuid` values when available
- allowed tool-use IDs found inside the window
- a SHA-256 authorization digest of the canonical window representation

The distiller may narrow evidence within that window, but it cannot change the
session or cite an ordinal/tool-use ID outside it. The host validates
containment before converting a candidate to the shared recall-import
representation.

The model never needs to invent provenance already known by the host. Its
structured output carries the durable claim plus optional relative/narrowed
evidence selections; the host binds source session, window, extractor, model,
prompt version, and review state.

The broad authorization window and the narrower durable evidence selection have
different fingerprints. Evidence rows persist the selection's endpoint source
UUIDs plus a SHA-256 digest of the canonical selected messages and referenced
tool calls. The authorization-window bounds and digest belong to the later
calibration-run or write-through-proposal record; they are not overloaded onto
the evidence row.

Every full message replacement, in-place diff that changes an existing message,
and full-database resync reconciles affected evidence. When both endpoint source
UUIDs exist and resolve uniquely inside the same session, they remap shifted
ordinals and the selected-range digest is rechecked. When stable endpoints are
absent or ambiguous, the old ordinal range is retained only if its selected
digest still matches. A missing endpoint, changed digest, or missing referenced
tool call sets the parent entry's `provenance_ok = false`, which removes it from
trusted recall without deleting it. Append-only insertion after all cited
ordinals does not require reconciliation.

The new canonical evidence schema includes endpoint UUID and selected-range
digest fields from the start. Placeholder or otherwise unverified imports leave
them empty and have provenance revoked.

### Error and concurrency behavior

- Review-state validation rejects unknown values at write boundaries.
- Query-event and exposure insertion is one transaction. An event is never
  stored with a partial exposure set.
- Evidence-window construction fails if any requested ordinal is missing or if
  the range crosses sessions.
- Candidate containment validation runs before duplicate detection or insert.
- Later write-time duplicate detection and insertion must share a serialized
  transaction or uniqueness mechanism; a query-then-insert race is not an
  acceptable gate.
- A read-only SQLite open returns ordinary recall normally and omits best-effort
  query recording. A calibration request with strict recording fails with
  `ErrReadOnly` rather than pretending a measurement ID exists. PostgreSQL and
  DuckDB retain their current recall-unavailable `ErrReadOnly` behavior until
  their separate parity milestone.

## Milestone 2 — extractor calibration

Calibration runs against an isolated lab copy that preserves real `sessions`,
`messages`, and `tool_calls`. The `evalingest` trajectory endpoint remains
useful for retrieval benchmarks, but it is not the extraction-calibration inlet:
it flattens raw JSON into synthetic chunks and cannot provide real ordinal
evidence.

The sample has two separately reported cohorts:

- deterministic, stratified session windows, including high-signal and ordinary
  sessions
- demand-triggered windows collected from recorded misses and subsequent archive
  searches

Model output is constrained by a versioned machine-readable schema. Each run
records the exact model identifier, prompt version, grammar/schema version,
decoding parameters, window digest, latency, and token counts.

Human review is blind to extractor configuration. Reviewers label correctness,
scope, transferability, semantic provenance support, harmfulness, and duplicate
pairs. If possible, a second reviewer labels at least 20 percent of candidates
and agreement is reported.

The first decisive run requires at least 50 non-baseline candidates after exact
deduplication. Automatic write-through remains disabled unless all gates pass:

- keeper precision at least 40 percent
- harmful (`wrong` or `unsupported`) rate at most 20 percent
- at least 60 percent of keepers marked transferable
- at least 90 percent of keepers with semantically valid provenance
- keeper rate at least 15 percentage points above a provenance-preserving
  heuristic baseline
- acceptable recorded latency and local resource cost

Candidate yield and abstention rate are always reported so an extractor cannot
obtain perfect precision by emitting nothing.

Near-duplicate detection is calibrated rather than used to define its own
metric. Reviewers first label duplicate pairs. Candidate-to-entry query
construction, scope filters, score-policy version, and threshold are then chosen
against those labels and reported with detector precision and recall. The
current unbounded lexical score is not treated as a normalized similarity.

Usefulness is not a Milestone 2 go/no-go metric unless held-out tasks produce
explicit outcome labels. Query-ledger exposure alone is insufficient.

## Milestone 3 — explicit write-through pilot

Write-through is an explicit callback, not an invisible side effect of a read:

1. `recall brief` or `recall query` returns a tracked query ID and a mechanical
   miss reason.
1. The agent uses `search_content` plus message reads, or the CLI equivalents,
   to find transcript evidence.
1. After answering, the agent or user invokes a proposal command/API with the
   query ID and selected ordinal windows.
1. The host rebuilds and verifies those windows, runs the local tools-disabled
   distiller, binds provenance, and applies calibrated duplicate detection.
1. The result is stored as `unreviewed_auto`; a human can promote it during the
   pilot. The exact miss query is stored in the existing `trigger` field.

This keeps cost at O(explicitly answered recall misses) while making the actor,
input, and outcome auditable. The existing `agentsview-finding-history` skill is
the read-path dispatcher; the pilot extends it with the explicit proposal step
instead of creating a second archive-search workflow.

Only one frozen extractor configuration is evaluated at a time. Promotion to
`calibrated_auto` requires a later policy decision backed by pilot precision,
provenance, duplicate, and explicit helpfulness outcomes.

## Milestone 4 — earned automation and backfill

Demand-driven backfill is the first automation candidate: run extraction only
over sessions surfaced for recorded misses. End-of-session extraction comes
later because most sessions contain nothing durable and automatic session-wide
population creates the largest duplicate and false-memory burden.

Bulk extraction requires registered floors for precision and semantic
provenance, a duplicate-rate ceiling, acceptable abstention/yield, and evidence
from explicit outcomes that calibrated automatic entries help later work.

Semantic/hybrid retrieval over `recall_entries` can then build on vector
`IndexSpec`, but it remains a separate measured retrieval change.

## Relationship to episodic-memory

`obra/episodic-memory` remains a useful retrieval reference: it archives and
parses conversations, embeds exchanges locally, stores them in
SQLite/sqlite-vec, and provides semantic/text search plus a search-dispatching
skill. Current versions also generate per-conversation summaries during indexing
through Claude or Codex; those summaries are not a local durable-fact population
model.

AgentsView already has the relevant read layer and ships the
`agentsview-finding-history` skill. The useful borrow is the habit of automatic,
bounded archive search. Per-session summaries are deferred until an evaluation
shows that they improve retrieval enough to justify another generated artifact.

## Non-goals

- A cloud extraction fallback.
- Automatic acceptance of model-authored candidates in Milestone 1 or 2.
- Treating packed context as proof of usefulness.
- A new write path that bypasses the shared recall validation funnel.
- PostgreSQL or DuckDB population in the local calibration milestone.
- Semantic recall-entry retrieval, graph memory, or inferred memory links.
- End-of-session extraction before write-through earns it.

## Verification strategy

Milestone 1 requires observable tests for:

- canonical-schema review-state defaults and write-surface classification
- provenance revocation for placeholder-session imports
- trusted-only exclusion of unreviewed/eval entries across DB, service, HTTP,
  and CLI behavior
- inability of JSONL or model-shaped input to spoof review state
- atomic query-event/exposure recording, packed flags, miss reasons, and
  read-only behavior
- evidence-window construction over real messages and tool calls
- rejection of out-of-window ordinals and tool-use IDs
- source-UUID remapping and provenance revocation across routine message
  replacement and a full resync that shifts ordinals or changes evidence
  content
- query-ledger preservation across full resync, including exposures whose entry
  no longer survives
- successful recall reads from a read-only SQLite open without ledger writes

All new Go tests use testify, real temporary SQLite databases, and literal
expected behavior. After Go changes, run `go fmt ./...`, `go vet ./...`, and the
focused recall packages before the repository-wide test suite.
