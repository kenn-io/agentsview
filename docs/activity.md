---
title: Activity
description: Activity, concurrency, and session-time reporting in AgentsView
---

Use **Activity** to see when agents ran, how much work overlapped, and what it
cost, and break down a period by project, model, agent, machine, or session.
Open it from the **Activity** button in the header or go directly to
`/activity`.

![Default daily Activity view](/docs/assets/generated/screenshots/activity-page.png)

AgentsView builds the report from timestamped session activity and usage rows.
By default, it includes one-shot and automated sessions, and you can narrow the
results with the page controls.

On a local archive, reopening an ended range reuses its finished report while
later sessions sync. Changes to report sessions, matching Claude usage, pricing,
or project identity cause the next open to rebuild it. For a range that includes
today, reopening also reuses unchanged usage while the report follows the
current time.

## Range and filters

The toolbar uses the shared range picker with calendar **Day**, **Week**,
**Month**, and **Custom** selections. Activity opens to the current day. It
adopts and publishes shared ranges only when **Settings > Date ranges > Link
date ranges across pages** is enabled, and it does not adopt a shared range
wider than Activity can represent.

![Weekly Activity view](/docs/assets/generated/screenshots/activity-week.png)

You can also scope the report with these filters:

- **Project.** Typeahead project filter.
- **Agent.** Dropdown of all agents present in the activity data.
- **Machine.** Dropdown of synced machine names.
- **Automation.** **All Sessions**, **Interactive**, or **Automated**.

AgentsView writes your filter and range state to the URL using query parameters
such as `preset`, `date`, `from`, `to`, `window_days`, `project`, `agent`,
`machine`, and `automation`.

## Summary cards

The summary cards show:

- **Interactive peak.** The maximum number of interactive conversations active
  at the same instant, with the clock time of the peak in the report timezone.
- **Active.** Active wall-clock time, plus idle time in the range.
- **Agent-minutes.** Combined active minutes across concurrent agents.
- **Sessions.** Session count, with interactive, subagent, automated, and
  untimed-session detail when applicable.
- **Projects** and **Models.** Distinct counts in the range.
- **Total Cost.** Selected session cost attributed to activity in the range,
  using authoritative reported totals when available and catalog estimates
  otherwise.

The report counts subagent sessions (for example, Claude Code Task-tool agents)
and fork sessions (rewound conversation branches) alongside their parent
sessions, so **Total Cost** lines up with `agentsview report usage daily` for
the same day and timezone. Usage rows that recur across related sessions are
deduplicated before totaling, following the same rule the Usage page applies.

Subagents include native child sessions and eligible `claude -p`, `codex exec`,
or Grok headless runs. Claude Code `sdk-cli` workers qualify when their first
real normalized prompt records `turnOrigin=sdk`, and a human origin wins over
conflicting markers. A queued first prompt, missing or unknown origin, SDK
library entrypoints, or an explicit provider kind blocks this inference. Human
SDK conversations stay Interactive unless their prompts match an automation
pattern, while known subagents keep that category even when their prompts match
built-in or user `[automated]` patterns. Runs without a subagent relationship
count as Automated when they carry a roborev tag or a matching first prompt,
and prompt matching requires one or fewer real user messages.

Parentless workers appear as sidebar roots, and flat lists, search, and CLI
include them when children are requested. General stats and project, agent, and
branch pickers exclude Subagents, while automation and one-shot filters still
apply. Readable sources reparse once to recover classification evidence. Stored
parentless non-interactive runs gain the Subagent relationship during each
classification audit, including rows imported after initialization, even when
their sources are unreadable. Historical worker repair can fail when sources are
unreadable or deleted and archived rows, including replicas, lack worker
evidence such as Claude SDK origin needed to distinguish workers from human SDK
conversations or native Grok child-kind evidence omitted by older clients.
Ambiguous rows keep their existing relationship. Classification audits preserve
stored automation flags in usage-only archives, and new writes classify current
evidence.

The session count separates subagents from interactive and automated
conversations. Every known subagent counts as Subagents, including scripted
workers whose parent disappears, while forks remain in the interactive or
automated category. Costs, agent-minutes, and concurrency use the same three
separate categories. Automation filters still use each session's automation
flag, including subagents.

If the selected range reaches into the future, the page marks it as partial and
shows the report's current **as of** time.

## Concurrency and messages

The **Concurrency** chart is a stacked bar chart on one time axis. Each bucket
is a single bar with **Interactive** in blue, **Subagents** in violet, and
**Automated** in orange, stacked from the baseline in that order on one shared
scale. Bar height shows the bucket's combined concurrency peak, and each segment
shows that class's count at the instant of that peak, so the segments always add
up to the bar. The legend above the chart names the segments, and the label on
the right gives the combined peak for the range. Interactive concurrency counts
overlapping human-facing conversations and does not measure human attention.

To see how often you send prompts, switch the chart to **User messages**. To see
agent output, switch to **Assistant messages**. Both views count timestamped
transcript messages in the same time buckets as concurrency. Each view uses its
own scale, so high assistant volume cannot hide sparse user prompts. The label
above the chart shows the bucket size and the total for the selected range.

