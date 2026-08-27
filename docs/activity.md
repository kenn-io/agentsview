---
title: Activity
description: Activity, concurrency, and session-time reporting in AgentsView
---

The **Activity** page is a top-level view for understanding when agents were
actually active, how much work overlapped, and which projects, models, agents,
machines, and sessions contributed to a time window. Open it from the
**Activity** button in the header or directly at `/activity`.

![Default daily Activity view](/assets/generated/screenshots/activity-page.png)

The report is built from timestamped session activity and usage rows. It
includes one-shot and automated sessions by default, then lets you narrow the
result with the page controls.

## Range And Filters

The toolbar uses the shared range picker with calendar **Day**, **Week**,
**Month**, and **Custom** selections. Activity opens to the current day. It only
adopts and publishes shared ranges when **Settings > Date ranges > Link date
ranges across pages** is enabled; a shared range wider than Activity can
represent is not adopted.

![Weekly Activity view](/assets/generated/screenshots/activity-week.png)

Additional filters scope the report by:

- **Project** — typeahead project filter
- **Agent** — dropdown of all agents present in the activity data
- **Machine** — dropdown of synced machine names
- **Automation** — **All Sessions**, **Interactive**, or **Automated**

Filter and range state is written to the URL with query parameters such as
`preset`, `date`, `from`, `to`, `window_days`, `project`, `agent`, `machine`,
and `automation`.

## Summary Cards

The summary cards show:

- **Peak Concurrency** — the maximum number of agents active in the same bucket,
  with the local clock time of the peak
- **Active** — active wall-clock time, plus idle time in the range
- **Agent-minutes** — combined active minutes across concurrent agents
- **Sessions** — session count, with interactive/automated and untimed-session
  detail when applicable
- **Projects** and **Models** — distinct counts in the range
- **Total Cost** — selected session cost attributed to activity in the range:
  authoritative reported totals when available, otherwise catalog estimates

The report counts subagent sessions (for example Claude Code Task-tool agents)
and fork sessions (rewound conversation branches) alongside their parent
sessions, so **Total Cost** lines up with `agentsview usage daily` for the same
day and timezone. Usage rows that recur across related sessions are deduplicated
before totaling, the same rule the Usage page applies.

If the selected range reaches into the future, the page marks it as partial and
shows the report's current **as of** time.

## Concurrency

The **Concurrency** chart shows active agents over the selected range. Blue
segments represent interactive sessions, orange segments represent automated
sessions, and the strip below the chart marks active versus idle buckets.

![Weekly Activity concurrency chart](/assets/generated/screenshots/activity-concurrency.png)

Hover a bucket to see its time range, peak agent count, agent-minutes, output
tokens, and cost. The **Overlay** control can draw an additional **Tokens** or
**Cost** trend over the concurrency bars.

Clicking a bucket filters the Sessions table to the sessions active in that time
slot. Click the same bucket again, or dismiss the **Active:** badge in the table
header, to clear the slot filter. Membership is computed by the same shared
aggregator that builds the chart, then fetched as a bounded page; the browser no
longer downloads every raw activity interval to perform this drill-down.

## Sessions

The **Sessions** table lists every session that contributed to the report. Rows
include the session title, model, project, agent, agent-minutes, cost, and
active window.

![Weekly Activity sessions table](/assets/generated/screenshots/activity-sessions.png)

Click a session title to open that session in the transcript viewer. Column
headers for **Project**, **Agent**, **Agent-min**, **Cost**, and **Window** are
sortable; timing-only sorts keep untimed sessions at the bottom.

The table initially includes at most 200 rows. Sorting, bucket filtering, and
later pages run on the server, with a maximum page size of 500 rows. A loading
indicator remains local to the table, so the report summary and chart stay
visible while a page is fetched.

Automated sessions are marked with an **Auto** badge. Untimed sessions can still
carry cost if usage rows exist but timestamped activity was unavailable.

## Breakdowns

The **Breakdown** panel ranks activity by **Project**, **Model**, and **Agent**.
Toggle between **Agent-min** and **Cost** to change the metric, and use the
stacked bars to compare interactive and automated contributions.

![Weekly Activity breakdowns](/assets/generated/screenshots/activity-breakdowns.png)

Rows with no value for the selected metric are omitted from that view, so
cost-only untimed sessions appear in **Cost** but not **Agent-min**.

Project, agent, session, bucket, and report totals use the authoritative session
total when one is available. For a multi-model session, AgentsView allocates
that total across usage rows in proportion to their catalog-price estimates. The
per-model costs are therefore estimated attributions, not provider-reported
model charges, but they still sum to the displayed total.

## Create A Project Mapping

