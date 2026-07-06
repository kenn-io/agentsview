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
**Month**, and **Custom** selections. The Activity page participates in the same
synchronized date-range selection as the Analytics dashboard and Usage page, so
a fixed or rolling range chosen in one panel can carry to the others.

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
- **Total Cost** — estimated cost attributed to activity in the range

The report counts subagent sessions (for example Claude Code Task-tool agents)
and fork sessions (rewound conversation branches) alongside their parent
sessions, so **Total Cost** lines up with `agentsview usage daily` for the same
day and timezone. Usage rows that recur across related sessions are
deduplicated before totaling, the same rule the Usage page applies.

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
header, to clear the slot filter.

## Sessions

The **Sessions** table lists every session that contributed to the report. Rows
include the session title, model, project, agent, agent-minutes, cost, and
active window.

![Weekly Activity sessions table](/assets/generated/screenshots/activity-sessions.png)

Click a session title to open that session in the transcript viewer. Column
headers for **Project**, **Agent**, **Agent-min**, **Cost**, and **Window** are
sortable; timing-only sorts keep untimed sessions at the bottom.

Automated sessions are marked with an **Auto** badge. Untimed sessions can still
carry cost if usage rows exist but timestamped activity was unavailable.

## Breakdowns

The **Breakdown** panel ranks activity by **Project**, **Model**, and **Agent**.
Toggle between **Agent-min** and **Cost** to change the metric, and use the
stacked bars to compare interactive and automated contributions.

![Weekly Activity breakdowns](/assets/generated/screenshots/activity-breakdowns.png)

Rows with no value for the selected metric are omitted from that view, so
cost-only untimed sessions appear in **Cost** but not **Agent-min**.

## Activity Insight

At the bottom of the page, **Activity Insight** shows an existing global
`daily_activity` insight for the exact resolved date range when one exists. If
the server is writable, generate a new insight from the same panel using Claude,
Codex, Copilot, Gemini, or Kiro.

![Weekly Activity Insight panel](/assets/generated/screenshots/activity-insight.png)

The **Open in Insights page** link opens the standalone
[Session Insights](/insights/) page prefilled with the same range. Insight
generation is disabled in read-only remote modes such as PostgreSQL-backed
`pg serve`.

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
```

See [CLI Reference](/commands/#agentsview-activity-report) and
[Session API](/session-api/#activity-report) for flags and response shape.

### JSON Contract

`agentsview activity report --json` and `/api/v1/activity/report` share one
versioned JSON contract. They use the same `schema_version` and move in
lockstep; if the CLI report changes in a way that requires a schema bump, the
HTTP report bumps with it.

Absent `schema_version` means legacy pre-v1 output. The activity report JSON,
`agentsview usage daily --json`, and
`agentsview export sessions --format json|ndjson` are separate versioned
surfaces.

This v1 JSON report format is intended to remain backward compatible, but it is
still relatively new and may see some instability as downstream usage settles.
Consumers should pin `schema_version`, ignore unknown additive fields, and treat
missing `schema_version` as legacy output.

The activity report includes the shared report-level `pricing` and `projects`
blocks. `pricing.models` contains effective model rates using fields such as
`input_cost_per_mtok`, `output_cost_per_mtok`, `cache_write_cost_per_mtok`, and
`cache_read_cost_per_mtok`. `projects` is keyed by the exact project labels used
in report rows, and unknown project identity is represented as `identity: null`.

See [Token Usage & Costs](/token-usage/#json-contract) for the shared bump
rules, [Pricing Provenance](/token-usage/#pricing-provenance) for pricing digest
and `cost_source` semantics, and
[Project Identity](/token-usage/#project-identity) for key derivation and
redaction notes.
