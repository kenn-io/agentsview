---
title: Token Usage & Costs
description: Fast token usage and cost reports from your local AgentsView database
---

AgentsView records token usage as it ingests messages and usage events from
agents that write model and token metadata to their local logs. The database
already holds the input, output, cache-creation, and cache-read tokens those
agents logged, and the `agentsview report usage` commands build daily cost
reports and a one-line today's-spend summary from that data without re-reading
source files. To attribute that same spend to specific time ranges and
concurrent agent activity, open the [Activity](/docs/activity/) dashboard.

If you have used [`ccusage`](https://github.com/ryoppippi/ccusage), this setup
is familiar. AgentsView answers the same core question, "how much did I spend
on AI coding yesterday?", across multiple coding agents from a single archive.

!!! warning "Experimental"

    Token usage and cost reporting is a newer part of AgentsView and is still
    maturing. The Usage dashboard and the `agentsview report usage` CLI can
    have rough edges, especially for agents whose parsers recently added token
    counts. If you run into a bug or want to request a feature,
    [open an issue](https://github.com/kenn-io/agentsview/issues).

## Agent coverage

!!! note

    AgentsView populates usage totals whenever the source session includes
    token metadata for **Claude Code**, **Codex**, **Command Code**,
    **Copilot CLI**, **OpenCode** and OpenCode-format forks such as
    **IcodeMate**, **Kilo**, and **MiMoCode**, **Cursor IDE**, **Posit
    Assistant**, **Pi**, **Prime Agent**, **Gemini**, **Qwen Code**,
    **OpenClaw**, **QClaw**, **Hermes**, **WorkBuddy**, **Forge**, **Piebald**,
    **Antigravity IDE/CLI**, **Zed**, **VS Code Copilot**, **Visual Studio
    Copilot**, **Mistral Vibe**, **gptme**, **Amp**, **Junie CLI**, **OMO**,
    **StepCode**, and **Direct API** usage files. Coverage also includes
    **Cline CLI**, **Tencent CodeBuddy CN**, **Augure Code**, **Open
    Interpreter**, **Augure Desktop 3 beta**, **Charm Crush**, **fx**, **Muse
    Code**, and **Factory Droid**, and reads **DeepSeek Harness** formats
    through version 3.

    Command Code forks count only usage produced after the fork.

    What you see depends on what each agent records. AgentsView uses recorded
    costs when available; otherwise it estimates cost from usable token counts
    and a model it can price. Sessions without usage metadata still appear in
    the browser, search, and analytics. Warp records session-level totals, but
    those totals are not yet folded into the per-message cost report.

Crush contributes recorded session totals and cost, with no per-message token
or cache breakdown. Because its aggregate usage is attributed to the most
recent assistant model, it cannot break down a session by model if the session
switched models.

Factory Droid writes one running token total per session, with no per-message
breakdown, in the `<id>.settings.json` file beside each transcript. AgentsView
reports it as a single usage entry dated at the session's last activity and
prices it from the pricing catalog; Factory credits are not used. AgentsView
adds subagent totals to the session that started them. Because Droid does not
record which model a subagent used, AgentsView prices those totals as that
session's model. A subagent session with its own transcript shows its messages
but no usage, because its parent already counts it. Droid also omits
per-request usage, so Factory sessions report total output tokens without a
peak context size. A bring-your-own-key model appears under its Droid name,
such as `custom:my-model-0`, which stays unpriced until you map it with
`price_as` in [custom model pricing](#custom-model-pricing).

For gptme messages, AgentsView uses provider-reported billed costs when
available, including a reported zero, while gptme messages with estimated costs
still use catalog pricing. Provider-routing suffixes in model names do not
block a catalog match.

fx stopped writing `usage-v2.json` in
[vercel-labs/fx#1226](https://github.com/vercel-labs/fx/pull/1226). When an
older session still has one, AgentsView reads it as cumulative per-model totals
dated at the earlier of the session's latest event and the usage file's
modification time. Sessions from current fx have no token usage, and an old
session resumed in newer fx keeps its old totals. Costs come from AgentsView's
pricing catalog. Tool results use saved `.txt` output only when it is a regular
UTF-8 file that matches the saved byte count or reaches the 16 MiB cap;
otherwise the preview stays. Restoring a file refreshes the tool result on the
next sync. Freshness checks cover entry names, sizes, and modification times in
`tool-results/`, which is enough because fx names result files by content hash.

Muse Code records token counts for each model call without a cost. AgentsView
prices those tokens using Meta's published Muse Spark and Muse Wonder rates.
Because Muse counts cached prompt tokens inside its input total, AgentsView
bills those tokens once at the cached rate. Automated approval reviews are not
counted.

Augure Code and Augure Desktop keep their recorded usage. Proprietary Augure
models have no catalog price, so token-based cost estimates stay unpriced
unless you add [custom model pricing](#custom-model-pricing). Recorded
authoritative costs are preserved whenever the source provides them.

Codex Cloud chats (ChatGPT dot, Codex web, and Codex cloud tasks) get token
counts only from live capture, which is off unless you set
`live_tokens = true`. AgentsView must listen in on a chat at least once while
it runs. Because each token update from OpenAI carries the chat's running
total, one update also covers earlier turns back to the last time OpenAI
restarted that total. Chats that AgentsView never listened in on carry plan
usage instead: OpenAI's usage report gives each chat's share of your ChatGPT
plan's weekly and five-hour limits along with a credits figure. AgentsView
leaves those chats out of token and cost totals. The Usage page and
`agentsview report usage daily` tell you how many were left out, and per-model
rows show "Plan usage" when those chats are the model's only usage. A chat that
AgentsView has not asked about yet is counted too, and its session details say
"Plan usage: not checked yet". If a chat has recorded token counts and a
finished turn that live capture did not fully see, it stays in the totals, and
the totals note that its token counts are low. See
[Codex Cloud](/docs/configuration/#codex-cloud) for setup and limits.

Junie coverage applies to CLI sessions and excludes tool calls and IDE-only
conversations. Antigravity plaintext transcripts include conversation,
reasoning, and tool calls, and without a usable database or usage-bearing
trajectory sidecar they have no token usage or cost. Since 0.46.1, Antigravity
CLI can combine a plaintext transcript with its database's usage and attach the
model and token counts to the matching model replies. See
[Antigravity transcript sources](/docs/configuration/#session-discovery).

When an agent filter selects only agents that do not expose per-message token
rows, AgentsView shows an unsupported usage state instead of an empty chart.
Copilot-family filters (`copilot`, `vscode-copilot`, and
`visualstudio-copilot`) keep Copilot-specific wording; any other no-token agent
falls back to the generic message "Matching sessions do not expose token usage
data." AI-credit cost denomination (shown as "Copilot AI Credits" for Copilot
agents) is tracked separately from no-token-data, so future agents can opt into
either behavior independently.

### Cursor Admin usage events

Cursor has two usage sources in AgentsView:

- local Cursor transcripts, when `~/.cursor/projects` contains usable token
  metadata
- Cursor Admin API usage events, imported on demand with
  `agentsview source cursor sync-usage`

Use the admin import when you want billable team usage directly from Cursor,
including headless or background events that may not map cleanly to a local
transcript. Configure an API key in `~/.agentsview/config.toml` or the
environment, then run the sync command:

```toml
cursor_admin_api_key = "key_xxxxx"
cursor_admin_email = "you@example.com" # optional default filter
cursor_admin_user_id = "152683922"     # optional default filter
```

```bash
# Import the last 30 days
agentsview source cursor sync-usage

# Import a specific inclusive local-date window
agentsview source cursor sync-usage --since 2026-05-01 --until 2026-05-31

# Import everything Cursor returns for this admin key/filter
agentsview source cursor sync-usage --all
```

The command calls Cursor's filtered usage-events endpoint, follows pagination,
stores the returned rows in your local archive, and deduplicates them using a
stable event fingerprint. You can safely rerun the same window after new events
arrive.

Imported admin rows appear in the Usage dashboard,
`agentsview report usage daily`, and PostgreSQL after the usual push or sync
path. They are tagged with `agent = cursor`. Because Cursor Admin events are
account-level billing rows rather than session transcripts, project, machine,
session-count, and top-session filters do not apply to them, while model and
date filters do.

Costs for admin rows come from Cursor's `chargedCents` field instead of
AgentsView's model-pricing table, so spend still appears even for models that
lack a LiteLLM price entry.

### xAI account usage CSV

To import a usage CSV downloaded from xAI, run:

```bash
agentsview usage grok usage.csv
```

AgentsView validates the entire file before writing billing events to the local
archive, and reimporting overlapping exports skips existing `Billing Item ID`
values. The importer accepts `grok-build`, `grok-bot`, and `cursor` products,
mapping them to the `grok`, `grok-bot`, and `cursor` agents. Their events
contribute to the Usage dashboard and daily reports and follow the usual mirror
push paths. The **Agent** filter includes billing-only products, and the **All**
range starts at the earliest session or billing event, even in archives with no
local sessions.

Prompt, completion, cached prompt, and cache-write counters stay separate.
Billing counters can aggregate many requests and have no per-request token cap.
Costs come from the export's USD `Pricing Contracted` field, rounded to
microdollars, and a reported $0 stays $0 even when model pricing is available.
AgentsView estimates cache savings at base model rates. Aggregate counters do
not establish a single request's context size for pricing bands. Blank models
stay blank in the archive and appear as `<unknown>` in reports. Event
timestamps retain their UTC instant, and reports display them in the selected
timezone.

These exports do not include conversations, session IDs, projects, machine
names, or automation evidence, and the import creates no sessions. Project,
machine, and session-only filters exclude these billing events, while agent,
model, and date filters apply. Each event's kind retains `Charge Category` and
`Usage Type` alongside the product.

AgentsView cannot match billing IDs to native session events or Cursor Admin
API fingerprints, and importing overlapping usage through those sources can
count the same usage twice. CSV reimport deduplication applies only to
identical billing IDs.

## Usage dashboard

AgentsView includes a dedicated **Usage** page in the web UI, available from
the **Usage** button in the header or directly at `/usage`. It gives you a
focused view of cost and token totals driven by the same data the CLI commands
read.

![Token usage dashboard](/docs/assets/generated/screenshots/usage-page.png)

The page is organized into four panels: summary cards, a cost trend over time,
a cost attribution treemap, and a bottom grid with top sessions by cost and a
cache efficiency breakdown.

Since 0.23.0, the page also includes several workflow updates:

- The page refreshes automatically when new sync data arrives in normal
  `agentsview serve` mode.
- The `Project | Model | Agent` selector is shared between the cost chart and
  the attribution panel.
- Active filters stay intact when you switch between the **Sessions** and
  **Usage** tabs.
- The top-cost sessions table shows session names instead of only IDs or
  message previews when names are available.

### Filters & date range

The toolbar at the top of the page scopes the entire dashboard. Pick a start
and end date with the date inputs, or narrow the view with the **Project**,
**Agent**, and **Model** dropdowns. Usage opens to a rolling 30-day range.
Linking its date selection to other date-aware pages is optional; to turn it
on, go to **Settings > Date ranges > Link date ranges across pages**. The URL
preserves dates and shareable filters, while project exclusions by key and the
opened attribution project stay on the current page because project keys belong
to the current archive set. A **Clear filters** link appears next to the
refresh button whenever any filter is active.

To narrow the page's stats, click a tile or row in Usage attribution to select
a project, model, or agent. Every tile and row stays visible in its own color,
with your selection highlighted and the rest dimmed. Click the selection again
or choose **Clear selection** in the panel header to clear that dimension while
keeping your dates and other filters. The **Project** and **Model** pickers
reflect the selected item. Selections combine across dimensions and stay active
if you widen the date range; unchecking the selected item in a picker clears
its selection and any opened project view. To inspect a project's groups and
individual sessions for the selected date range, double-click the project or
select it and click **Open** beside **Clear selection** (on touch screens, use
**Open**). Usage outside the opened project's top 100 tiles appears under
**Other**. You can also press Enter or Space to select any item.

For Hermes and Augure Desktop, usage counts model calls recorded in
`session_model_usage`, including background review and title generation.
Main-loop and voice-chat calls count once alongside any main usage recorded
only in the session summary, and each ledger row keeps its recorded model and
billing provider. Main ledger costs follow the session summary's actual,
included, or estimated cost basis, with any positive uncovered cost placed on
the summary model. When the summary cost is unknown, known ledger costs stay on
their models and unknown costs fall back to catalog pricing, while auxiliary
calls use their own recorded cost. Sourced actual and included zero costs
remain known; default zero estimates remain open to catalog pricing. Older
databases without the ledger use session summaries. Malformed ledger rows are
skipped with a warning, and uncovered main usage remains in the session
summary. Session-list token totals still show the session summary.

Hermes cron jobs from different homes stay separate, showing their latest
recorded name in that range and their home-scoped ID on hover. To return to
project tiles with your current dates, filters, and selection intact, click
**All projects** or press Escape or Backspace. Opening a project stays inside
the panel, while browser Back and Forward follow page history. Use the
**Projects** picker to exclude a project. Usage-only and discarded-evidence
archives omit titles and show job IDs, as do archives without `state.db`
titles. Archived Hermes and Augure Desktop cron runs whose sources were gone
before upgrading stay individual sessions unless they already have a stored job
ID.

![A selected project in the cost breakdown](/docs/assets/generated/screenshots/usage-attribution-selected.png)

To analyze token volume instead of spend, switch the toolbar metric from
**Cost** to **Tokens**. The token-type multi-select scopes token totals,
trends, attribution, comparisons, and top-session rankings to any combination
of **Input**, **Cache Writes**, **Cached Read**, and **Output** (all four are
selected by default). Token-type selections are written to the URL, so you can
share an Output-only project or session ranking directly.

Project-key exclusions are the exception. Because shared-store project keys are
scoped to the current aggregate archive set, the page keeps those exclusions in
memory and does not write or restore them through the URL. Project and model
selections also stay in memory, while agent selections use the page's **Agent**
filter.

![Usage toolbar with filters](/docs/assets/generated/screenshots/usage-toolbar.png)

Each filter dropdown supports multi-select with a search box, Select all /
Deselect all shortcuts, and a colored dot for each agent so you can tell them
apart at a glance.

In the **Model** picker, checked models are visible and unchecked models are
hidden, and attribution selections appear in the picker label. Hidden models
stay in the picker after a reload or when you open a shared URL.

Usage saves model visibility with `exclude_model`. The older Usage-only `model`
URL parameter and saved inclusion selections no longer restrict the view; hide
models in the picker instead. The Analytics model filter and API model filters
are unchanged.

![Model filter dropdown](/docs/assets/generated/screenshots/usage-filter-dropdown.png)

### Summary cards

Ten baseline cards at the top summarize the selected window, and the **Total
Cost** card shows its value in larger text. **Total Input** adds uncached
input, cache writes, and cache reads to show the full prompt volume the models
read. **Uncached Input** counts input not reported as a cache read or cache
write, with cache reads noted beneath it. The remaining cards show output
tokens, daily burn, peak day, cache hit rate, project and model counts, and
active days. When Copilot-family sessions have priced usage, an extra
**Copilot AI Credits** card shows the same spend converted at 100 credits per
dollar.

![Usage summary cards](/docs/assets/generated/screenshots/usage-summary-cards.png)

### Cost over time

A stacked chart plots cost per day across the range, grouped by project, model,
or agent. Switch the grouping with the segment buttons in the panel header.
Each series uses the same color as the attribution panel below so you can
cross-reference them. Attribution selections and sidebar session-filter changes
preserve the range selected with the chart's brush.

Pick **Smooth**, **Lines**, or **Bars** in the chart header, and the page
saves that style in your browser. Days with no spend stay visible as zero, and
you can drag across the chart to select a narrower date range. The same
controls work in **Tokens** mode.

![Cost over time chart](/docs/assets/generated/screenshots/usage-cost-trend.png)

![Cost over time shown as bars](/docs/assets/generated/screenshots/cost-chart-bars.png)

### Cost attribution

The attribution panel breaks down total spend for the window into a treemap and
a ranked side rail. Switch the grouping between **Project**, **Model**, and
**Agent**, or switch from **Treemap** to **List** for a table view. Click a
tile or row to select it while keeping every item visible. To view a project's
groups and sessions, double-click it or select it and click **Open**. Use the
**Projects** filter to hide projects.

![Cost attribution treemap](/docs/assets/generated/screenshots/usage-attribution.png)

### Pairwise cost comparison

The Usage page includes **Comparative cost analysis** for side-by-side cost
checks. Pick a dimension (**Project** or **Model**) and value for the left
side, then pick the same or a different dimension and value for the right side.
The comparison uses the page's active date range and shared filters and asks
the backend to compute both slices.

The result table shows total cost, session count, cost per session, total
tokens, tokens per session, uncached input tokens, output tokens, the absolute
delta from left to right, and the percent delta when a ratio can be computed.
This helps answer questions like "how much more expensive was project A than
project B this week?" or "how do two models compare after normalizing by
session count?"

The same comparison is available over REST:

```http
GET /api/v1/usage/pairwise-comparison
```

Pass the normal usage filters along with `left_dimension`, `left_value`,
`right_dimension`, and `right_value`. Supported dimensions are `project` and
`model`. Supply all four side parameters together, or omit all four to use the
server's default pair, which picks the two highest-cost models first, then two
projects, then a model and a project. Each response side includes its
`dimension` and `value`. When too few options exist, both sides return zero
totals and omit `dimension` and `value`.

To fall back to the default pair if either side is missing from the scope's
cost ranking, pass `fallback_to_default=true` along with a full pair. The
response echoes the pair used.

For branch-scoped comparisons, pass the shared `git_branch` usage filter with
an opaque token from `GET /api/v1/branches`.

### Top sessions by cost

This panel lists the most expensive sessions in the window, showing the agent
pill, session name, project, token total, and cost. Click any row to open that
session in the message viewer.

![Top sessions by cost](/docs/assets/generated/screenshots/usage-top-sessions.png)

### Cache efficiency

A stacked bar breaks down cache reads, cache writes, uncached input tokens, and
output tokens, paired with a savings callout showing how much you saved (or
overspent) compared to a no-cache baseline. It helps spot prompts that spend
heavily on cache creation without earning the reads back.

![Cache efficiency panel](/docs/assets/generated/screenshots/usage-cache-efficiency.png)

Because the dashboard reads from the same `model_pricing` table that backs the
CLI commands below, its numbers match exactly what
`agentsview report usage daily` prints.

### PostgreSQL-backed usage

Since 0.23.0, the Usage page and usage API also work when the UI is served from
PostgreSQL with [`agentsview replica pg serve`](/docs/pg-sync/). Shared or
multi-machine deployments can browse the same cost and token dashboards without
relying on a local SQLite archive.

Live SSE refresh still applies only to the normal local `serve` runtime;
`pg serve` is read-only and does not expose the global live event stream.

## Quick tour

```bash
# Last 30 days of spend as a terminal table
agentsview report usage daily

# Full history, with per-model breakdown rows
agentsview report usage daily --all --breakdown

# JSON output for scripting
agentsview report usage daily --json > spend.json

```

Examples:

```
> agentsview report usage daily --since 2026-04-01
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
> agentsview report usage statusline
$195.64 today
```

The daily table lists input, output, cache-creation, and cache-read token
totals for each local-time day, along with the estimated cost and the models
that contributed to that row. Pass `--breakdown` to print indented per-model
sub-rows so you can see which model drove spend each day. In JSON output,
`--breakdown` also populates per-project, per-agent, and per-machine breakdown
arrays for every day, so you can separate costs from shared multi-machine
archives without a second query.

On large archives, AgentsView serves aggregate usage reads from a local cache
of daily totals that stays exact as sessions sync. The first query after an
install, upgrade, or cache deletion takes longer while that cache builds; later
queries are fast.

## How costs are computed

Every message parsed from a session file stores its raw `token_usage` JSON
(input, output, cache creation, cache read) and the model name the agent
reported. When you run a usage command, AgentsView does the following:

1. Loads `model_pricing` and its `model_pricing_bands` children into memory
   once per invocation. These tables hold base per-million-token rates and any
   whole-request input thresholds published by the pricing catalog.
1. Scans `messages` filtered by the requested date range and agent, parsing
   each row's `token_usage` blob in Go with `gjson`, which is faster than
   SQLite's per-row `json_extract`.
1. Selects the applicable complete rate tuple for each request, multiplies that
   row's tokens by the selected rates, and buckets and aggregates the result.

Message usage and usage events tied to a `message_ordinal` represent individual
requests. For those rows, AgentsView sums uncached input, cache creation, and
cache-read input, then picks the highest catalog band whose threshold is
strictly below that total. Because the threshold is exclusive, exactly 272,000
tokens keep the 272K base rate, while 272,001 tokens select the band. Unbound
usage events can summarize several requests, so they conservatively use the
base rate instead of applying a threshold to an aggregate.

The default window is the last 30 days. To scan your full history, pass
`--all`.

!!! note

    AgentsView does not mint usage events on your behalf. It can only report
    token usage that the agent wrote to its own session files. Agents that do
    not emit token counts, or that strip them from local logs, do not show up.

### Pricing source

Model rates come from the
[LiteLLM model pricing catalog](https://github.com/BerriAI/litellm), with the
[OpenRouter model catalog](https://openrouter.ai/docs/api/api-reference/models/get-models)
layered underneath for models LiteLLM does not list. AgentsView fetches both
together (at most hourly on `usage` invocations, and daily in the server) and
merges them into the `model_pricing` table. When both catalogs list a model,
LiteLLM's rate wins, and an OpenRouter row is dropped once LiteLLM adds the
model. If the LiteLLM fetch fails, AgentsView keeps the last stored rates; if
only the OpenRouter fetch fails, it stores the fresh LiteLLM rates and keeps
the previously stored OpenRouter rates. Fresh databases start from an embedded
LiteLLM snapshot so offline use works before any refresh finishes. To skip the
fetch and always use the embedded fallback, pass `--offline`.

The embedded fallback updates with each AgentsView release, so its numbers are
as current as your installed version. For up-to-the-minute rates, leave
`--offline` off.

Both fetched and embedded catalogs preserve LiteLLM's standard whole-request
200K and 272K token rates. Processing variants such as Batch, Flex, regional,
Ultrafast, and one-hour cache pricing are not inferred when stored usage does
not identify that service tier. Codex Fast is the exception, described below.

### Costs the agent records itself

Some agents calculate a cost from their own price table and write it into the
session. AgentsView stores that figure as a recorded cost, and uses it as the
price only when its own catalog has no rate for the model. When the catalog can
price the model, AgentsView uses the catalog price and keeps the recorded cost
for comparison. A recorded cost of $0 never prices a row because agents write
$0 for models they have no rate for, so those rows stay unpriced.

AgentsView records these agent-supplied costs from Pi and the agents built on
it, OpenClaw, QClaw, Shelley, fx, and OpenCode, Kilo, and MiMo Code. For the
OpenCode family, it sums the cost of each model request in a message. Some of
these figures are what a provider or gateway billed rather than an estimate,
and they follow the same rule. For fx, costs are kept only when fx has a
billing record for every request. Existing sessions from these agents are
re-read once after you upgrade.

Run `agentsview session usage <id>` to compare the two figures. An
`Agent cost:` line (and a `recorded_cost` object in JSON) shows what the agent
recorded next to what AgentsView calculated for the same rows. A large gap
usually means the agent used different rates or the transcript is missing calls
the agent billed.

Claude Code also writes a running total for each session with its own cost
estimate and token counts per model, including subagents and side calls such as
title generation that never reach the transcript. AgentsView keeps the latest
one and displays it as an `Agent total:` line and a `session_total` object.
That figure is never added to usage totals because it overlaps the per-call
rows. Compare it against a report that includes subagents (the default), and
expect it to sit slightly above AgentsView's figure in short sessions where the
title call is a larger share of the work.

Agents whose cost AgentsView already uses as the price (such as Junie, gptme,
Codebuff, Hermes, Goose, and Crush) keep that treatment: their reported cost is
the price.

When a usage record carries a cost but names no model, AgentsView counts it
under the `<unknown>` model, the same label Cursor account imports use. Its
cost and tokens appear in totals and breakdowns instead of being dropped.

### Anthropic web search fee

Anthropic charges a flat
[$10 per 1,000 server-side web search requests](https://docs.claude.com/en/docs/agents-and-tools/tool-use/web-search-tool)
on top of tokens. Because that fee is not a per-token rate, it lives outside
the `model_pricing` catalog as a fixed $0.01 per request. AgentsView adds it to
any usage row whose stored `token_usage` reports
`server_tool_use.web_search_requests`, and `session usage --format json`
includes the per-row count as `web_search_requests` so you can audit the
charge. Rows with an authoritative reported cost are left alone because that
cost already settles the whole row.

Claude Code runs each `WebSearch` in an out-of-band side call and writes a zero
counter on the assistant message, so AgentsView reads the count from the linked
tool result instead. That side call's own tokens, tens of thousands of input
tokens on a Haiku model per search, are not written to the transcript at all
and are therefore a known undercount: AgentsView records the fee, not the
hidden tokens.

Since 0.32.0, the embedded fallback includes `claude-opus-4-7` at the same Opus
tier used for 4.6 and 4.8, so offline reports and fresh installs price Opus 4.7
sessions without waiting for a live LiteLLM fetch. Version 0.33.0 adds
`claude-fable-5` at its launch rates ($10 input, $50 output, $12.50 cache
creation, and $1 cache read per million tokens).

### Custom model pricing

Since 0.24.0, you can supply per-million-token rates for models missing from
the LiteLLM catalog, or override catalog rates for models that are listed. Add
`[custom_model_pricing.<model>]` tables to `~/.agentsview/config.toml`:

```toml
[custom_model_pricing."acme-ultra-2.1"]
input_microdollars_per_mtok = 2_000_000
output_microdollars_per_mtok = 8_000_000
cache_creation_microdollars_per_mtok = 2_500_000
cache_read_microdollars_per_mtok = 200_000

[custom_model_pricing.internal-tiny]
input_microdollars_per_mtok = 200_000
output_microdollars_per_mtok = 800_000
```

| Field                                     | Description                                                                                                                                                 |
| ----------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `input_microdollars_per_mtok`             | Integer microdollars per million input tokens (defaults to `0` if omitted)                                                                                  |
| `output_microdollars_per_mtok`            | Integer microdollars per million output tokens (defaults to `0` if omitted)                                                                                 |
| `cache_creation_microdollars_per_mtok`    | Integer microdollars per million cache-creation tokens (optional)                                                                                           |
| `cache_creation_1h_microdollars_per_mtok` | Integer microdollars per million 1-hour-TTL cache-creation tokens (optional; when omitted or `0`, 1h writes bill at `cache_creation_microdollars_per_mtok`) |
| `cache_read_microdollars_per_mtok`        | Integer microdollars per million cache-read tokens (optional)                                                                                               |
| `price_as`                                | Target model whose rates and tiers this name uses; excludes rate fields                                                                                    |

Set the table key to the model name as it appears in your session data (match
the exact string the agent writes, dots and all, and quote the key if it
contains special characters). Custom rates override LiteLLM, OpenRouter, and
the embedded fallback across the Usage dashboard, `agentsview report usage`,
and `pg serve`. An entry with explicit rates replaces the entire rate row for
that model, so omitted fields count as zero instead of falling through to
another catalog. Explicit rates are flat and suppress fetched request bands for
that model, while models without a custom entry keep resolving through the
stored catalog.

To map a deployment name to a catalog model's rates and future updates, use
`price_as`:

```toml
[custom_model_pricing."gateway-deployment"]
price_as = "gpt-6-sol"
```

A bare key also matches a provider-prefixed name, while a key containing `/`
matches only that exact name. An exact custom rate takes precedence. Each entry
resolves its target once and uses that target's pricing ladder, including any
custom rate. Unknown targets stay unpriced, and empty rate tables set all rates
to zero. Restart AgentsView after changing mappings, and use the same
configuration on every machine that shares a mirror.

### Model aliases

You can group variants of a model under one name in Usage and Analytics. Add
quoted model keys to `~/.agentsview/config.toml`, then restart AgentsView:

```toml
[model_aliases]
"qwen/qwen3.6" = "qwen3.6"
"qwen3.6-mlx" = "qwen3.6"
```

Usage combines both variants as `qwen3.6` and adds their tokens and costs
together. Analytics gets the grouped **Model** filter, though it has no
per-model rows. Each variant keeps its own pricing before totals are combined.
Keys and names must be non-empty, cannot have surrounding whitespace or commas,
and cannot form alias chains or use the reserved names `__all__` and
`__other__`.

Plan-only usage counts each session once per model group, even if the session
uses several variants in that group.

A saved filter naming one model variant keeps its one-variant meaning after you
configure an alias, and the picker reflects that selection on the group row.
The selected variant names appear on hover and are available to screen readers.
On Analytics, the trigger also shows the selected variant names; click the row
once to clear those variants, and click again to apply the whole group. On
Usage, excluding individual variants keeps the group row ticked and shows the
hidden-variant count on the trigger. Even when you exclude every configured
variant, a raw model sharing the group's name can still have visible usage.
The row appears unticked only when you exclude the group name. Clicking the
ticked row excludes the whole group and replaces its individual variant
exclusions; clicking the unticked row removes the group exclusion. Grouped
summary responses include the `model_aliases` map used to display these
selections.

Model filters expand group names on `/api/v1/usage/summary`,
`/api/v1/usage/summary/stream`, `/api/v1/usage/comparison`,
`/api/v1/usage/pairwise-comparison`, `/api/v1/usage/top-sessions`, and the
`/api/v1/analytics/*` endpoints. A raw member name selects only that variant
unless it is also a group name, in which case it selects the whole group. API
callers can request grouped report rows with `group_models=true` on usage
endpoints. On the analytics summary, that flag adds `model_aliases` for the
picker while keeping the model list raw. When aliases are configured, grouped
usage summaries omit `schema_version` and `pricing`. Without the flag,
responses, `usage daily`, exports, and MCP keep raw model names and raw
`pricing.models` entries.

### Posit Assistant billing

For usage rows whose stored provider ID is exactly `positai`, AgentsView estimates
computed token costs and cache savings at 110% of the selected catalog rates.
Model, timestamp, and request-band selection happen before that adjustment.
Rows with empty or other provider IDs, including observed `anthropic`
bring-your-own-provider rows in the same session, use base rates.

Custom pricing overrides are not adjusted. Explicit reported costs stay
authoritative, and fixed web-search fees stay at their published face value.

### Codex Fast billing

Codex messages whose latest `thread_settings_applied` event records provider
`openai` and service tier `priority` are estimated at LiteLLM's `_priority`
prices. This reflects the selected tier rather than the tier Codex sent on
every request. Because Codex can omit priority for a model that does not
advertise support, the estimate can exceed the billed cost. Fast bands take
precedence at matching thresholds. A standard input threshold fills a gap; its
published companion rates supply fields missing from that Fast band, and other
fields use the Fast base rate. If neither row publishes bands, Fast base rates
apply at every request size. Ultrafast and other recorded tiers use standard
rates, as do rollouts without a tier record. Forks can inherit the parent's
last recorded tier through replay until the child records a new setting. The
embedded catalog includes tier prices for offline use, and any model without a
price for its tier (or with a custom override) keeps its standard rates.

Because the catalog does not keep historical Fast prices, LiteLLM's current
Fast prices apply to every session date.

### Copilot CLI token metrics

Since 0.32.0, Copilot CLI sessions contribute to usage and cost reports. When
available, per-call usage from `session-store.db` supplies input, output,
cache, and reasoning counts for sessions starting June 1, 2026 or later.
Otherwise, `session.usage_record` receipts supply those counts at each
request's own timestamp, including for sessions without a shutdown summary.
Store rows replace all `session.shutdown.modelMetrics` rollups. When using
receipts, AgentsView sums shutdown token rows from before the first usable
receipt, as it does for sessions without receipts, and drops shutdown token
rows written after that receipt because they are cumulative snapshots of what
receipts already record. The published
[Copilot CLI 1.0.105 schema](https://registry.npmjs.org/@github/copilot-linux-x64/-/copilot-linux-x64-1.0.105.tgz)
describes `tokenDetails` and `totalNanoAiu` as "Session-wide accumulated" and
`accountingSnapshot` as "captured atomically with receipt watermarks", and it
defines `session.usage_checkpoint` for "reconstructing aggregate accounting on
resume". Without either per-call source, AgentsView sums shutdown token
metrics, then known `assistant.message.outputTokens`. That message-only
fallback cannot report input, cache, reasoning, or Copilot AI Credit totals.
Fresh input tokens equal total input minus cache reads and cache writes, with
cache writes mapped to cache-creation tokens and cache reads mapped to
cache-read tokens. Because Copilot's Claude model IDs use dotted version
numbers, the parser normalizes names such as `claude-sonnet-4.6` to
`claude-sonnet-4-6` before pricing lookup.

Store updates refresh the affected sessions without rereading unchanged
transcripts. Overlapping transcript output contributes only its positive
per-model difference from the selected per-call totals; missing input cannot be
reconstructed. Per-call tokens use request-level catalog estimates. The latest
shutdown reported cost keeps the treatment described below, unless a later
receipt makes that total stale and receipts are the selected source.

Upgrading to 0.32.0 bumps the parser data version so existing Copilot CLI
sessions re-index with the new usage rows.

Some Copilot CLI records include `session.shutdown.modelMetrics` totals without
per-message `assistant.message.outputTokens`. AgentsView can still use complete
aggregate model totals from those records, though it cannot reconstruct
per-message output-token rows. Since 0.35.1, the CLI reports that limit
directly instead of implying that the session has no usage data at all.

### VS Code Copilot token metrics

Since 0.34.0, VS Code Copilot chat sessions contribute when their persisted
request metadata includes token counts. AgentsView reads `promptTokens`,
`outputTokens`, and the resolved model from the session payload. Because VS
Code reports prompt tokens as a single total, AgentsView treats them as input
tokens and does not split out prompt-cache discounts unless the source log
exposes them separately.

Upgrading to 0.34.0 re-indexes existing VS Code Copilot sessions so historical
chats pick up the newly available usage rows.

### Visual Studio Copilot token metrics

Since 0.34.0, Visual Studio Copilot traces contribute when OpenTelemetry spans
include `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, and model
attributes. AgentsView deduplicates repeated trace flushes for the same chat
turn and keeps the copy with the most complete token usage before pricing it.

### Copilot reported billing

Copilot CLI `session.shutdown` events can include a `totalNanoAiu` billing
total. Copilot sessions starting on or after June 1, 2026 can report an
authoritative `totalNanoAiu` billing total, while older sessions stay
catalog-priced because they were created under the premium-request pricing
model. AgentsView converts that cumulative amount exactly
(`totalNanoAiu / 1e11` USD) and stores the session-level value in the normal
reported-cost fields on one stable usage-event row. Later shutdowns supersede
earlier totals, including an authoritative final zero.

When the selected data contains this reported session cost, AgentsView
suppresses every catalog-priced estimate for that session to avoid double
counting across models, days, and resumed segments. Historical Copilot sessions
without `totalNanoAiu` and other Copilot-family agents keep catalog pricing.

For unfiltered reports, AgentsView allocates the session total across the
selected usage rows in proportion to their catalog-price estimates. That keeps
per-day and per-model breakdowns additive, so their costs sum to the displayed
session or report total. In a multi-model session, those model costs are
estimated attributions, and Copilot reports only the session total rather than
a charge for each model.

A date window containing only rows before the reported settlement can still
show catalog estimates because the later session total falls outside the
selected data. Model-filtered reports also stay catalog estimates because
applying the full session total to one selected model would overstate that
model.

Comma-separated usage filters trim spaces and ignore blank entries, and blank
model filters keep the reported session total.

### Copilot AI Credits

Usage reports compute **Copilot AI Credits** for Copilot-family agents
(`copilot`, `vscode-copilot`, and `visualstudio-copilot`) whenever their usage
rows have a complete cost. The conversion divides cost by `$0.01`, matching the
unit AgentsView uses for Copilot credit reporting. When a session carries an
authoritative Copilot-reported cost, credits derive from that reported total;
otherwise they derive from the catalog estimate. The Usage dashboard displays
this as an optional summary card, `agentsview session usage` prints an
`AI Credits` line for priced Copilot-family sessions, and usage report totals
expose the `copilotAICredits` field.

### Claude streaming & Codex token events

The 0.20.0 cost tracking release also improved how AgentsView extracts raw
token usage so the input side is accurate:

- **Claude Code.** Streaming deltas previously wrote the same token counts
  multiple times, roughly doubling input totals. The parser now deduplicates
  them.
- **Codex.** Per-request `token_count` events embedded in `event_msg` entries
  are now captured, so Codex sessions have populated token usage where they
  previously reported zero.

If you upgrade from an earlier version, the first `usage` invocation triggers a
full resync so these fixes apply to historical sessions.

Codex also records prompt-cache writes inside its input count. AgentsView moves
them into the cache-creation bucket so GPT-5.6 and later writes price at the
cache-write rate. Hosted raw archives pick up the change through the existing
[`pg raw-reparse hosted`](hosted-raw-sync.md#reparse-and-rollback) workflow.

### Amp token metrics

Amp thread documents carry a `usage` object on each assistant message with the
model and its input, output, cache-creation, and cache-read token counts.
AgentsView reads them per inference, so a thread that switches models keeps
each model's own tokens instead of collapsing to one.

Amp routes every prompt token into one of three input buckets. Anthropic-backed
threads already use Anthropic's cache semantics and are read as-is.
OpenAI-backed threads report `inputTokens` as zero and classify the entire
uncached prompt as cache creation; because that bucket is the full uncached
prompt rather than an OpenAI-reported cache write, AgentsView records those
tokens as uncached input and emits no cache-creation bucket.

Keep two limits in mind. First, older threads can omit the model entirely.
Because usage reporting counts only rows that carry a model, those inferences
are left out of daily totals and model breakdowns instead of appearing at a
guessed rate, though their tokens are still stored and visible in the session's
own usage view. Second, Amp's exports record only main-thread inference, so
tokens spent by `oracle`, `librarian`, and other subagents do not appear in a
thread's totals.

Recent Amp versions keep complete threads server-side and leave only stub files
on disk. AgentsView reports usage for whatever complete threads exist locally;
stub-only threads have nothing to read.

### Direct API sessions

Tools that call model APIs directly, with no coding agent in between, can
record their token usage as **Direct API** sessions. The tool writes one
body-free JSONL file per session (never prompts, documents, or replies), with
one JSON object per line. After the header, each line's `parentId` is the `id`
of the line before it, and the first line after the header has a null
`parentId`:

1. Header, with a UUID4 `id`, an ISO `timestamp`, and the absolute `cwd`:
   `{"type":"session","version":3,"id":<uuid4>,"timestamp":<iso>,"cwd":<cwd>}`
2. Optional name, with an 8-hex `id`:
   `{"type":"session_info","id":<8 hex>,"parentId":null,"timestamp":<iso>,"name":<name>}`
3. One line per model call, with an 8-hex `id` and `timestamp` in epoch
   milliseconds inside `message`:
   `{"type":"message","id":<8 hex>,"parentId":<previous line id>,"timestamp":<iso>,"message":{"role":"assistant","provider":<provider>,"model":<model>,"content":[],"usage":{"input":I,"output":O,"cacheRead":C,"cacheWrite":0,"totalTokens":I+O+C},"timestamp":<epoch ms>}}`

`input` excludes cached prompt tokens and `cacheRead` holds the cached prompt
tokens, so `totalTokens` equals `input + output + cacheRead`. AgentsView prices
the tokens from the catalog.

By default, AgentsView reads files from `~/.agentsview/direct-api/`. To read a
different directory, set `DIRECT_API_DIR`, or configure `dirs` under
`[agents.direct-api]` in the
[config file](configuration.md#multiple-directories).

## Reporting model

`agentsview report usage` reads token and cost facts from the same SQLite
archive as the UI and joins them with the applicable pricing data.

Version 0.42.0 also maintains an exact daily rollup cache for repeated reports.
The cache is disposable rather than authoritative: AgentsView rebuilds any
missing or stale day from the archived usage rows before returning it. This
reporting workflow also:

- **Works beyond Claude Code.** Coverage includes Claude Code, Codex, Copilot
  CLI, OpenCode-format tools, Pi, Prime Agent, Gemini, Qwen Code,
  OpenClaw/QClaw, Hermes, WorkBuddy, Forge, Piebald, Antigravity, Zed, VS Code
  Copilot, Visual Studio Copilot, Mistral Vibe, and gptme from the same
  database and command whenever those sessions log token metadata. Pass
  `--agent <name>` to view a single agent.
- **Shares one database with the UI.** The same data powers
  [Analytics](/docs/usage/#dashboard) and session detail views, so there is no
  second index to keep fresh.
- **Reads committed archive data.** Daily reports can run while the daemon
  imports new source changes. Run `agentsview source sync` first if those
  changes must be included. Statusline finishes pending startup ingestion
  before querying, while session usage attempts a targeted refresh by default
  and can return saved totals while startup is pending.

<a id="agentsview-usage-daily"></a>

## `agentsview report usage daily`

Run this command for a daily cost report. It prints a tab-aligned table to
stdout by default, or JSON if you pass `--format json` (or `--json`).

When the cache is cold, the command prepares the sessions needed for the
report. Slow reports print preparation phases and elapsed time to stderr, even
when you request JSON output. Usage reports can take longer than the server's
normal write timeout. Press Ctrl+C to stop waiting; shared cache preparation
can keep running in the daemon, and canceling daemon startup leaves the
detached child running.

Full parser rebuilds continue serving the retained archive while rebuilding,
and CLI reports print a stale warning to stderr. Scoped upgrades serve saved
rows while affected agents reparse, so reports can use those saved rows. Schema
repairs finish before the daemon serves requests. While waiting for repairs at
startup, the CLI displays phases and keeps waiting as
long as progress updates arrive, including when a foreground server starts in
another terminal. Restart older daemons after upgrading so the usage progress
endpoint is available.

```bash
agentsview report usage daily [flags]
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
| `--offline`   | `false`       | Read the archive directly without sync or pricing fetches                |
| `--no-sync`   | `false`       | Skip source refresh; a new daemon starts without automatic sync          |
| `--timezone`  | system        | IANA timezone name used for date bucketing                               |

The default 30-day window applies only when you omit `--since`, `--until`, and
`--all`. Passing only `--until` leaves the start open so you can query
everything up to that date.

**JSON shape:**

```json
{
  "schema_version": 6,
  "pricing": {
    "source": "fetched",
    "table_version": "2026-07-03T12:00:00Z",
    "latest_row_updated_at": "2026-07-03T12:00:00Z",
    "custom_override_count": 0,
    "effective_row_count": 2432,
    "digest": "sha256:8d815a1737bce68fa1a19ba977bf33c8c8efcc74deb954fcf62ce80e46e75f2c",
    "cost_source": "mixed",
    "fallback": {
      "used": false,
      "models": []
    },
    "models": {
      "gpt-5.4": {
        "cost_source": "computed",
        "resolutions": [
          {
            "priced_model": "gpt-5.4",
            "matched_pattern": "gpt-5.4",
            "input_cost_per_mtok": {"microdollars": 2500000},
            "output_cost_per_mtok": {"microdollars": 15000000},
            "cache_write_cost_per_mtok": {"microdollars": 0},
            "cache_write_1h_cost_per_mtok": {"microdollars": 0},
            "cache_read_cost_per_mtok": {"microdollars": 250000},
            "cost_source": "computed",
            "bands": [
              {
                "above_input_tokens": 272000,
                "input_cost_per_mtok": {"microdollars": 5000000},
                "output_cost_per_mtok": {"microdollars": 22500000},
                "cache_write_cost_per_mtok": {"microdollars": 0},
                "cache_write_1h_cost_per_mtok": {"microdollars": 0},
                "cache_read_cost_per_mtok": {"microdollars": 500000}
              }
            ],
            "application": {
              "base_request_count": 14,
              "aggregate_row_count": 0,
              "bands": [
                {"above_input_tokens": 272000, "request_count": 2}
              ]
            }
          }
        ]
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
  },
  "machine_labels": {
    "build-host": "Build Host"
  }
}
```

`modelsUsed` is sorted by cost within each day, so the most expensive model
appears first. Daily entries always emit `modelBreakdowns`,
`projectBreakdowns`, `agentBreakdowns`, and `machineBreakdowns` as arrays;
empty breakdowns appear as `[]` rather than being omitted. `modelBreakdowns`
always includes one row per model. The other three arrays populate when you
pass `--breakdown`, which also enables per-model sub-rows in the terminal
table. With `--breakdown`, `machine_labels` maps each
`machineBreakdowns[].machineName` key to its display label and includes only
keys present in the report. If the archive has no labels or the catalog read
fails, the map is `{}` (failures write a warning to stderr and the report
continues). Without `--breakdown`, `machine_labels` is omitted and the command
does not read the catalog. Adding this field does not change `schema_version`.

### JSON contract

`agentsview report usage daily --json` is a versioned JSON surface. The Usage daily
JSON, Activity report JSON, and session summary export JSON/NDJSON are separate
versioned surfaces, so a future bump in one does not imply a bump in the others.
Usage and activity already emitted `schema_version: 1` before 0.38, and the
session-summary v1 contract shipped in 0.37.1. Releases 0.38.0 and 0.38.1
emitted the substantially revised project-evidence shape while still reporting
version 1. Version 2 corrected those markers, version 3 introduced exact
microdollar money objects, and version 4 adds resolved-model pricing provenance
with request-pricing bands and application counts. Version 5 selects complete
Claude snapshots before generic deduplication and charges the maximum observed
server-side web-search count. Versions 4 and 5 are contract bumps because
version 4 changed band-selection pricing semantics and digest canonicalization,
while version 5 changes snapshot and server-tool accounting. Version 6 applies
provider-specific billing identity to computed usage and preserves reported
costs and custom pricing overrides; consumers must not treat v5 and v6 costs as
interchangeable. The two transitional releases must not be treated as
v1-compatible. The commands do not provide an earlier-version output mode.
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

Additive fields may appear in future compatible payloads. Consumers should
ignore unknown keys.

### Pricing provenance

Versioned usage, activity, and session-export payloads include a report-level
`pricing` block. `pricing.models` is nested under that block and is keyed by the
distinct model names reported in payload rows, not by canonical pricing names or
every row in the pricing table. Each reported-model entry has an aggregate
`cost_source` and a `resolutions` array. A resolution reports `priced_model`,
`matched_pattern`, `input_cost_per_mtok`, `output_cost_per_mtok`,
`cache_write_cost_per_mtok`, `cache_write_1h_cost_per_mtok`,
`cache_read_cost_per_mtok`, `cost_source`, `bands`, and `application`.
`cache_write_1h_cost_per_mtok` is the 1-hour-TTL cache-write rate; zero means
the catalog publishes no separate 1h rate and 1h writes bill at
`cache_write_cost_per_mtok`. Resolutions are sorted by `priced_model` and then
`matched_pattern`.

Each `bands` item is a complete rate tuple with an exclusive
`above_input_tokens` threshold. `application` reports how computed rows in this
payload used those rates:

- `base_request_count` counts request-scoped rows that selected no band.
- `aggregate_row_count` counts unbound aggregate rows forced to base rates.
- `bands` lists each selected threshold and its `request_count`.

Reported-only rows do not increment application counts. These counts preserve
the difference between one above-threshold request and several smaller requests
after their tokens and costs have been aggregated.

When no catalog bands or applied bands exist, their canonical JSON value is
`null`, not `[]`. Consumers should accept future additive fields but can rely on
that null-versus-nonempty-array representation within schema version 6.

Ordinary models have one resolution whose `priced_model` is the reported model.
Fixed aliases such as `k2d6-agent`, `gpt-reserve`, and `codex-auto-review` keep
that reported name and resolve `priced_model` to a catalog row
(`moonshot/kimi-k2.6`, and `gpt-5.6-luna` for both Codex names).
Timestamp-aware aliases can have more than one resolution in a report. For
example, one `kimi-for-coding` entry can contain both `moonshot/kimi-k2.6` and
`kimi-k3` resolutions when its rows span the pricing cutoff. An exact
custom-pricing row for the reported alias takes precedence before timestamp
canonicalization and produces one self-resolution. Fallback provenance also
lists reported model names.

`cost_source` is a closed enum everywhere it appears: `computed`, `reported`, or
`mixed`. It was established in schema version 2 and is unchanged in version 6.
`computed` means AgentsView derived cost from token counts and the effective
pricing resolver. `reported` means a source supplied explicit cost, such as
Cursor Admin API billing data; those amounts may not be derivable from tokens
times rates. `mixed` means the enclosing report, reported-model entry, or rollup
contains both provenance kinds. Each resolution carries its own source, while
the reported-model entry aggregates its resolutions. For computed costs,
reasoning tokens are priced at the output-token rate.

An authoritative session total is never added to that session's computed
estimate. It replaces the estimate completely. A report-level pricing block can
still be `mixed` when it records that authoritative settlement alongside
computed per-model attribution. Those model allocations are estimates derived
from catalog-cost weights and sum to the reported total. A multi-session rollup
can also be `mixed` when different sessions use different sources.

If a source reports an amount for a model with no matching effective pricing
row, its model entry and resolution have `cost_source: "reported"`, the
resolution has `matched_pattern: null`, and all four rate fields are zero. The
reported amount remains authoritative; the zero rates express unavailable rate
provenance, not a zero-rate calculation.

A computed resolution with `matched_pattern: null` means no effective pricing
row matched the model. Its rates are zero, so its tokens add nothing to the
computed cost; add a `[custom_model_pricing]` row to price it.
`pricing.fallback.models` lists models priced from the embedded catalog, not
unpriced ones.

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
`cache_write_per_mtok`, `cache_read_per_mtok`, `source`, `updated_at`, and
`bands`. Bands are sorted by `above_input_tokens`; each canonical band contains
exactly `above_input_tokens`, `input_per_mtok`, `output_per_mtok`,
`cache_write_per_mtok`, `cache_read_per_mtok`, and `updated_at`. Application
counts describe one report and are deliberately excluded from the catalog
digest. `updated_at` is `null` or a UTC RFC3339 timestamp such as
`2026-07-03T12:00:00Z`. Digest canonicalization errors fail the export instead
of emitting an empty digest. The digest uses the resolver's internal canonical
pricing-row keys; the public `pricing.models` block uses the `*_cost_per_mtok`
field names shown above. The reported-to-priced resolution mapping is not part
of the pricing-table digest.

### Project identity

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

SQLite catalog keys are archive-scoped. Shared PostgreSQL and ClickHouse
dashboard responses may aggregate archives, so their catalog keys are response-scoped and
must not be persisted as durable selectors. The canonical `identity.key`, not
the display label or catalog key, represents project continuity.

<a id="agentsview-usage-statusline"></a>

## `agentsview report usage statusline`

Print today's spend on a single line for shell prompts, tmux status lines, and
window titles. The command has a 30-second deadline covering startup and the
report request, and returns an error if that deadline expires.

```bash
agentsview report usage statusline [flags]
```

| Flag        | Default | Description                                      |
| ----------- | ------- | ------------------------------------------------ |
| `--format`  | `human` | Output format: `human` or `json`                 |
| `--json`    | `false` | Alias for `--format json`                        |
| `--agent`   |         | Filter by agent name                             |
| `--offline` | `false` | Read the archive without sync or pricing fetches |
| `--no-sync` | `false` | Skip source refresh                              |

Default output is a single line:

```
$9.61 today
```

With `--agent claude`:

```
$6.42 today (claude)
```

Pass `--json` to emit the same values for scripts, with cost in exact
microdollars instead of a formatted string:

```json
{
  "date": "2026-08-04",
  "cost": {
    "microdollars": 9610000
  }
}
```

When you filter with `--agent`, the JSON object includes `agent`.

This command always reports the current local-time day. To get the full row
instead, run `agentsview report usage daily --since $(date +%Y-%m-%d)`.

<a id="agentsview-usage-cursor"></a>

## `agentsview source cursor sync-usage`

Import Cursor Admin API usage events into your local archive so they appear in
the Usage dashboard and daily reports.

```bash
agentsview source cursor sync-usage [flags]
```

| Flag          | Default       | Description                                              |
| ------------- | ------------- | -------------------------------------------------------- |
| `--since`     | `30 days ago` | Start date (`YYYY-MM-DD`), inclusive                     |
| `--until`     | today         | End date (`YYYY-MM-DD`), inclusive                       |
| `--all`       | `false`       | Include all history; overrides the default 30-day window |
| `--page-size` | `100`         | Cursor Admin API events requested per page               |
| `--email`     | config        | Filter by Cursor team member email                       |
| `--user-id`   | config        | Filter by Cursor team member user ID                     |

An API key is required. Set `cursor_admin_api_key` in
`~/.agentsview/config.toml` or export `AGENTSVIEW_CURSOR_ADMIN_API_KEY`. You
can also set optional default member filters with `cursor_admin_email` and
`cursor_admin_user_id` or their matching environment variables.

### Example: Starship prompt module

```toml
# ~/.config/starship.toml
[custom.agentsview]
command = "agentsview report usage statusline --offline --no-sync"
when = "true"
format = "[$output]($style) "
style = "bold green"
```

The offline flags read the existing archive without starting a daemon or
syncing source files, leaving a separate `agentsview serve` process or periodic
`agentsview source sync` to keep the archive fresh. Cache preparation can still
take time.

## Source freshness

`usage daily` reads committed archive data. When it starts a daemon, routine
source ingestion runs after the daemon is ready. If your report must include
new source changes, run `agentsview source sync` first. Full parser rebuilds
continue serving the retained archive with a stale warning. Scoped upgrades
serve saved rows while affected agents reparse. Schema repairs finish before
the daemon serves requests.

`usage statusline` waits for pending startup ingestion to finish before
querying, even when an earlier daily report started the daemon. After startup
ingestion finishes, subsequent queries do not repeat a full sync. The daemon's
file watcher handles later source changes. During a full parser rebuild,
statusline skips startup ingestion and reads the retained archive with a stale
warning.

By default, `session usage` (and its `token-use` alias) tries to refresh known
sessions without waiting. To wait for startup or a rebuild before refreshing,
pass `session usage --wait` (known sessions can still refresh during catch-up).
Missing sessions are retried until the wait expires: a confirmed missing ID
exits with code 2 when status is clear, while an incomplete lookup, pending
work, or unavailable status returns a retry error. Passing `--own-only --wait`
can start deferred ingestion before reading the session's own archived usage,
and combined totals include archived descendants. See the
[session usage contract](/docs/session-api/#agentsview-session-usage) for
refresh behavior, missing-session exit codes, and the `refresh_incomplete` JSON
field.

`--no-sync` skips source refresh and starts any new daemon without automatic
sync, though it does not disable sync on a daemon that is already running.
`--offline` reads the archive directly without starting a daemon, syncing
sources, or fetching pricing. When daemon autostart is disabled, a direct
online query still runs its incremental refresh before reading. Read-only
mirror daemons do not refresh the local SQLite archive, so stop them before
running a local online usage report.

To investigate an archived session's cost, run `session diagnose <id>`, which
combines usage with context growth, compactions, large tool results, retry
candidates, and child links. It uses the same source refresh as
`session usage`. See the
[diagnosis contract](https://agentsview.io/docs/session-api/#agentsview-session-diagnose)
for coverage limits.

## Scripting examples

**Monthly spend for the current month.**

Costs come back as `money` objects, so read `.microdollars` and divide to get
dollars:

```bash
agentsview report usage daily \
  --since "$(date +%Y-%m-01)" \
  --json \
  | jq '.totals.totalCost.microdollars / 1000000'
```

**Per-agent totals for the last 7 days.**

Because `date` arithmetic differs between BSD (macOS) and GNU (Linux), this
snippet tries the BSD form first and falls back to GNU:

```bash
since=$(date -v-7d +%Y-%m-%d 2>/dev/null \
  || date -d '7 days ago' +%Y-%m-%d)

for a in claude codex copilot gemini; do
  total=$(agentsview report usage daily \
    --since "$since" \
    --agent "$a" \
    --json 2>/dev/null \
    | jq -r '.totals.totalCost.microdollars / 1000000 | tostring')
  printf "%-8s  \$%s\n" "$a" "$total"
done
```

**Alert when today crosses a budget.**

This script writes to stderr and exits non-zero so you can connect it to
whatever notifier fits your OS, such as cron's `MAILTO`, launchd's
`StandardErrorPath`, a systemd timer's journal, or a Windows Task Scheduler
action.

Because `--json` reports the cost in exact microdollars, you can compare
integers instead of parsing a formatted string. `jq -e` exits non-zero when the
value is missing, so a broken read cannot pass as an under-budget day:

```bash
budget_usd=25
spent=$(agentsview report usage statusline --json --offline --no-sync \
  | jq -e '.cost.microdollars') || exit 1
if [ "$spent" -gt $((budget_usd * 1000000)) ]; then
  printf 'AgentsView: $%d.%02d today (over $%d)\n' \
    $((spent / 1000000)) $((spent % 1000000 / 10000)) "$budget_usd" >&2
  exit 1
fi
```

## Where the data lives

Usage reports read from the same local SQLite database that powers the
[web UI](/docs/usage/) and [Analytics dashboard](/docs/usage/#dashboard). Each
message row in the `messages` table stores its token usage, and a small
`model_pricing` table caches pricing and refreshes on each `usage` invocation.

Your usage data stays on your machine unless you turn on a sharing feature. The
only outbound requests the usage commands make are the LiteLLM and OpenRouter
pricing fetches, which you can disable with `--offline`. See
[Privacy and Telemetry](/docs/configuration/#privacy-and-telemetry) for the
full picture.
