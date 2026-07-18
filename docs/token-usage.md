---
title: Token Usage & Costs
description: Fast token usage and cost reports from your local AgentsView database
---

AgentsView records token usage while ingesting messages and usage events from
agents that write model and token metadata to their local logs. The database
already knows the input, output, cache-creation, and cache-read tokens those
agents have logged, and the `agentsview usage` commands turn that into daily
cost reports and a one-line today's-spend summary without re-reading source
files. To see that same cost attributed to specific time ranges and concurrent
agent activity, see the [Activity](/activity/) dashboard.

If you've used [`ccusage`](https://github.com/ryoppippi/ccusage) this will feel
familiar. AgentsView covers the same core job — "how much did I spend on AI
coding yesterday?" — across multiple coding agents from one archive. Because it
reads from pre-indexed SQLite instead of re-parsing JSONL on every invocation,
it's also dramatically faster on large histories (see
[benchmarks](#how-it-compares-to-ccusage) below).

!!! warning "Experimental"

    Token usage and cost reporting is a newer area of AgentsView and is still
    maturing. The Usage dashboard and the `agentsview usage` CLI may have rough
    edges, especially around agents whose parsers were recently taught to emit token
    counts. Bug reports and feature requests are very welcome — please
    [open an issue](https://github.com/kenn-io/agentsview/issues).

## Agent Coverage

!!! note

    **As of 0.34.0**, usage totals are populated when the source session includes
    token metadata for **Claude Code**, **Codex**, **Copilot CLI**, **OpenCode** and
    OpenCode-format forks such as **Kilo** and **MiMoCode**, **Pi**, **Gemini**,
    **Qwen Code**, **OpenClaw**, **QClaw**, **Hermes**, **WorkBuddy**, **Forge**,
    **Piebald**, **Antigravity IDE/CLI**, **Zed**, **VS Code Copilot**, **Visual
    Studio Copilot**, **Mistral Vibe**, and **gptme**.

    Coverage is opportunistic rather than guaranteed for every session from those
    agents: rows contribute to cost only when the local transcript includes usable
    token counts and a model name that can be priced. Other supported agents still
    appear in the session browser, search, and analytics even when their local logs
    do not expose token usage. Warp records session-level totals, but those totals
    are not yet folded into the per-message cost report.

When an agent filter selects only agents that do not expose per-message token
rows, AgentsView reports that as an unsupported usage state instead of silently
showing an empty chart. Copilot-family filters (`copilot`, `vscode-copilot`, and
`visualstudio-copilot`) keep Copilot-specific wording; any other no-token agent
falls back to a generic "matching sessions do not expose token usage data"
message.

### Cursor Admin Usage Events

Cursor has two usage sources in AgentsView:

- local Cursor transcripts, when `~/.cursor/projects` contains usable token
  metadata
- Cursor Admin API usage events, imported on demand with
  `agentsview usage cursor`

The admin import is useful when you want billable team usage from Cursor itself,
including headless/background events that may not map cleanly to a local
transcript. Configure an API key in `~/.agentsview/config.toml` or the
environment, then run:

```toml
cursor_admin_api_key = "key_xxxxx"
cursor_admin_email = "you@example.com" # optional default filter
cursor_admin_user_id = "152683922"     # optional default filter
```

```bash
# Import the last 30 days
agentsview usage cursor

# Import a specific inclusive local-date window
agentsview usage cursor --since 2026-05-01 --until 2026-05-31

# Import everything Cursor returns for this admin key/filter
agentsview usage cursor --all
```

The command calls Cursor's filtered usage-events endpoint, follows pagination,
stores the returned rows in the local archive, and deduplicates rows by a stable
event fingerprint. It is safe to rerun the same window after new events arrive.

Imported admin rows are folded into the Usage dashboard,
`agentsview usage daily`, DuckDB mirrors, and PostgreSQL after the usual
push/sync path. They appear as `agent = cursor`; because Cursor Admin events are
account-level billing rows rather than session transcripts, project, machine,
session-count, and top-session filters do not apply to those rows. Model and
date filters do apply.

Costs for admin rows come from Cursor's `chargedCents` field instead of
AgentsView's model-pricing table, so they can report spend even for models that
do not have a LiteLLM price entry.

## Usage Dashboard

AgentsView includes a dedicated **Usage** page in the web UI, reachable from the
**Usage** button in the header or directly at `/usage`. It's a focused view of
cost and token totals, driven by the same data the CLI commands read.

![Token usage dashboard](/assets/generated/screenshots/usage-page.png)

The page is built around four panels: summary cards, a cost trend over time, a
cost attribution treemap, and a bottom grid with top sessions by cost and a
cache efficiency breakdown.

In 0.23.0, the page also picks up a few workflow improvements:

- the page refreshes automatically on new sync data in normal `agentsview serve`
  mode
- the `Project | Model | Agent` selector is shared between the cost chart and
  the attribution panel
- active filters are preserved when switching between the **Sessions** and
  **Usage** tabs
- the top-cost sessions table now shows session names instead of only IDs or
  message previews where available

### Filters & Date Range

The toolbar at the top of the page scopes the entire dashboard. Pick a start and
end date with the date inputs, or narrow down with the Project, Agent, and Model
filter dropdowns. Usage opens to a rolling 30-day range. Linking its date
selection to other date-aware pages is optional. Turn it on with **Settings >
Date ranges > Link date ranges across pages**. Filter state is written back to
the URL — copying the address bar gives you a shareable link to the exact view
you're looking at. A **Clear filters** link appears next to the refresh button
when anything is active.

Project-key exclusions are the exception. Shared-store project keys are scoped
to the current aggregate archive set, so the page keeps those exclusions in
memory and does not write or restore them through the URL.

![Usage toolbar with filters](/assets/generated/screenshots/usage-toolbar.png)

Each filter dropdown supports multi-select with a search box, Select all /
Deselect all shortcuts, and a colored dot for agents so you can tell them apart
at a glance.

![Model filter dropdown](/assets/generated/screenshots/usage-filter-dropdown.png)

### Summary Cards

Eight baseline cards at the top summarize the selected window. The Total Cost
card is featured with a larger value; the rest show total tokens, daily burn,
peak day, cache hit rate, project and model counts, and active days.

![Usage summary cards](/assets/generated/screenshots/usage-summary-cards.png)

### Cost Over Time

A stacked chart shows cost per day across the range, grouped by project, model,
or agent — toggle the grouping with the segment buttons in the panel header.
Each series is colored consistently with the attribution panel below so you can
cross-reference them.

![Cost over time chart](/assets/generated/screenshots/usage-cost-trend.png)

### Cost Attribution

The attribution panel breaks down total spend for the window into a treemap plus
a ranked side rail. Switch the group-by between **Project**, **Model**, and
**Agent**, or flip the view from **Treemap** to **List** for a table-style
readout. Click any cell (or row) to hide it from the chart above, which is the
primary drill-down mechanic — hide the obvious outliers and the remaining
breakdown tells you where the smaller spend is going.

![Cost attribution treemap](/assets/generated/screenshots/usage-attribution.png)

### Pairwise Cost Comparison

The Usage page also includes **Comparative Cost Analysis** for side-by-side cost
checks. Pick a dimension (**Project** or **Model**) and value for the left side,
then pick the same or a different dimension and value for the right side. The
comparison uses the page's active date range and shared filters, then asks the
backend to compute both slices.

The result table shows total cost, session count, cost per session, total
tokens, tokens per session, input tokens, output tokens, the absolute delta from
left to right, and the percent delta when a ratio can be computed. It is useful
for questions such as "how much more expensive was project A than project B this
week?" or "how do two models compare after normalizing by session count?"

The same comparison is available over REST:

```http
GET /api/v1/usage/pairwise-comparison
```

Pass the normal usage filters plus `left_dimension`, `left_value`,
`right_dimension`, and `right_value`. Supported dimensions are `project` and
`model`. Branch-scoped comparisons use the shared `git_branch` usage filter with
an opaque token from `GET /api/v1/branches`.

### Top Sessions by Cost

A ranked list of the most expensive sessions in the window, with the agent pill,
session name, project, token total, and cost. Click any row to jump straight to
that session in the message viewer.

![Top sessions by cost](/assets/generated/screenshots/usage-top-sessions.png)

### Cache Efficiency

A stacked bar breakdown of cache reads, cache writes, uncached input tokens, and
output tokens, plus a savings callout showing how much you saved (or overspent)
versus a no-cache baseline. Useful for spotting prompts that are blowing through
cache creation without earning the reads back.

![Cache efficiency panel](/assets/generated/screenshots/usage-cache-efficiency.png)

The dashboard reads from the same `model_pricing` table that backs the CLI
commands below, so the numbers line up exactly with what
`agentsview usage daily` prints.

### PostgreSQL-Backed Usage

As of 0.23.0, the Usage page and usage API also work when the UI is served from
PostgreSQL via [`agentsview pg serve`](/pg-sync/). That means shared or
multi-machine deployments can browse the same cost and token dashboards without
relying on a local SQLite archive.

The live SSE refresh path is still specific to the normal local `serve` runtime.
`pg serve` is read-only and does not expose the global live event stream.

## Quick Tour

```bash
# Last 30 days of spend as a terminal table
agentsview usage daily

# Full history, with per-model breakdown rows
agentsview usage daily --all --breakdown

# JSON output for scripting
agentsview usage daily --json > spend.json

```

Examples:

```
> agentsview usage daily --since 2026-04-01
DATE        INPUT      OUTPUT    CACHE_CR   CACHE_RD    COST        MODELS
----        -----      ------    --------   --------    ----        ------
2026-04-01  77116868   2220604   39278251   887929665   $867.5794   claude-opus-4-6, gpt-5.4, claude-haiku-4-5-20251001, claude-sonnet-4-6, gemini-3.1-pro-preview
2026-04-02  39512998   2052183   32282358   669276138   $639.0390   claude-opus-4-6, gpt-5.4, claude-sonnet-4-6, claude-haiku-4-5-20251001
2026-04-03  44691255   1546401   28098708   429185669   $505.4604   claude-opus-4-6, gpt-5.4, claude-sonnet-4-6, claude-haiku-4-5-20251001
2026-04-04  46934657   1325888   14553015   414338356   $395.3920   claude-opus-4-6, gpt-5.4, claude-sonnet-4-6, claude-haiku-4-5-20251001, gpt-5.4-mini, gemini-3.1-pro-preview
2026-04-05  25170256   1941103   30847323   561656999   $528.7120   claude-opus-4-6, gpt-5.4, claude-sonnet-4-6, claude-haiku-4-5-20251001, gemini-3.1-pro-preview
2026-04-06  31754752   2229449   35744879   819607019   $737.4766   claude-opus-4-6, gpt-5.4, claude-haiku-4-5-20251001, gemini-3.1-pro-preview
2026-04-07  8892030    845077    13634936   320512173   $267.4140   claude-opus-4-6, gpt-5.4, claude-sonnet-4-6, claude-haiku-4-5-20251001, gemini-3.1-pro-preview
2026-04-08  31293887   1544001   20655222   342488168   $382.1367   claude-opus-4-6, gpt-5.4, claude-haiku-4-5-20251001, claude-sonnet-4-6, gemini-3.1-pro-preview
2026-04-09  13727647   993999    15882703   365800542   $319.4668   claude-opus-4-6, gpt-5.4, claude-haiku-4-5-20251001
2026-04-10  31267328   1733973   23694161   457175785   $460.5217   claude-opus-4-6, gpt-5.4, claude-haiku-4-5-20251001, claude-sonnet-4-6
2026-04-11  15380652   1346181   27087393   614828847   $522.8961   claude-opus-4-6, gpt-5.4, claude-haiku-4-5-20251001
2026-04-12  3633802    871157    7123679    231912052   $188.7764   claude-opus-4-6, gpt-5.4, claude-sonnet-4-6
----        -----      ------    --------   --------    ----        ------
TOTAL       369376132  18650016  288882628  6114711413  $5814.8710

# One-line today's spend (for prompt or tmux status line)
> agentsview usage statusline
$195.64 today
```

The daily table shows input, output, cache-creation, and cache-read token totals
per local-time day, the estimated cost, and the models that contributed to each
row. Adding `--breakdown` prints indented per-model sub-rows so you can see
which model drove the spend on each day.

## How Costs Are Computed

Every message parsed from a session file stores its raw `token_usage` JSON
(input, output, cache creation, cache read) and the model name reported by the
agent. The usage command:

1. Loads the `model_pricing` table into memory once per invocation. The table
   holds per-million-token rates for input, output, cache creation, and cache
   read.
1. Scans `messages` filtered by the requested date range and agent, parsing each
   row's `token_usage` blob in Go with `gjson` — faster than SQLite's per-row
   `json_extract`.
1. Multiplies each row's tokens by the model's rates, buckets the result by
   local-time day, and aggregates per model.

The default window is the last 30 days; pass `--all` to scan the full history.

!!! note

    AgentsView does not mint usage events on your behalf. It can only report token
    usage that the agent wrote to its own session files. Agents that don't emit
    token counts (or that strip them from local logs) won't show up.

### Pricing Source

Model rates come from the
[LiteLLM model pricing catalog](https://github.com/BerriAI/litellm), which is
fetched on each `usage` invocation and upserted into the `model_pricing` table.
If the fetch fails — no network, or LiteLLM is down — AgentsView falls back to
an embedded copy of the catalog so offline use keeps working. Pass `--offline`
to skip the fetch entirely and always use the embedded fallback.

The embedded fallback is updated with AgentsView releases, so the numbers are as
current as your installed version. For up-to-the-minute rates, leave `--offline`
off.

As of 0.32.0, the embedded fallback includes `claude-opus-4-7` at the same Opus
tier used for 4.6 and 4.8, so offline reports and fresh installs price Opus 4.7
sessions without waiting for a live LiteLLM fetch. 0.33.0 adds `claude-fable-5`
at its launch rates ($10 input, $50 output, $12.50 cache creation, $1 cache read
per million tokens).

### Custom Model Pricing

As of 0.24.0, you can supply per-million-token rates for models that aren't in
the LiteLLM catalog, or override the catalog's rates for models that are. Add
`[custom_model_pricing.<model>]` tables to `~/.agentsview/config.toml`:

```toml
[custom_model_pricing."acme-ultra-2.1"]
input = 2.0
output = 8.0
cache_creation = 2.5
cache_read = 0.2

[custom_model_pricing.internal-tiny]
input = 0.2
output = 0.8
```

| Field            | Description                                                       |
| ---------------- | ----------------------------------------------------------------- |
| `input`          | USD per million input tokens (defaults to `0` if omitted)         |
| `output`         | USD per million output tokens (defaults to `0` if omitted)        |
| `cache_creation` | USD per million cache-creation tokens (optional, defaults to `0`) |
| `cache_read`     | USD per million cache-read tokens (optional, defaults to `0`)     |

The table key is the model name as it appears in your session data (match the
string the agent itself writes, dots and all — quote the key if it contains
special characters). Custom rates take precedence over both the LiteLLM fetch
and the embedded fallback, and apply to the Usage dashboard, the
`agentsview usage` CLI, and `pg serve` alike. A custom entry replaces the full
rate row for that model, so omitted fields are treated as zero rather than
falling through to LiteLLM. Models without a custom entry continue to resolve
through LiteLLM as before.

### Copilot CLI Token Metrics

As of 0.32.0, Copilot CLI sessions contribute to usage and cost reports.
AgentsView reads per-message assistant output tokens from
`assistant.message.outputTokens`, then reads model-level session totals from
`session.shutdown.modelMetrics`. Fresh input tokens are computed as total input
minus cache reads and cache writes; cache writes map to cache-creation tokens,
and cache reads map to cache-read tokens. Copilot's Claude model IDs use dotted
version numbers, so the parser normalizes names such as `claude-sonnet-4.6` to
`claude-sonnet-4-6` before pricing lookup.

Upgrading to 0.32.0 bumps the parser data version so existing Copilot CLI
sessions are re-indexed with the new usage rows.

Some Copilot CLI records include `session.shutdown.modelMetrics` totals but omit
per-message `assistant.message.outputTokens`. AgentsView can still use the
aggregate model totals where they are complete, but it cannot reconstruct
per-message output-token rows from those records. As of 0.35.1, the CLI reports
that limitation directly instead of implying that the session has no usage data
at all.

### VS Code Copilot Token Metrics

As of 0.34.0, VS Code Copilot chat sessions also contribute when their persisted
request metadata includes token counts. AgentsView reads `promptTokens`,
`outputTokens`, and the resolved model from the session payload. VS Code reports
prompt tokens as a single total, so AgentsView treats them as input tokens;
prompt-cache discounts are not split out unless the source log exposes them
separately.

Upgrading to 0.34.0 re-indexes existing VS Code Copilot sessions so historical
chats pick up the newly available usage rows.

### Visual Studio Copilot Token Metrics

As of 0.34.0, Visual Studio Copilot traces also contribute when OpenTelemetry
spans include `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, and
model attributes. AgentsView deduplicates repeated trace flushes for the same
chat turn and keeps the copy with the most complete token usage before pricing
it.

### Copilot Reported Billing

Copilot CLI `session.shutdown` events can report an authoritative
`totalNanoAiu` billing total. AgentsView converts that cumulative amount
exactly (`totalNanoAiu / 1e11` USD) and stores the session-level value
through the normal reported-cost fields, on one stable usage-event row.
Later shutdowns supersede earlier totals, including an authoritative final
zero.

When the selected data contains this reported session cost, AgentsView
suppresses every catalog-priced estimate for that session. This prevents
double counting across models, days, and resumed segments. Historical Copilot
sessions without `totalNanoAiu` and other Copilot-family agents retain
catalog pricing.

The cumulative cost is attributed to the final shutdown day. Consequently, a
date window containing that shutdown receives the full session cost, while a
window containing only earlier rows can show estimates because the later
authoritative record is outside the selected data. This limitation is
unavoidable without allocating a cumulative session total across time.

Model-filtered reports and per-model breakdown costs remain token-price
estimates because the session-level cost cannot be allocated accurately among
models. Their sum can therefore differ from the authoritative session or
aggregate total.

### Claude Streaming & Codex Token Events

The 0.20.0 cost tracking release also improved how raw token usage is extracted
so the input side of the equation is accurate:

- **Claude Code:** streaming deltas previously wrote the same token counts
  multiple times, roughly doubling input totals. The parser now deduplicates
  them.
- **Codex:** per-request `token_count` events embedded in `event_msg` entries
  are now captured, so Codex sessions have populated token usage where they
  previously reported zero.

If you upgraded from an earlier version, the first `usage` invocation triggers a
full resync so these corrections apply to historical sessions.

## How It Compares to `ccusage`

`ccusage` re-walks every Claude Code JSONL file and re-parses from scratch on
every invocation. `agentsview usage` queries pre-indexed SQLite with an
in-memory pricing join, so the cost of reporting drops dramatically as history
grows.

Measured on a real 22,000-session database (~310,000 token-bearing messages) on
an M5 Max, median of 5 steady-state runs:

| Command                                                  |    Time | Speedup vs `ccusage` |
| -------------------------------------------------------- | ------: | -------------------: |
| `npx ccusage@latest daily --json --offline`              | 44.59 s |                   1× |
| `agentsview usage daily --json --all --offline`          |  0.53 s |              **84×** |
| `agentsview usage daily --json --offline` (default 30 d) |  0.41 s |             **109×** |
| `agentsview usage daily --json --offline --no-sync`      |  0.20 s |             **223×** |

!!! note

    These numbers are from a large local database (22k sessions, 310k token-bearing
    messages). The speedup scales with session count — smaller databases will see
    smaller absolute differences because `ccusage` has less JSONL to re-parse, but
    AgentsView stays in the sub-second range either way. The ratios above are an
    upper bound, not a universal guarantee.

Apples-to-apples: `ccusage` scans all history by default, so the `--all` row is
the matched comparison. The default 30-day window is faster still because most
invocations don't need four months of history, and `--no-sync` skips the
refresh-recent-files pass entirely (useful when you just want to re-render an
existing report).

Beyond raw speed, `agentsview usage`:

- **Works beyond Claude Code** — coverage includes Claude Code, Codex, Copilot
  CLI, OpenCode-format tools, Pi, Gemini, Qwen Code, OpenClaw/QClaw, Hermes,
  WorkBuddy, Forge, Piebald, Antigravity, Zed, VS Code Copilot, Visual Studio
  Copilot, Mistral Vibe, and gptme from the same database and command whenever
  those sessions log token metadata. Filter with `--agent <name>` when you
  want a single-agent view.
- **Shares one database with the UI** — the same data powers
  [Analytics](/usage/#dashboard) and session detail views, so there's no
  second index to keep fresh.
- **Includes on-demand sync** — when no AgentsView server is running, `usage`
  does a quick incremental sync scoped to files modified since the last sync
  start time so reports always reflect current state. Skip with `--no-sync`
  for the fastest path.

## `agentsview usage daily`

Daily cost report. Outputs a tab-aligned table to stdout by default, or JSON
with `--format json` (or the `--json` alias).

```bash
agentsview usage daily [flags]
```

| Flag          | Default       | Description                                                              |
| ------------- | ------------- | ------------------------------------------------------------------------ |
| `--format`    | `human`       | Output format: `human` or `json`                                         |
| `--json`      | `false`       | Alias for `--format json`                                                |
| `--since`     | `30 days ago` | Start of window, a duration like `28d` or a `YYYY-MM-DD` date, inclusive |
| `--until`     |               | End of window, a duration like `28d` or a `YYYY-MM-DD` date, inclusive   |
| `--all`       | `false`       | Include all history; overrides the default 30-day window                 |
| `--agent`     |               | Filter by agent name (e.g. `claude`, `codex`)                            |
| `--breakdown` | `false`       | Show indented per-model sub-rows under each day                          |
| `--offline`   | `false`       | Skip the LiteLLM fetch; use the embedded fallback pricing                |
| `--no-sync`   | `false`       | Skip the on-demand sync pass before querying                             |
| `--timezone`  | system        | IANA timezone name used for date bucketing                               |

The default 30-day window only kicks in when neither `--since` nor `--until` nor
`--all` is given. Passing just `--until` leaves the start open so "everything up
to X" still works.

**JSON shape:**

```json
{
  "schema_version": 2,
  "pricing": {
    "source": "fetched",
    "table_version": "2026-07-03T12:00:00Z",
    "latest_row_updated_at": "2026-07-03T12:00:00Z",
    "custom_override_count": 0,
    "effective_row_count": 2428,
    "digest": "sha256:8d815a1737bce68fa1a19ba977bf33c8c8efcc74deb954fcf62ce80e46e75f2c",
    "cost_source": "mixed",
    "fallback": {
      "used": false,
      "models": []
    },
    "models": {
      "claude-opus-4-6": {
        "matched_pattern": "claude-opus-4-6",
        "input_cost_per_mtok": 15,
        "output_cost_per_mtok": 75,
        "cache_write_cost_per_mtok": 18.75,
        "cache_read_cost_per_mtok": 1.5,
        "cost_source": "computed"
      }
    }
  },
  "projects": {
    "pl1:sha256:333e5f19bc8ed34f56fa89e51a9307bbc972d173498993ed02e564d32162196f": {
      "display_label": "agentsview",
      "resolution": "resolved",
      "identity": {
        "key": "p1:sha256:eb8c8bb90c27de41cdfb780f4c756cc4c3b9faf4f7c785c9f6afa7e160c2112c",
        "kind": "git_remote",
        "normalized_remote": "github.com/example/agentsview",
        "repository_key": "repo1:sha256:8a7da005b67fa8300b6072fd3a38629dc4505097258f7fb4398bf4cfd670df10"
      }
    },
    "pl1:sha256:ba5a8fb68c3e3f1454c428f19fdfd2dff9b2c40ae6dc2fef3a19a7c761bd72a1": {
      "display_label": "unknown-project",
      "resolution": "unknown"
    }
  },
  "daily": [
    {
      "date": "2026-04-12",
      "inputTokens": 33410,
      "outputTokens": 142805,
      "cacheCreationTokens": 301223,
      "cacheReadTokens": 2984511,
      "totalCost": 9.6052,
      "modelsUsed": ["claude-opus-4-6", "gpt-5.1"],
      "modelBreakdowns": [
        {
          "modelName": "claude-opus-4-6",
          "inputTokens": 28102,
          "outputTokens": 124901,
          "cacheCreationTokens": 287441,
          "cacheReadTokens": 2812004,
          "cost": 8.4123
        }
      ],
      "projectBreakdowns": [],
      "agentBreakdowns": [],
      "machineBreakdowns": [
        {
          "machineName": "build-host",
          "inputTokens": 33410,
          "outputTokens": 142805,
          "cacheCreationTokens": 301223,
          "cacheReadTokens": 2984511,
          "cost": 9.6052
        }
      ]
    }
  ],
  "totals": {
    "inputTokens": 134450,
    "outputTokens": 528375,
    "cacheCreationTokens": 1172133,
    "cacheReadTokens": 10908442,
    "totalCost": 36.4700
  }
}
```

`modelsUsed` is sorted by cost within each day, so the most expensive model
appears first. Daily entries always emit `modelBreakdowns`, `projectBreakdowns`,
`agentBreakdowns`, and `machineBreakdowns` as arrays; empty breakdowns are `[]`,
not omitted. `modelBreakdowns` always includes a row per model. The other three
arrays are populated when `--breakdown` is passed; the flag also controls
per-model terminal table output.

### JSON Contract

`agentsview usage daily --json` is a versioned JSON surface. The Usage daily
JSON, Activity report JSON, and session summary export JSON/NDJSON are separate
versioned surfaces, so a future bump in one does not imply a bump in the others.
Usage and activity already emitted `schema_version: 1` before 0.38, and the
session-summary v1 contract shipped in 0.37.1. Releases 0.38.0 and 0.38.1
emitted the substantially revised project-evidence shape while still reporting
version 1. Current builds correct all three markers to version 2; those two
transitional releases must not be treated as v1-compatible. The commands do
not provide a v1 output mode.

Relative to the v1-era payloads, usage daily v2 also drops the
`copilotAICredits` totals field, and totals substitute the authoritative
Copilot-reported session cost for catalog estimates when one is present.
Consumers should require the expected `schema_version` and ignore unknown
additive fields.

| Change                                                                                | Requires `schema_version` bump? |
| ------------------------------------------------------------------------------------- | ------------------------------- |
| Additive fields                                                                       | No                              |
| Row semantic changes                                                                  | Yes                             |
| Field type changes                                                                    | Yes                             |
| Sort order changes                                                                    | Yes                             |
| Cursor semantics changes                                                              | Yes                             |
| Required-field meaning changes                                                        | Yes                             |
| Field removal                                                                         | Yes                             |
| Pricing digest canonicalization changes                                               | Yes                             |
| Project key derivation, remote normalization, or path fallback normalization changes  | Yes                             |
| New closed-enum values for project resolution, session classification, or cost source | Yes                             |

Additive fields may appear in future payloads without a bump. Consumers
should ignore unknown keys.

### Pricing Provenance

Versioned usage, activity, and session-export payloads include a report-level
`pricing` block. `pricing.models` is nested under that block and is keyed by the
distinct model names that appear in the payload, not by every row in the pricing
table. Each model entry reports `matched_pattern`, `input_cost_per_mtok`,
`output_cost_per_mtok`, `cache_write_cost_per_mtok`, `cache_read_cost_per_mtok`,
and `cost_source`.

`cost_source` is a closed v2 enum everywhere it appears: `computed`, `reported`,
or `mixed`. `computed` means AgentsView derived cost from token counts and the
effective pricing resolver. `reported` means at least one source row supplied
explicit cost, such as Cursor Admin API billing data; those rows may not be
derivable from tokens times rates. `mixed` means both computed and reported
costs contributed. For computed costs, reasoning tokens are priced at the
output-token rate.

If a source reports an amount for a model with no matching effective pricing
row, the model entry has `cost_source: "reported"`, `matched_pattern: null`, and
zero in all four rate fields. The reported amount remains authoritative; the
zero rates express unavailable rate provenance, not a zero-rate calculation.

`pricing.source` is one of `embedded`, `fetched`, `custom`, `custom+embedded`,
or `custom+fetched`. Combined values always serialize `custom` first, followed
by the base table ingredient. `pricing.table_version` is the embedded fallback
snapshot version for embedded base tables, the latest fetched row timestamp for
fetched base tables, and `custom` for custom-only effective tables.
`pricing.digest` is independently recomputable as RFC 8785-style canonical JSON
hashed with SHA-256 and prefixed with `sha256:`. The digest input is exactly a
`{"rows":[...]}` object. Rows are sorted by `model_pattern` bytewise ascending,
then row `source`, the four rate fields, and `updated_at`; each row contains
exactly `model_pattern`, `input_per_mtok`, `output_per_mtok`,
`cache_write_per_mtok`, `cache_read_per_mtok`, `source`, and `updated_at`.
`updated_at` is `null` or a UTC RFC3339 timestamp such as
`2026-07-03T12:00:00Z`. Digest canonicalization errors fail the export instead
of emitting an empty digest. The digest uses the resolver's internal canonical
pricing-row keys; the public `pricing.models` block uses the `*_cost_per_mtok`
field names shown above.

### Project Identity

Versioned usage, activity, and session-export payloads include a report-level
`projects` catalog keyed by opaque `project_key` values prefixed with
`pl1:sha256:`. Project-bearing rows use the same key. Catalog entries contain a
presentation-only `display_label`, an explicit `resolution`, and `identity` only
when resolution succeeds.

Resolved identities prefer remote-backed keys. Local `file://` and bare-path
remotes are ignored for key derivation because they are machine-local. If no
usable network remote resolves, AgentsView falls back to a normalized root path
and emits identity `kind` as `machine_root`. Path-backed keys are useful within
one archive or machine, but consumers should not expect them to join across
machines.

Canonical project keys use `p1:sha256:`; repository, root, and worktree keys use
`repo1:sha256:`, `r1:sha256:`, and `wt1:sha256:`. Network remotes are normalized
after credentials, scheme, query, fragment, default port, and `.git` suffix are
removed. Absolute paths, raw remotes, and credentials do not cross the export
boundary. Remote-backed catalog identities omit machine-local `root_key`;
session rows retain their own complete root, worktree, and checkout facts.

SQLite catalog keys are archive-scoped. Shared PostgreSQL and DuckDB dashboard
responses may aggregate archives, so their catalog keys are response-scoped and
must not be persisted as durable selectors. The canonical `identity.key`, not
the display label or catalog key, represents project continuity.

## `agentsview usage statusline`

One-line today's spend, designed for shell prompts, tmux status lines, and
window titles.

```bash
agentsview usage statusline [flags]
```

| Flag        | Default | Description                        |
| ----------- | ------- | ---------------------------------- |
| `--agent`   |         | Filter by agent name               |
| `--offline` | `false` | Use embedded fallback pricing only |
| `--no-sync` | `false` | Skip on-demand sync                |

Output is a single line:

```
$9.61 today
```

With `--agent claude`:

```
$6.42 today (claude)
```

The command always scopes to the current local-time day. Use
`agentsview usage daily --since $(date +%Y-%m-%d)` if you want the full row
instead.

## `agentsview usage cursor`

Import Cursor Admin API usage events into the local archive so they contribute
to the Usage dashboard and daily reports.

```bash
agentsview usage cursor [flags]
```

| Flag          | Default       | Description                                              |
| ------------- | ------------- | -------------------------------------------------------- |
| `--since`     | `30 days ago` | Start date (`YYYY-MM-DD`), inclusive                     |
| `--until`     | today         | End date (`YYYY-MM-DD`), inclusive                       |
| `--all`       | `false`       | Include all history; overrides the default 30-day window |
| `--page-size` | `100`         | Cursor Admin API events requested per page               |
| `--email`     | config        | Filter by Cursor team member email                       |
| `--user-id`   | config        | Filter by Cursor team member user ID                     |

The API key is required and can be supplied as `cursor_admin_api_key` in
`~/.agentsview/config.toml` or as `AGENTSVIEW_CURSOR_ADMIN_API_KEY`. Optional
default member filters can be supplied with `cursor_admin_email` /
`cursor_admin_user_id` or their matching environment variables.

### Example: Starship Prompt Module

```toml
# ~/.config/starship.toml
[custom.agentsview]
command = "agentsview usage statusline --offline --no-sync"
when = "true"
format = "[$output]($style) "
style = "bold green"
```

Pair with `--no-sync` so the prompt never blocks on a sync pass; a separate
`agentsview` server (or a periodic `agentsview sync` cron) keeps the database
fresh.

## On-Demand Sync

When no AgentsView server is running, the `usage` commands do a quick
incremental sync before querying so reports always include recent activity:

1. If the parser data version has changed (i.e. you just upgraded), a full
   resync runs first.
1. Otherwise, the sync scans only files modified since the last recorded sync
   start time, minus a 10-second safety margin to catch files written during
   the prior sync.

If an `agentsview serve` process is already running, the file watcher already
has you covered and the on-demand sync is skipped to avoid duplicate work. A
running `pg serve` process does not keep your local SQLite archive fresh, so the
CLI still treats the local archive as the source of truth for command-line
reporting.

Pass `--no-sync` to skip the refresh unconditionally — useful for scripting and
for prompt modules that must stay snappy.

## Scripting Examples

**Monthly spend for the current month:**

```bash
agentsview usage daily \
  --since "$(date +%Y-%m-01)" \
  --json \
  | jq '.totals.totalCost'
```

**Per-agent totals for the last 7 days:**

`date` arithmetic differs between BSD (macOS) and GNU (Linux), so the snippet
tries the BSD form first and falls back to GNU:

```bash
since=$(date -v-7d +%Y-%m-%d 2>/dev/null \
  || date -d '7 days ago' +%Y-%m-%d)

for a in claude codex copilot gemini; do
  total=$(agentsview usage daily \
    --since "$since" \
    --agent "$a" \
    --json 2>/dev/null \
    | jq '.totals.totalCost')
  printf "%-8s  \$%s\n" "$a" "$total"
done
```

**Alert when today crosses a budget:**

The script writes to stderr and exits non-zero so you can wire it into whatever
notifier fits your OS — cron's `MAILTO`, launchd's `StandardErrorPath`, a
systemd timer's journal, or a Windows Task Scheduler action:

```bash
today=$(agentsview usage statusline --offline --no-sync \
  | tr -dc '0-9.')
if awk -v t="$today" 'BEGIN {exit !(t+0 > 25)}'; then
  echo "AgentsView: AI spend \$$today today (> \$25)" >&2
  exit 1
fi
```

## Where the Data Lives

Usage reports read from the same local SQLite database that powers the
[web UI](/usage/) and [Analytics dashboard](/usage/#dashboard). Token usage is
stored on each message row in the `messages` table; pricing is cached in a small
`model_pricing` table that's refreshed on each `usage` invocation.

No data leaves your machine. The only outbound request is the LiteLLM pricing
fetch, which you can disable with `--offline`. See
[Privacy and Telemetry](/configuration/#privacy-and-telemetry) for the full
picture.