User messages count only interactive conversations, excluding automated runs and
subagent instructions. Assistant messages include all sessions selected by the
page filters. Both counts exclude system entries and tool results. Assistant
messages can contain tool calls or thinking; they represent transcript messages,
not completed turns. Messages without timestamps cannot appear on the chart.
Hover over a bucket in either message view to compare both counts.

![Activity showing assistant-message frequency](/docs/assets/generated/screenshots/activity-message-frequency.png)

![Weekly Activity concurrency chart](/docs/assets/generated/screenshots/activity-concurrency.png)

Hover over a bucket to see its time range, the stacked split at the combined
peak, the combined peak, each class's own peak within the bucket, agent-minutes,
uncached input and output tokens, and cost. A class's own peak can exceed its
segment when that class peaked at a different instant from the combined peak.
The **All-session overlay** control draws a combined **Tokens** or **Cost**
trend over the bars, with its own scale on the right. These usage totals include
all three classes.

Click a bucket to filter the Sessions table to sessions with activity or counted
messages in that time slot; this time filter stays active when you switch chart
metrics. Drag across buckets to select a range. With a keyboard, select with
Enter or Space, extend with Shift+Arrow, and clear with Escape. To clear the
slot filter, click the same bucket again or dismiss the **Active:** badge in the
table header. The same shared aggregator that builds the chart computes bucket
membership, which the page then fetches as a bounded page. The browser no longer
downloads every raw activity interval to perform this drill-down.

## Sessions

The **Sessions** table lists every session that contributed to the report, with
columns for the session title, model, project, agent, agent-minutes, cost, and
active window.

![Weekly Activity sessions table](/docs/assets/generated/screenshots/activity-sessions.png)

Click a session title to open that session in the transcript viewer. You can
sort by the **Session**, **Project**, **Agent**, **Model**, **Agent-min**,
**Cost**, and **Window** headers; timing-only sorts keep untimed sessions at the
bottom.

The table initially loads at most 200 rows and fetches more as you scroll.
Sorting, bucket filtering, and later pages run on the server, with a maximum
page size of 500 rows. If a page load fails, the table offers a retry. The
loading indicator stays local to the table, so the report summary and chart
remain visible while a page loads.

Subagent sessions carry a **Subagent** badge, and other automated sessions carry
an **Auto** badge. Untimed sessions can still show a cost if usage rows exist
but timestamped activity was unavailable.

## Breakdowns

The **Breakdown** panel ranks activity by **Project**, **Model**, and **Agent**.
Toggle between **Agent-min** and **Cost** to change the metric, and use the
stacked bars to compare interactive, subagent, and automated contributions. Each
session contributes to exactly one segment.

![Weekly Activity breakdowns](/docs/assets/generated/screenshots/activity-breakdowns.png)

Rows with no value for the selected metric are omitted from that view, so
cost-only untimed sessions appear in **Cost** but not in **Agent-min**.

Project, agent, session, bucket, and report totals use the authoritative session
total when one is available. For a multi-model session, AgentsView allocates
that total across usage rows in proportion to their catalog-price estimates. The
per-model costs are therefore estimated attributions rather than
provider-reported model charges, though they still sum to the displayed total.

## Create a project mapping

