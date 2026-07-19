---
title: Recall (Experimental)
description: Experimental, provenance-linked durable knowledge over the local session archive
---

!!! warning "Active research"

    Recall's schema, scoring, trust policy, and workflows may change. Treat its
    entries and measurement rows as a rebuildable research corpus. Until Recall
    stabilizes, upgrades may require rebuilding its new tables instead of migrating
    them. The session archive remains authoritative and must not be deleted,
    truncated, or recreated to reset Recall.

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
- `recall extract` for opt-in model-backed extraction (see
  [Automatic extraction](#automatic-extraction)), including
  `recall extract preview` for previewing deterministic session chunks; and
- `recall import --dry-run` for validating reviewed JSONL candidates.

Reviewed JSONL import is a guarded laboratory inlet, not a stable or recommended
end-user workflow. Use an isolated `AGENTSVIEW_DATA_DIR` for experiments. The
import command refuses the default data directory unless the operator explicitly
overrides that guard.

Recall is not available through PostgreSQL or DuckDB stores. It also has no web
UI and no semantic retrieval over Recall entries.

The daemon exposes the same inspection and query operations over its HTTP API.
Ordinary queries record measurement data when the SQLite store is writable, but
read-only archives remain queryable without recording.

## Automatic extraction

Extraction is opt-in and off by default. When `[recall.extract]` is enabled, a
local OpenAI-compatible model distills ended sessions into entries, and the
daemon schedules passes automatically: sync activity triggers incremental
passes, and a periodic backstop revisits sessions whose transcripts changed
after extraction. Entries produced this way are stored `unreviewed_auto` — they
remain outside trusted Recall until promoted.

```toml
[recall.extract]
enabled = true
model = "your-model-name"

[recall.extract.servers.local]
endpoint = "http://127.0.0.1:30000/v1"
```

Optional keys: `deployment` (labels which serving instance produced the corpus),
`server` (selects among multiple named servers), `quiet_period` (default `"30m"`
— how long a session must have been ended before extraction),
`backstop_interval` (default `"1h"`), `failure_backoff` (default `"1h"`),
`max_window_chars` (default 50000), `max_tokens`, a `[recall.extract.prompts]`
table (`profile`, `dir`), and a `[recall.extract.request]` table (`temperature`,
`extra_body`).

Non-loopback endpoints must use HTTPS: extraction sends transcript content to
the endpoint, and plaintext HTTP off the machine could be intercepted. A server
entry may set `allow_http = true` to accept that risk explicitly (for example on
a trusted LAN). Redirects are never followed: a redirect would replay the
request — transcript content included — to whatever destination the endpoint
names, and even a same-origin allowance can be steered elsewhere by re-resolving
the hostname. Configure the endpoint with its final URL.

Sessions are only ever extracted when they are not automated, not trashed, and
have a clean, current **full** secret scan — a session with secret findings of
any confidence, one never scanned, or one covered only by the fast inline sync
scan never reaches the model. Run `agentsview secrets scan --backfill` to make
sessions eligible. These filters are not configurable. Session content is sent
only to the endpoints you configure.

Each distillation configuration (model, prompts, segmentation, request shape) is
fingerprinted as a *generation*; changing the configuration builds a new corpus
rather than mixing outputs, and one generation is active at a time.
`recall extract status` shows coverage, `run` executes a manual pass,
`activate`/`retire` manage generations, and `doctor` validates the configuration
with a single probe call. See `docs/internal/recall-extraction.md` for the
design contracts.

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

Evidence authorization is host-owned. A model or importer may narrow a window,
but it cannot select another session, cite messages outside the supplied window,
or manufacture stable message IDs and digests. Evidence must belong to the same
source session as its entry. These checks run through the shared insertion and
reviewed-import boundaries rather than through a separate model write path.

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
harness inspecting `eval_raw` material must request `trusted_only=false`. The
build-tagged eval-ingest response returns a versioned `corpus_id`; pass it as
`source_session_id` when querying so changed trajectory content or source
versions do not mix with earlier corpus versions from the same run.

An omitted review state fails closed to `unreviewed_auto`. Archived entries are
never trusted, and a trusted-only request with an explicit non-accepted status
is rejected instead of returning a misleading empty result.

## Reviewed imports and supersession

Reviewed JSONL import is the current laboratory population inlet. Candidate IDs
are immutable import identities: re-importing an existing ID is an idempotent
skip, even if its transcript has subsequently been reparsed. A new candidate
still must pass current session, evidence, and supersession validation.

A replacement may supersede only an active accepted entry that has no existing
successor. AgentsView archives that entry and links it to the replacement in the
same transaction. This prevents two accepted replacements from branching from
one historical entry. Imports that use placeholder sessions have unverified
provenance: they may replace other unverified entries for evaluation, but cannot
supersede a provenance-valid entry or remove it from trusted recall.

Run the import command with `--dry-run` first. A write requires `--yes`, and a
remote write also requires `--allow-remote-import`. Local import refuses the
default production data directory unless `--allow-production-import` is supplied
explicitly. These confirmations acknowledge the risk; they do not relax
evidence, review-state, or supersession validation.

## Measurement and data lifecycle

Completed Recall queries record an append-only measurement event with the
surface, serialized filters, result and packed counts, miss reason, and the
ranked entries exposed to the caller. This ledger supports retrieval calibration
without changing the source session archive.

The response returns an opaque query ID when recording succeeds. Initial miss
reasons distinguish no ranked results from results that could not fit in the
requested context. Ranked and packed exposure is not treated as proof that an
answer used the entry or that the entry was helpful.

Ordinary recording is best effort so a ledger failure does not hide useful
Recall output. Calibration callers can require strict recording. Events and
their ranked exposure snapshots survive full resync even if a referenced Recall
entry no longer exists.

The experimental ledger is currently append-only and has no pruning policy.
Before running calibration at volume, the project must define bounded request
sizes plus retention and export behavior.

During this research phase, Recall entries and measurement rows may need to be
rebuilt when schemas, parsers, scoring, or extraction policies change. Reset
only the experimental Recall corpus through an explicit future workflow. Never
delete or recreate the session archive as a Recall reset strategy.

## Research direction

The Recall substrate introduced in 0.38.0 is the population foundation. It
deliberately does not ship a model runner, automatic write-through, bulk
extraction, automatic promotion, or per-session generated summaries. The next
work is intended to earn those capabilities in stages.

### Local extractor calibration

Calibration will run against isolated laboratory copies of real session rows and
exact host-built ordinal windows. One frozen, tools-disabled local model
configuration will extract structured candidates at a time. Each run should
record model and prompt versions, schema and decoding settings, input digests,
latency, and token or resource cost.

Independent judge models will evaluate correctness, semantic evidence support,
scope, transferability, harmfulness, and candidate duplication. Judges are local
by default, preferably from a different model family than the extractor. Small
blind human audits estimate judge error; the user is not expected to hand-label
the primary evaluation corpus.

A remote frontier judge is permitted only after an explicit per-run opt-in names
the endpoint and model and states that candidate text and supporting transcript
material will leave the machine. There is no automatic cloud fallback. Synthetic
or otherwise non-sensitive sessions can be selected for remote runs.

Calibration reports yield and abstention alongside keeper precision, harmful
output, transferability, semantic provenance, duplicate detection quality, and
local resource cost. Exposure records alone are not usefulness labels. Model
generation or judging never confers `human_reviewed`; automated entries remain
outside trusted Recall until a separate promotion policy is approved.

### Explicit write-through pilot

The first population pilot is an explicit callback after an answered Recall
miss, not an invisible side effect of reading:

1. `recall query` or `recall brief` returns a query ID and mechanical miss
   reason.
1. The agent or user finds supporting transcript regions with archive search and
   message reads.
1. A future proposal command submits the query ID and selected ordinal windows.
1. The host rebuilds and verifies those windows, runs the local distiller, and
   applies calibrated duplicate detection.
1. Candidates are stored as `unreviewed_auto`; the pilot requires an explicit
   promotion decision before they enter trusted Recall.

This bounds model cost to explicitly answered misses and keeps the actor, input,
evidence, and output auditable.

### Earned automation and benchmarks

Demand-driven backfill over sessions surfaced by recorded misses comes before
end-of-session extraction. Broad extraction is deferred until measured
precision, provenance, duplicate control, yield, cost, and explicit helpfulness
outcomes justify it. Semantic or hybrid retrieval over Recall entries is a
separate later experiment rather than part of population.

LongMemEval-v2 is planned as a complementary long-horizon benchmark once the
local extraction and population interfaces stabilize. It can measure whether a
populated corpus answers questions over time, but it does not replace
candidate-level provenance, harmfulness, and duplication evaluation.
