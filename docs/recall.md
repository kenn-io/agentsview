---
title: Recall (Experimental)
description: Experimental, provenance-linked durable knowledge over the local session archive
---

!!! warning "Active research"

    Recall's schema, scoring, trust policy, and workflows may change. Treat its
    entries and measurement rows as a rebuildable research corpus. The session
    archive remains authoritative and must not be deleted to reset Recall.

Recall is an experimental layer for durable, provenance-linked knowledge from
past agent sessions. It stores compact facts, procedures, preferences, and
warnings as entries that can be listed, queried, and packed into a task brief.

This is different from [semantic search](/semantic-search/). Semantic search
finds relevant passages in the transcript archive. Recall searches a separate
set of distilled entries and keeps the transcript region supporting each entry
as evidence. Recall entry retrieval is lexical today; it does not use the
embedding index.

## Current surface

The current implementation is local and SQLite-only. The CLI provides:

- `recall list`, `get`, and `stats` for inspection;
- `recall query` for ranked lexical retrieval;
- `recall brief` for a packed, trusted task briefing;
- `recall extract --dry-run` for previewing deterministic session chunks; and
- `recall import --dry-run` for validating reviewed JSONL candidates.

There is no automatic model-backed distillation yet. The extraction command does
not write entries, and reviewed JSONL import is a guarded laboratory inlet, not
a stable or recommended end-user workflow. Use an isolated `AGENTSVIEW_DATA_DIR`
for experiments. The import command refuses the default data directory unless
the operator explicitly overrides that guard.

Recall is not available through PostgreSQL or DuckDB stores. It also has no web
UI and no semantic retrieval over Recall entries.

## Evidence and trust

Each durable entry identifies a source session. Its evidence records exact
message ordinals, stable message identities when available, the selected tool
uses, and a digest of the model-visible content. When a transcript is reparsed
or rewritten, AgentsView verifies that evidence mechanically.

If an anchored message disappears, becomes ambiguous, or its selected content
changes, the entry's provenance is revoked. Revocation is sticky: later parser
output does not automatically restore trust or replace the stored digest.
Experimental users should expect parser improvements to require regeneration of
some or all of the Recall corpus.

Entries have one of four review states:

| Review state      | Meaning                                                 |
| ----------------- | ------------------------------------------------------- |
| `human_reviewed`  | Explicitly accepted through the reviewed import surface |
| `unreviewed_auto` | Generated or omitted review decision                    |
| `calibrated_auto` | Automated output from a calibrated future policy        |
| `eval_raw`        | Quarantined evaluation material                         |

A trusted-only read requires an accepted, `human_reviewed` entry that is both
transferable and provenance-valid. Automated labels cannot confer
`human_reviewed`. Raw evaluation entries are deliberately excluded; an eval
harness inspecting `eval_raw` material must request `trusted_only=false`.

## Measurement and data lifecycle

Completed Recall queries record an append-only measurement event with the
surface, serialized filters, result and packed counts, miss reason, and the
ranked entries exposed to the caller. This ledger supports retrieval calibration
without changing the source session archive.

During this research phase, Recall entries and measurement rows may need to be
rebuilt when schemas, parsers, scoring, or extraction policies change. Reset
only the experimental Recall corpus through an explicit future workflow. Never
delete or recreate the session archive as a Recall reset strategy.

## Research direction

The planned population path uses frozen, tools-disabled local models to extract
candidates from exact session windows. Independent judging is local by default.
A future calibration run may use a remote frontier judge only after an explicit
per-run opt-in names the endpoint and model and states that candidate text and
supporting transcript material will leave the machine. There is no automatic
remote fallback.

Model-generated or model-judged entries remain outside trusted Recall until a
separate trust decision promotes them. LongMemEval-v2 is planned as a later
long-horizon benchmark after the local extraction and population interfaces
stabilize; it does not replace evidence-level provenance evaluation.