Worktree layouts the parser does not recognize can surface a branch or worktree
directory name as a project. Each row in the **Project** breakdown links to that
project on the [Data page](/data/), where the mapping editor lists the project's
observed session folders, previews the full-archive impact of a folder-path →
project rule, and applies a
[worktree project mapping](/configuration/#worktree-project-mappings) rule in
one atomic step. Cleaning always evaluates the complete archive; the current
Activity range and filters do not carry over.

## Activity Insight

At the bottom of the page, **Activity Insight** shows an existing global
`daily_activity` insight for the exact resolved date range when one exists. If
the server is writable, generate a new insight from the same panel using Claude,
Codex, Copilot, Gemini, or Kiro.

![Weekly Activity Insight panel](/assets/generated/screenshots/activity-insight.png)

The **Open in Generated insights** link opens the
[Generated insights](/recall/?tab=generated) tab prefilled with the same range.
Generation is disabled when the connected server cannot run an agent CLI.

## CLI And API

The web page uses:

```http
GET /api/v1/activity/report
```

The same report is available from the CLI:

```bash
agentsview activity report --preset day --date 2026-06-20
agentsview activity report --preset week --date 2026-06-20 --json
agentsview activity report --preset custom \
  --from 2026-06-20T14:00:00Z \
  --to 2026-06-20T18:00:00Z \
  --bucket 15m
agentsview activity report --preset month --date 2026-07-01 --json \
  --sessions-limit 200 --sessions-sort cost --sessions-direction desc
```

Large reports emit progress on stderr in both daemon and direct-database modes;
JSON stdout remains one machine-readable document. Use `--sessions-cursor` with
the returned `sessions_next_cursor` to request a later page. In daemon mode,
also pass the paired `report_id` as `--sessions-report-id`; this preserves the
original generation when a current or partial range advances between commands.
The same paging flags work with `--offline`; the signed direct-mode cursor
carries the original resolved range and filters, then deterministically
recomputes that generation before selecting the next page.

See [CLI Reference](/commands/#agentsview-activity-report) and
[Session API](/session-api/#activity-report) for flags and response shape.

### JSON Contract

`agentsview activity report --json` and `/api/v1/activity/report` share one
versioned JSON contract. Schema version 6 contains a bounded first session page,
`sessions_total`, `sessions_next_cursor`, and a signed self-describing
`report_id`; it no longer contains the message-sized `intervals` array. The CLI
and HTTP report use the same `schema_version` and move in lockstep; if the CLI
report changes in a way that requires a schema bump, the HTTP report bumps with
it.

Clients may request progress from the report route with
`Accept: text/event-stream`. The stream sends `progress` events for loading
sessions, loading usage, scanning activity, finalizing, and completion, followed
by one `report` event. A client that does not request SSE receives ordinary JSON
from the same URL. Both forms use the long-running route path and therefore are
not subject to the normal 30-second API operation timeout; request cancellation
still stops database scans.

Session pages are available at:

```http
GET /api/v1/activity/report/{report_id}/sessions
```

The endpoint accepts `limit`, `cursor`, `sort`, `direction`, and the optional
zero-based half-open range `bucket_start` and `bucket_end`. Both range bounds
must be present. Ordinary page responses contain only the bounded session page.
Stateless clients that also need report metadata can request
`include_report=true`; refresh-required responses always include the complete
replacement report. Ordering always ends with session ID ascending, so a signed
position cursor remains deterministic after cache eviction or daemon restart.
The report token carries the resolved query and a coarse archive probe. Cached
artifacts expire after 15 idle minutes and are bounded by entry, row, and byte
limits; a cache miss recomputes from the token. If the archive generation has
changed, the response sets `refresh_required` and returns a complete replacement
report and first page together, rather than mixing a new table with an old
summary. Sync notifications continue to mark the dashboard stale and do not
automatically reaggregate it.

Each project, branch, agent, or machine filter is limited to 1,024 UTF-8 bytes,
with a 3,072-byte combined limit. The server also validates the fully encoded
signed `report_id`, including JSON escaping and base64 expansion, before
aggregation.

Older CLIs do not validate `schema_version` and can decode a v6 plain-JSON
response. The embedded first page deliberately preserves the prior default
ordering—agent-minutes descending, untimed sessions last, session ID ascending
as the final tie-break—so their five-row human summary remains compatible. They
cannot request later pages or consume progress.

The activity report JSON, `agentsview usage daily --json`, and
`agentsview export sessions --format json|ndjson` are separate versioned
surfaces. Usage and activity already emitted `schema_version: 1` before 0.38,
and the session-summary v1 contract shipped in 0.37.1. Releases 0.38.0 and
0.38.1 emitted the substantially revised project-evidence shape while still
reporting version 1. Version 2 corrected those markers, version 3 introduced
exact microdollar money objects, and version 4 adds resolved-model pricing
provenance with complete request-pricing bands and application counts. Those two
transitional releases must not be treated as v1-compatible. Consumers should
require the expected `schema_version` and ignore unknown additive fields. The
commands do not provide an earlier-version output mode.

The activity report includes the shared report-level `pricing` and `projects`
blocks. `pricing.models` is keyed by reported model names. Each entry contains
an aggregate `cost_source` and explicit `resolutions` with `priced_model` and
effective rate fields such as `input_cost_per_mtok`, `output_cost_per_mtok`,
`cache_write_cost_per_mtok`, `cache_write_1h_cost_per_mtok`, and
`cache_read_cost_per_mtok`, plus available
`bands` and report-specific `application` counts. Every project-bearing report
row contains an opaque `project_key`. `projects` is keyed by that value and
carries the presentation-only `display_label`; unknown project identity is
represented by an explicit `resolution` with `identity` omitted.

See [Token Usage & Costs](/token-usage/#json-contract) for the shared bump
rules, [Pricing Provenance](/token-usage/#pricing-provenance) for pricing digest
and `cost_source` semantics, and
[Project Identity](/token-usage/#project-identity) for key derivation and
redaction notes.
