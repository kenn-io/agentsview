# Recall population: write-through local distillation

- Status: draft (agreed direction, not yet implemented)
- Date: 2026-07-09
- Branch context: `memory-substrate` (PR #1046, recall substrate)

## Problem

The recall substrate (PR #1046) ships the read side: storage (`recall_entries` /
`recall_evidence`), lexical ranking, context packing, CLI/HTTP surface, and a
gated JSONL import. What it deliberately does not ship is population — nothing
turns session transcripts into recall entries except a human (or harness)
authoring JSONL by hand.

Embeddings alone do not close this gap. Semantic search finds likely transcript
regions; it does not decide which facts, procedures, warnings, preferences, and
decisions are durable enough to carry forward. That judgment step — distillation
— is the missing half of the equation, and it is also the half that was never
validated: the extraction go/no-go gate in the original design was never run.

## Direction

Build **local, evidence-grounded distillation**, sequenced demand-first rather
than "every session auto-populates recall":

1. **Extractor calibration experiment** — measurement only, never touching the
   default archive.
1. **Write-through distillation** — the first real population path: a recall
   miss or archive search surfaces spans, a local model distills only what was
   actually needed, the verifier imports only evidence-backed entries.
1. **Broader backfill / end-of-session extraction** — later, and only if
   calibration shows the local extractor is precise enough.

The distiller is a **local, tools-disabled model** shown only exact ordinal
windows. Because MCP `search` (hybrid) and `get_messages` already speak in
session-id + ordinal coordinates — the same coordinates `recall_evidence` stores
— a model that only ever sees an ordinal window cannot cite evidence outside it,
and `requireRecallImportEvidence` mechanically verifies every cited range and
tool-use id against the real session at import time. Provenance verification
stays cheap and non-negotiable regardless of which model did the distilling.

Output is the existing accepted-recall JSONL shape, imported through the
existing gates in `internal/db/recall_import.go`: known type/scope, keeper label
(`correct` / `useful-but-uncertain` / `genuine-tradeoff`), `transferable`,
`provenance_ok`, evidence ordinals, and (by default) real-session verification.
No new write path bypasses these gates.

## Phase 1 — extractor calibration (measurement only)

Run the local extractor over sampled sessions and query-triggered spans in a lab
archive. The default-archive guard (`--allow-production-import` refusal on the
default data dir) already makes "don't silently populate the user's archive" a
property of the code, not of discipline. The `evalingest`-tagged trajectory
endpoint is the harness inlet.

Metrics:

- **Precision** — human-judged: is the distilled entry correct and stated at the
  right scope?
- **Provenance validity** — fraction of candidates whose cited spans actually
  support the claim (mechanical range checks are necessary but not sufficient;
  the span must semantically back the statement).
- **Duplicate rate** — see dedup definition below.
- **Usefulness** — did a later query recall and use the entry? Requires the
  usage instrumentation below; without it, usefulness stays anecdotal.

Prerequisites (instrumentation gaps to close before or during phase 1):

1. **Recall-hit tracking.** Entries currently have no usage signal — no
   `last_recalled_at`, no hit count. Add lightweight usage logging on the
   query path (e.g. bump a counter / timestamp when an entry is packed into
   returned context). Without this, phase 1 cannot measure usefulness and
   phase 3's gate criteria cannot be evaluated.
1. **Near-duplicate definition.** Supersession handles "this replaces that";
   nothing defines when two distilled entries are the same lesson. Pragmatic
   start: run each candidate through `recall query` against the existing
   corpus and against the current batch, and treat a match above a score
   threshold as a duplicate. This same check later becomes the write-time
   dedup gate for phase 2.

Extractor configuration hygiene: constrain decoding to the accepted-recall JSONL
schema directly (llama.cpp grammar / structured output) rather than parsing free
text, and stamp the existing `model` and `extractor_method` fields so
calibration results slice per extractor config for free.

## Phase 2 — write-through distillation (first population path)

The loop: an agent searches the archive (MCP `search` + `get_messages`, or the
CLI), reads the matching spans, answers something real — and when the answer was
worth the dig, the distiller runs over exactly those ordinal windows and emits a
candidate entry. Population becomes a side effect of use: entries are born
pre-validated by one real recall, evidence spans are exact, and cost is bounded
at O(answered recall misses), not O(sessions).

Design points:

- **Miss trigger must be precise.** Define "recall miss" as: empty packed
  context, or no result above a score threshold, for a `recall query` /
  `recall brief` invocation. Vague "seemed useful" triggers will flood the
  corpus.
- **Store the miss-query on the entry.** The schema's existing `trigger` field
  takes the query that missed — it is both the reason the entry exists and its
  best future retrieval hint.
- **Distinct provenance class.** Write-through entries self-apply the keeper
  label, so mark them via `extractor_method` (e.g. `write-through-local`).
  Trusted-only filtering can then distinguish human-reviewed from
  write-through until calibration numbers justify collapsing the distinction.
- **Write-time dedup.** Apply the phase-1 near-duplicate check before insert; a
  duplicate hit should either be dropped or routed into supersession if it
  strictly improves the existing entry.

## Phase 3 — backfill and end-of-session extraction (gated)

Only after phase 1/2 numbers show the local extractor is precise enough.
End-of-session extraction creates a large volume of low-value candidates unless
the gate is very strict; most sessions yield nothing durable. Intermediate
option: demand-driven backfill — run extraction only over sessions that semantic
search surfaces for queries recall missed, spending extraction budget where
recall demand demonstrably exists.

Entry criteria for enabling any automatic bulk extraction (to be firmed up with
phase-1 data): precision and provenance-validity floors, a duplicate rate
ceiling, and evidence from phase 2 that write-through entries are actually being
recalled and used.

## What we borrow from episodic-memory — and what we don't

obra/episodic-memory (README and MCP-TOOLS docs) is sync → parse → local embed
(Transformers.js) → SQLite/sqlite-vec index → semantic/text search, with full
transcripts archived at session end and intelligence applied at read time by a
search-dispatching skill. It has no durable-fact extraction; its population
model is trivially "archive everything."

AgentsView already has that entire layer: the session archive is the episodic
store, and FTS + semantic search (v0.37) plus the read-only MCP tools are the
retrieval path. What we borrow:

- **Read-path ergonomics** — a skill/dispatch pattern that reaches for archive
  search automatically when recall is needed.
- **Cheap per-session summaries** (they default to Haiku; ours would be local) —
  better semantic-search targets, and later a much smaller extraction input
  than raw transcripts.

What we do not borrow: the population model, because in the distillation sense
it does not have one.

## Non-goals (for this spec)

- Semantic/hybrid recall over `recall_entries` themselves (builds on the vector
  `IndexSpec` work; separate follow-up).
- Cloud-model extraction. The distiller is local; nothing in the archive leaves
  the machine.
- Any relaxation of the import gates. All population paths converge on the same
  funnel.
