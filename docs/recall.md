---
title: Recall (Experimental)
description: Find reusable lessons from past sessions, with links to the supporting messages
---

Recall helps you reuse facts, procedures, preferences, and warnings from past
agent sessions. It stores each lesson as an entry linked to the messages that
support it. You can browse entries, search them, or pack them into a task brief.

!!! warning "Active research"

    Recall's schema, scoring, trust policy, and workflows may change. Treat its
    entries and measurement rows as a rebuildable research corpus. Until Recall
    stabilizes, upgrades may require rebuilding its new tables instead of
    migrating them. The session archive remains authoritative and must not be
    deleted, truncated, or recreated to reset Recall.

While [Semantic search](/docs/semantic-search/) finds passages in transcripts,
Recall searches these extracted entries. You can match words (`lexical`, the
default), meaning (`vector`), or both (`hybrid`).

## Current surface

Right now, Recall runs locally on SQLite only. The CLI includes:

- `recall list`, `get`, and `stats` to inspect entries and corpus state;
- `recall query` for ranked lexical, vector, or hybrid retrieval;
- `recall brief` to build a packed, trusted task briefing;
- `recall extract` for opt-in model-backed extraction (see
  [Automatic extraction](#automatic-extraction)), including
  `recall extract preview` to preview deterministic session chunks; and
- `recall import --dry-run` to validate reviewed JSONL candidates.

To list entries changed at or after an RFC3339 timestamp, run
`recall list --updated-since <timestamp>`, or use `--created-since <timestamp>`
for entries created at or after that time. Both bounds are inclusive to the
millisecond, filtering `updated_since` against `updated_at` and `created_since`
against `created_at`. When you pass both, an entry must satisfy both bounds.

Time-bounded lists only return entries that still exist and do not report
deletions. If you mirror Recall elsewhere, periodically reconcile your copy
against a full listing and remove entries that are no longer present.

A bounded list returns the oldest change first and includes both archived and
accepted entries unless you add a status filter, though `trusted_only` still
selects accepted entries. Without a time bound, lists return accepted entries
newest first. Each entry's `source_run_id` identifies the extraction generation
it came from.

You cannot combine time bounds with a text query. Search orders by relevance,
while a checkpoint walk needs change ordering.

Both `GET /api/v1/recall/entries` and MCP `list_recall` accept `updated_since`
and `created_since`, returning `next_cursor` when more entries remain. To
follow changes:

1. List entries using your saved checkpoint as `updated_since`, and pass each
   `next_cursor` back until a page returns none.
1. After the final page, save the last entry's `updated_at` as your new
   checkpoint. Upsert each returned entry by `id`, because a later walk can
   return an updated version of an entry you already saved.
1. If a write lands during the walk, the next page fails with "recall corpus
   changed during pagination" (HTTP 409). Restart from the first page using
   every bound from the original walk (`updated_since`, `created_since`, or
   both) and keep your old checkpoint. Restarting from the last entry you
   received can miss an entry written in the same millisecond. Any Recall entry
   or evidence write can interrupt a walk, even if it falls outside your
   filters.

An archive created before Recall tracked changes can return a bounded result as
long as it fits on one page. If more entries match, narrow your bounds or
filters until they fit on a single page.

The top-level **Recall** page has two tabs:

- **Corpus** lets you browse and review distilled entries. It displays
  extraction coverage and generation state, and lets you filter by text,
  project, entry type, generation, and review state. Expand any entry to inspect
  its body, trigger, uncertainty, provenance metadata, evidence links back to
  the source transcript, and review controls for automatic entries.
- **Generated insights** creates and stores longer reports over a session scope
  you choose. Its form always shows the date range, project, session agent,
  automated-session scope, report template, generator, and optional focus for
  the next report. You can export, publish, link, or delete saved reports from
  the archive.

Open Recall from the header or go directly to `/recall`. Links to a specific
generated report use `/recall?tab=generated&insight=<id>`.

![Recall corpus browser](/docs/assets/generated/screenshots/recall-corpus.png)

![Generated insights](/docs/assets/generated/screenshots/recall-generated-insights.png)

When `[insights]` sets both an `endpoint` and a `model`, generated insights
call that OpenAI-compatible endpoint; when neither is set, they run the
selected agent CLI on your machine. To change the initial CLI selection, see
[Choosing the default agent](#choosing-the-default-agent). Setting only one of
`endpoint` or `model` fails configuration validation. Endpoint mode sends a
single non-streaming `POST /chat/completions` request with the generated prompt
as a user message, and accepts the first choice's `assistant` message with a
string `message.content` and optional response `model`. It does not support
streaming, `/responses`, legacy completions, tool calls, or content-part
arrays. If the endpoint fails, AgentsView returns an error and does not fall
back to a CLI.

```toml
[insights]
endpoint = "http://127.0.0.1:11434/v1"
model = "llama3.1"
api_key_env = "OPENAI_API_KEY" # optional; the value is read at runtime
# api_key_file = "~/.config/agentsview/insights-key" # or a private file
# allow_http = true            # required for non-loopback HTTP endpoints
# default_agent = "codex"      # generator used when no endpoint is configured
```

Local models can use loopback HTTP endpoints, while remote endpoints require
HTTPS unless you explicitly opt into plaintext transport with
`allow_http = true`. Because the endpoint receives transcript-derived content,
review the provider's privacy and retention policies before enabling it.
AgentsView reads API keys from the environment variable or private file you
name and sends them only as a bearer header, never storing the key in
`config.toml`. Canned insight cache keys include the effective backend, model,
and a safe endpoint identity, so changing `[insights]` and restarting the
server selects a separate cached report. Credential or transport opt-in changes
do not alter that identity; use force refresh if you want to regenerate a
report under the same endpoint and model.

You can also list, fetch, and generate insights from the CLI with
`agentsview insight`; see the [CLI reference](/docs/commands/).

### Tool effectiveness reports

From a session's actions menu, click **Analyze tool effectiveness** to generate
a report on that session showing which tool calls helped and which wasted
turns. Each conclusion is marked **Helped**, **Did not help**, or **Unclear**,
and names the calls it cites; clicking a cited call shows those calls, each with
a link to its message. The session's observed tool sequences and a note on
anything cut from the model's input appear below the conclusions.

This report sends each tool call's input and result to the insight agent, up to
256 KiB of text per run, so it uses more tokens than Agent Analysis and sends
whatever the commands printed, including any secrets. If a reply cites a
message or call that the model was never shown, the server retries once and
saves nothing if the second reply fails as well.

![Analyze tool effectiveness in the session actions menu](/docs/assets/generated/screenshots/tool-effectiveness-action.png)

![Tool effectiveness assessment with cited calls](/docs/assets/generated/screenshots/tool-effectiveness-report.png)

Saved reports appear under **Recall > Generated insights** with shareable local
links. If the transcript changes after you generate a report, the report warns
you that its observed facts may differ from what the model assessed. For rates
across projects and dates, see [Tool Usage](/docs/usage/#tool-usage).

### Configuring agent binaries

By default, AgentsView looks up `claude`, `codex`, `copilot`, `gemini`, and
`kiro-cli` on your `PATH`. To pin a specific executable, add a table for that
agent:

```toml
[agent.claude]
binary = "/usr/local/bin/claude"

[agent.gemini]
binary = "/usr/local/bin/gemini"
```

Each known agent has its own independent override. This setting only affects
report generation; session discovery still reads from your configured session
directories.

### Choosing the default agent

To pick the agent a new report starts with, set `default_agent` under
`[insights]`:

```toml
[insights]
default_agent = "codex"
```

Both the Web UI picker and requests that omit an agent use this value when set,
or fall back to `claude`. Valid values are `claude`, `codex`, `copilot`,
`gemini`, and `kiro`; any other value fails configuration validation at
startup. Picking a different agent in the UI overrides the default until you
switch servers. When `[insights]` also configures an `endpoint` and `model`,
`default_agent` has no effect because endpoint mode never invokes an agent CLI.

Reviewed JSONL import is a guarded laboratory inlet rather than a stable or
recommended end-user workflow. Use an isolated `AGENTSVIEW_DATA_DIR` for
experiments. The import command refuses to run against the default data
directory unless you explicitly override that guard.

PostgreSQL and ClickHouse stores do not support the Corpus tab, so those
read-only servers open Recall on Generated insights instead. In the local
SQLite UI, Session Vital Signs also includes a read-only Recall panel for the
open session, with each evidence range linked back to the transcript. Corpus
population, extraction-generation management, and ranked querying remain CLI
and HTTP API workflows.

The daemon exposes the same inspection and query operations over its HTTP API.
Ordinary queries record measurement data when the SQLite store is writable,
while read-only archives stay queryable without recording.

### Review extracted entries

To review an accepted `unreviewed_auto` entry, expand it in the Corpus table
and choose **Approve** or **Archive**. **Approve** immediately marks the entry
`human_reviewed`, and is disabled if its source evidence has been revoked.
**Archive** asks for confirmation, then marks the entry `human_rejected` and
removes it from the served Recall corpus.

Review actions are unavailable when `archive_content = "usage"`, which excludes
transcript-derived Recall entries from storage.

Both decisions are durable human states that later extraction-generation
changes never reverse. This view deliberately has no entry editing, bulk
review, or undo action.

![Review controls on an extracted Recall entry](/docs/assets/generated/screenshots/recall-review.png)

Recall requires SQLite FTS5, which all standard builds include. Opening a
writable archive upgrades the previous review-state schema when present and
converts any old FTS4 Recall search indexes to FTS5 in the same transaction;
index conversion also runs when the review-state schema is already current.
Existing entries and evidence stay intact, and existing FTS5 indexes do not
need rebuilding. The migration blocks startup until it finishes and never runs
again on later opens. Read-only archives keep their existing indexes and fall
back to a slower text lookup when legacy indexes cannot run ranked entry
search.

## Vector and hybrid retrieval

Both `recall query` and `recall brief` accept `--mode lexical` (the default),
`--mode vector`, or `--mode hybrid`. Vector search ranks the separate Recall
embedding store, and hybrid search combines lexical and vector ranks.

Recall shares the `[vector]` model and embeddings servers with session semantic
search, but keeps its own independent index generation. To build it manually,
run:

```bash
agentsview index embeddings build --store recall
```

Because accepted entries can contain distilled private content, automatic
Recall embedding requires separate consent:

```toml
[vector.embed]
recall = true
```

Enabling this setting lets startup, corpus-mutation, and periodic refresh jobs
send accepted Recall entry titles, bodies, and triggers to your configured
embeddings endpoint. It is off by default, and running the build command by
hand counts as one-time consent for that run.

Vector and hybrid queries keep working even when the Recall index trails the
corpus by a few entries, which normally happens while extraction is writing new
entries. Each insert, delete, accept, reject, or text edit of an accepted entry
counts as one corpus revision, and the index can trail by up to
`[vector] recall_max_revision_lag` revisions (default 256):

```toml
[vector]
recall_max_revision_lag = 256   # 0 requires the index to match the corpus exactly
```

Entries newer than the index are only absent from the vector ranking; hybrid
search still finds them through its lexical ranking, which runs after the query
is encoded. Results always reflect the entries as the query reads them, so any
entry deleted or rejected since the last build drops out automatically. Once
lag passes the bound, vector and hybrid queries fail closed until the Recall
store is rebuilt manually or by automatic refresh (when enabled), while lexical
mode keeps working. For shared embedding configuration and endpoint privacy
details, see [Semantic Search](/docs/semantic-search/#enabling-vector).

AgentsView ranks vector candidates before applying Recall filters. The
embedding backend bounds the candidate window, so a narrow project, cwd, or
other filter can return a short vector page when it hits that boundary. Hybrid
search still ranks the lexical candidates selected for your query and fuses
them with the available vector candidates.

## Automatic extraction

Extraction is opt-in and off by default. When you enable `[recall.extract]`, a
local OpenAI-compatible model distills ended sessions into entries, and the
daemon schedules passes automatically: sync activity triggers incremental
passes, and a periodic backstop revisits sessions whose transcripts changed
after extraction. Entries produced this way are stored as `unreviewed_auto` and
stay outside trusted Recall until promoted.

When your machine runs on battery, scheduled model passes pause and resume
within about a minute of returning to AC power, or on the next daemon start if
the daemon exited while waiting. Retraction of entries from trashed, automated,
or secret-bearing sessions continues on battery.

```toml
[recall.extract]
enabled = true
model = "your-model-name"

[recall.extract.servers.local]
endpoint = "http://127.0.0.1:30000/v1"
```

For a remote OpenAI-compatible provider such as Atlas Cloud, keep your API key
in the environment and point the server entry at the provider's `/v1` base URL:

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

Optional keys include `deployment` (labels which serving instance produced the
corpus), `server` (selects among multiple named servers), `quiet_period`
(default `"30m"`, setting how long a session must have ended before
extraction), `backstop_interval` (default `"1h"`), `failure_backoff` (default
`"1h"`), `max_window_chars` (default 50000), `max_tokens`,
`candidate_findings` (`"block"` by default, or `"allow"`, described below),
per-server `api_key_env` or `api_key_file` (see
[Read secrets from files](/docs/configuration/#read-secrets-from-files)),
per-server `concurrency` (default 1, described below), a
`[recall.extract.prompts]` table (`profile`, `dir`), and a
`[recall.extract.request]` table (`temperature`, `extra_body`).

`concurrency` controls how many sessions a pass distills at once against that
server, though each session's units still go out one at a time in order. Raise
it for hosted endpoints or servers that batch concurrent requests, where a
single-session pass leaves the endpoint mostly idle between round trips. For a
single local model, keep the default of 1 because parallel requests split the
same compute rather than adding throughput. Each in-flight session holds its
transcript in memory, and hosted endpoint rate limits still apply.

Because extraction sends transcript content to the configured endpoint,
non-loopback endpoints must use HTTPS so traffic off the machine is not exposed
over plaintext HTTP. You can set `allow_http = true` on a server entry to
accept that risk explicitly (for example on a trusted LAN). AgentsView never
follows redirects, because a redirect would replay the request and its
transcript content to whatever destination the endpoint names, and even a
same-origin allowance can be steered elsewhere by re-resolving the hostname.
Configure each endpoint with its final URL.

Sessions are eligible for extraction only when they are not automated, not
trashed, and have a clean, current **full** secret scan. By default, a session
with secret findings of any confidence, a session that has never been scanned,
or one covered only by the fast inline sync scan never reaches the model. Run
`agentsview secrets scan --backfill` to make sessions eligible. AgentsView
sends session content only to the endpoints you configure.

If you want to narrow that boundary deliberately, set
`candidate_findings = "allow"` under `[recall.extract]`. With `"allow"`,
candidate-confidence findings, which are the false-positive-prone heuristics
(`high-entropy-assignment`, JWT-shaped tokens, and basic-auth URLs) that
`secrets list` hides unless asked, remain recorded for review but no longer
block a session. Only definite findings then exclude a session during
discovery, the pre-send transcript check, commit, and reconciliation. The
default, `"block"`, keeps every recorded finding blocking. Consider `"allow"`
when the endpoint is a machine you own and your archive is full of paths and
identifiers that trip the entropy heuristic; keep the default for any endpoint
where you would not send a suspected secret.

Each distillation configuration (model, prompts, segmentation, and request
shape) is fingerprinted as a *generation*. Changing the configuration builds a
new corpus instead of mixing outputs, and only one generation is active at a
time. Use `recall extract status` to inspect coverage, `run` to execute a
manual pass, `activate` and `retire` to manage generations, and `doctor` to
validate your configuration with a single probe call.

When no generation is active yet, you can start serving the first collection
before the full backlog finishes:

```bash
agentsview recall extract status
agentsview recall extract activate
```

At least one session must be complete and usable entries must exist. Extraction
processes recent sessions first, and later passes add entries to the active
collection. Replacing an existing active generation still requires complete
eligible coverage so the replacement does not drop older knowledge, and
automatic activation also waits for that coverage. For the full set of
controls, see [extraction commands](/docs/commands/#agentsview-recall).

## Evidence and trust

Every durable entry points to a source session. Its evidence records exact
message ordinals, stable message identities when available, the selected tool
uses, and a digest of the model-visible content. Whenever a transcript is
reparsed or rewritten, AgentsView verifies that evidence mechanically.

If an anchored message disappears, becomes ambiguous, or changes its selected
content, AgentsView revokes the entry's provenance. Revocation is sticky, so
later parser output does not automatically restore trust or replace the stored
digest. Expect parser improvements to require regenerating some or all of your
Recall corpus during this experimental phase.

The host owns evidence authorization. A model or importer can narrow a window,
but it cannot point to another session, cite messages outside the supplied
window, or invent stable message IDs and digests. Evidence must belong to the
same source session as its entry, and AgentsView enforces these checks through
the shared insertion and reviewed-import boundaries rather than a separate
model write path.

Entries have one of five review states:

| Review state      | Meaning                                          |
| ----------------- | ------------------------------------------------ |
| `human_reviewed`  | Explicitly approved by a human                   |
| `human_rejected`  | Explicitly rejected and archived by a human      |
| `unreviewed_auto` | Generated or omitted review decision             |
| `calibrated_auto` | Automated output from a calibrated future policy |
| `eval_raw`        | Quarantined evaluation material                  |

A trusted-only read requires an accepted, `human_reviewed` entry that is both
transferable and provenance-valid. Automated labels cannot grant
`human_reviewed`. Raw evaluation entries are deliberately excluded; an eval
harness inspecting `eval_raw` material must pass `trusted_only=false`. The
build-tagged eval-ingest response returns a versioned `corpus_id`, which you
pass as `source_session_id` when querying so changed trajectory content or
source versions do not mix with earlier corpus versions from the same run.

An omitted review state fails closed to `unreviewed_auto`. Archived entries are
never trusted, and a trusted-only request with an explicit non-accepted status
returns an error instead of a misleading empty result.

## Reviewed imports and supersession

Reviewed JSONL import is the current laboratory population inlet. Candidate IDs
are immutable import identities, so re-importing an existing ID is an
idempotent skip even if its transcript was reparsed since the first import. New
candidates still go through current session, evidence, and supersession
validation.

A replacement can supersede only an active accepted entry that has no existing
successor. AgentsView archives that entry and links it to the replacement in
the same transaction, preventing two accepted replacements from branching off
one historical entry. Imports that use placeholder sessions have unverified
provenance: they can replace other unverified entries for evaluation, but they
cannot supersede a provenance-valid entry or remove it from trusted recall.

Always run the import command with `--dry-run` first. Writing requires `--yes`,
and a remote write also requires `--allow-remote-import`. Local import refuses
to write to the default production data directory unless you explicitly pass
`--allow-production-import`. These confirmations acknowledge the risk without
relaxing evidence, review-state, or supersession validation.

## Measurement and data lifecycle

Completed Recall queries record an append-only measurement event capturing the
surface, serialized filters, result and packed counts, miss reason, and the
ranked entries exposed to the caller. This ledger supports retrieval
calibration without changing the source session archive.

When recording succeeds, the response includes an opaque query ID. Initial miss
reasons separate queries with no ranked results from results that could not fit
within the requested context. Ranked and packed exposure is not treated as
proof that an answer used the entry or that the entry helped.

Ordinary recording runs on a best-effort basis so a ledger failure never hides
useful Recall output, while calibration callers can require strict recording.
Events and their ranked exposure snapshots survive a full resync even if a
referenced Recall entry no longer exists.

Right now, the experimental ledger is append-only with no pruning policy.
Before running calibration at volume, the project must define bounded request
sizes along with retention and export behavior.

During this research phase, you may need to rebuild Recall entries and
measurement rows when schemas, parsers, scoring, or extraction policies change.
Reset only the experimental Recall corpus through an explicit future workflow,
and never delete or recreate the session archive as a Recall reset strategy.

## Experimental limits

Recall remains an opt-in research feature. Automatic extraction never promotes
entries into trusted Recall, and the session panel is inspection-only. There is
no PostgreSQL or ClickHouse Recall backend, no stable end-user import workflow,
and no pruning policy for the measurement ledger yet. Expect corpus rebuilds as
the schema, scoring, extraction policy, and trust model evolve.
