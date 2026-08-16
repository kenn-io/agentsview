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
as evidence. Recall queries support lexical, vector, and hybrid retrieval; the
default remains lexical while this feature is experimental.

## Current surface

The current implementation is local and SQLite-only. The CLI provides:

- `recall list`, `get`, and `stats` for inspection;
- `recall query` for ranked lexical, vector, or hybrid retrieval;
- `recall brief` for a packed, trusted task briefing;
- `recall extract` for opt-in model-backed extraction (see
  [Automatic extraction](#automatic-extraction)), including
  `recall extract preview` for previewing deterministic session chunks; and
- `recall import --dry-run` for validating reviewed JSONL candidates.

The top-level **Recall** page has two tabs:

- **Corpus** is a read-only browser for distilled entries. It shows extraction
  coverage and generation state, and filters entries by text, project, entry
  type, generation, and review state. Expand an entry to inspect its body,
  trigger, uncertainty, provenance metadata, and evidence links back to the
  source transcript.
- **Generated insights** creates and stores longer reports over an explicit
  session scope. Its form always shows the date range, project, session agent,
  automated-session scope, report template, generator, and optional focus used
  for the next report. Saved reports can be exported, published, linked, or
  deleted from the archive.

Open Recall from the header or navigate directly to `/recall`. Generated-report
links use `/recall?tab=generated&insight=<id>`.

![Recall corpus browser](/assets/generated/screenshots/recall-corpus.png)

![Generated insights](/assets/generated/screenshots/recall-generated-insights.png)

Generated insights use the configured OpenAI-compatible endpoint when
`[insights]` has both an `endpoint` and `model`; when `[insights]` is absent,
they use the selected agent CLI on your machine. Partial endpoint configuration
is rejected during configuration validation. Endpoint mode sends one non-streaming
`POST /chat/completions` request with the generated prompt as a user message.
It accepts the first choice's `assistant` message with string `message.content`
and optional response `model`. It does not support streaming, `/responses`, legacy completions,
tool calls, or content-part arrays. An endpoint failure returns an error and
does not retry through a CLI.

```toml
[insights]
endpoint = "http://127.0.0.1:11434/v1"
model = "llama3.1"
api_key_env = "OPENAI_API_KEY" # optional; the value is read at runtime
# allow_http = true            # required for non-loopback HTTP endpoints
```

Loopback HTTP endpoints are allowed for local models. Remote endpoints must
use HTTPS unless `allow_http = true` explicitly opts into plaintext transport.
The endpoint receives transcript-derived content, so review the provider's
privacy and retention behavior. API keys stay in the environment and are sent
only as a bearer header; they are not stored in the AgentsView configuration.
Canned insight cache keys include the effective backend, model, and a safe
endpoint identity, so changing `[insights]` after restarting the server selects
a separate cached report. Changes to credentials or transport opt-ins do not
change that identity; use force refresh when those changes should regenerate a
report under the same endpoint and model.

### Configuring agent binaries

AgentsView normally resolves `claude`, `codex`, `copilot`, `gemini`, and
`kiro-cli` through `PATH`. To pin a particular executable, configure its agent
table:

```toml
[agent.claude]
binary = "/usr/local/bin/claude"

[agent.gemini]
binary = "/usr/local/bin/gemini"
```

Each known agent has an independent override. This setting affects report
generation only; session discovery continues to read the configured session
directories.

Reviewed JSONL import is a guarded laboratory inlet, not a stable or recommended
end-user workflow. Use an isolated `AGENTSVIEW_DATA_DIR` for experiments. The
import command refuses the default data directory unless the operator explicitly
overrides that guard.

The Corpus tab is not available through PostgreSQL or DuckDB stores, so those
read-only servers open Recall on Generated insights instead. On the local SQLite
UI, Session Vital Signs also includes a read-only Recall panel for the open
session and links each evidence range back to the transcript. Corpus population,
review, extraction-generation management, and ranked querying remain CLI and
HTTP API workflows.

The daemon exposes the same inspection and query operations over its HTTP API.
Ordinary queries record measurement data when the SQLite store is writable, but
read-only archives remain queryable without recording.

## Vector and hybrid retrieval

`recall query` and `recall brief` accept `--mode lexical`, `--mode vector`, or
`--mode hybrid`. Lexical is the default. Vector search ranks the separate Recall
embedding store, while hybrid search combines lexical and vector ranks.

Recall uses the same `[vector]` model and embeddings servers as session semantic
search, but it has an independent index generation. Build it explicitly with:

```bash
agentsview embeddings build --store recall
```

Automatic Recall embedding requires separate consent because accepted entries
may contain distilled private content:

```toml
[vector.embed]
recall = true
```

That setting permits startup, corpus-mutation, and periodic refresh work to send
accepted Recall entry titles, bodies, and triggers to the configured embeddings
endpoint. It is off by default; manually running the build command is treated as
one-time consent for that invocation.

Vector and hybrid queries fail closed when the active Recall corpus is newer
than its last completed vector build. Rebuild the Recall store, or continue
using lexical mode while an automatic refresh catches up. See
[Semantic Search](/semantic-search/#enabling-vector) for the shared embedding
configuration and endpoint privacy considerations.

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

For a remote OpenAI-compatible provider such as Atlas Cloud, keep the key in the
environment and point the server entry at the provider's `/v1` base URL:

```toml
[recall.extract]
enabled = true
model = "deepseek-ai/deepseek-v4-pro"
server = "atlascloud"

[recall.extract.servers.atlascloud]
endpoint = "https://api.atlascloud.ai/v1"
api_key_env = "ATLASCLOUD_API_KEY"
timeout = "120s"
```

Optional keys: `deployment` (labels which serving instance produced the corpus),
`server` (selects among multiple named servers), `quiet_period` (default `"30m"`
— how long a session must have been ended before extraction),
`backstop_interval` (default `"1h"`), `failure_backoff` (default `"1h"`),
`max_window_chars` (default 50000), `max_tokens`, per-server `api_key_env`, a
`[recall.extract.prompts]` table (`profile`, `dir`), and a
`[recall.extract.request]` table (`temperature`, `extra_body`).

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

## Experimental limits

Recall remains an opt-in research feature. Automatic extraction never promotes
entries into trusted Recall, and the session panel is inspection-only. There is
no PostgreSQL or DuckDB Recall backend, no stable end-user import workflow, and
no pruning policy for the measurement ledger yet. Expect corpus rebuilds as the
schema, scoring, extraction policy, and trust model evolve.