If a branch or worktree directory appears as its own project, correct the
assignment with [project mapping rules](/docs/data/#rules).

When the [opt-in project workspace](/docs/data/#enable-the-project-workspace) is
enabled, each row in the **Project** breakdown links to that project's folder
suggestions and session previews. Review the impact before you save a
correction. The Activity range and filters do not carry over, and mapping rules
always apply across the complete archive.

## Activity insight

At the bottom of the page, **Activity insight** shows an existing global
`daily_activity` insight for the exact resolved date range when one exists. If
the server is writable, you can generate a new insight from the same panel using
Claude, Codex, Copilot, Gemini, or Kiro.

![Weekly Activity Insight panel](/docs/assets/generated/screenshots/activity-insight.png)

Select **Open in Generated insights** to open the
[Generated insights](/docs/recall/?tab=generated) tab prefilled with the same
range. Generation is disabled when the connected server cannot run an agent CLI.
Scripts can generate and inspect the same reports with
[`agentsview insight`](/docs/commands/#agentsview-insight).

## CLI and API

The web page uses:

```http
GET /api/v1/activity/report
```

You can run the same report from the CLI:

```bash
agentsview report activity --preset day --date 2026-06-20
agentsview report activity --preset week --date 2026-06-20 --json
agentsview report activity --preset custom \
  --from 2026-06-20T14:00:00Z \
  --to 2026-06-20T18:00:00Z \
  --bucket 15m
agentsview report activity --preset month --date 2026-07-01 --json \
  --sessions-limit 200 --sessions-sort cost --sessions-direction desc
```

Large reports write progress to stderr in both daemon and direct-database modes,
while JSON stdout stays a single machine-readable document. To request a later
page, pass `--sessions-cursor` with the returned `sessions_next_cursor`. In
daemon mode, also pass the paired `report_id` as `--sessions-report-id` to
preserve the original generation when a current or partial range advances
between commands. The same paging flags work with `--offline`; the signed
direct-mode cursor carries the original resolved range and filters, then
deterministically recomputes that generation before selecting the next page.

For flags and response shape, see
[CLI Reference](/docs/commands/#agentsview-activity-report) and
[Session API](/docs/session-api/#activity-report).

### JSON contract

`agentsview report activity --json` and `/api/v1/activity/report` emit schema
version 8. The report contains a bounded first page of sessions,
`sessions_total`, `sessions_next_cursor`, and a signed `report_id` that ties
later pages to the report. It does not include the old `intervals` array. The
CLI and HTTP API share the same schema version and change together.

Version 8 separates interactive, subagent, and automated sessions in session
counts, agent-minutes, costs, and concurrency. The three class counts sum to
`totals.sessions`, and total usage and cost accounting is unchanged. Buckets
include independent `max_interactive_agents`, `max_subagent_agents`, and
`max_automated_agents` values, while the `*_at_peak` fields describe the split
at the combined `max_agents` peak. The report includes `interactive_peak`,
`subagent_peak`, and `automated_peak`, each with its own count and timestamp.
Session rows include `is_subagent`, which takes precedence over `is_automated`
when assigning the visible class.

Clients may request progress from the report route with
`Accept: text/event-stream`. The stream sends `progress` events for loading
sessions, loading usage, scanning activity, finalizing, and completion, followed
by one `report` event. Clients that do not request SSE receive ordinary JSON
from the same URL. Both forms use the long-running route path and therefore are
not subject to the normal 30-second API operation timeout, while request
cancellation still stops database scans.

Session pages are available at:

```http
GET /api/v1/activity/report/{report_id}/sessions
```

This endpoint accepts `limit`, `cursor`, `sort`, `direction`, and the optional
zero-based half-open range `bucket_start` and `bucket_end`, where both range
bounds must be present. Ordinary page responses contain only the bounded session
page. Stateless clients that also need report metadata can pass
`include_report=true`, and refresh-required responses always include the
complete replacement report. Ordering always ends with session ID ascending, so
a signed position cursor remains deterministic after cache eviction or daemon
restart. The report token carries the resolved query and a coarse archive probe.
Cached artifacts expire after 15 idle minutes and are bounded by entry, row, and
byte limits; a cache miss recomputes from the token. If the archive generation
has changed, the response sets `refresh_required` and returns a complete
replacement report and first page together rather than mixing a new table with
an old summary. Sync notifications continue to mark the dashboard stale and do
not automatically reaggregate it.

Version 7 applies provider-specific billing identity to computed usage and
preserves reported cost rows and custom pricing overrides. Do not compare costs
from v6 and v7 as the same billing semantics.

Each project, branch, agent, or machine filter is limited to 1,024 UTF-8 bytes,
with a 3,072-byte combined limit. Before aggregation, the server also validates
the fully encoded signed `report_id`, including JSON escaping and base64
expansion.

Older CLIs do not validate `schema_version` and can decode a v7 plain-JSON
response. The embedded first page deliberately preserves the prior default
ordering, with agent-minutes descending, untimed sessions last, and session ID
ascending as the final tie-break, so their five-row human summary remains
compatible. They cannot request later pages or consume progress.

The activity report JSON, `agentsview report usage daily --json`, and
`agentsview export sessions --format json|ndjson` are separate versioned
outputs. Usage and activity already emitted `schema_version: 1` before 0.38,
and the session-summary v1 contract shipped in 0.37.1. Releases 0.38.0 and
0.38.1 emitted the substantially revised project-evidence shape while still
reporting version 1. Version 2 corrected those markers, version 3 introduced
exact microdollar money objects, and version 4 added resolved-model pricing
provenance with complete request-pricing bands and application counts. Currently,
usage daily JSON is version 6, session-summary export (`export sessions`) is
version 7, and the activity report is version 8. Do not treat transitional
releases 0.38.0 and 0.38.1 as v1-compatible. Consumers should require the
expected `schema_version` and ignore unknown additive fields. The commands do
not provide an earlier-version output mode.

The activity report includes the shared report-level `pricing` and `projects`
blocks. `pricing.models` is keyed by reported model names, and each entry
contains an aggregate `cost_source` and explicit `resolutions` with
`priced_model` and effective rate fields such as `input_cost_per_mtok`,
`output_cost_per_mtok`, `cache_write_cost_per_mtok`,
`cache_write_1h_cost_per_mtok`, and `cache_read_cost_per_mtok`, plus available
`bands` and report-specific `application` counts. Every project-bearing report
row contains an opaque `project_key`. `projects` is keyed by that value and
carries the presentation-only `display_label`, while unknown project identity is
represented by an explicit `resolution` with `identity` omitted.

See [Token Usage & Costs](/docs/token-usage/#json-contract) for the shared bump
rules, [Pricing Provenance](/docs/token-usage/#pricing-provenance) for pricing
digest and `cost_source` semantics, and
[Project Identity](/docs/token-usage/#project-identity) for key derivation and
redaction notes.
