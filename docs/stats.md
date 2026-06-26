---
title: Stats
description: Experimental workspace analytics via agentsview stats
---

`agentsview stats` is a top-level CLI command added in 0.23.0 for
window-scoped reporting over your local session archive. It emits a
human-readable summary by default and can also emit JSON for scripts
and downstream tooling.

!!! warning "Experimental"
    `agentsview stats` is experimental. The human-readable output is not
    stable, and the exact JSON output may change in future releases.
    Treat it as a moving surface and parse it defensively.

## What It Reports

The command pulls together several categories of information:

- **Session totals** — total sessions, human versus automation
  sessions, total messages, total user messages
- **Session archetypes** — automation, quick, standard, deep, and
  marathon buckets
- **Session shape** — mean duration, user-message counts, peak
  context, and tools-per-turn
- **Velocity** — turn-cycle timing, first-response timing, and
  messages per active hour
- **Tool, model, and agent mix** — top tool categories plus token
  and session mix by model and agent
- **Claude-only optional sections** — cache economics, plan-mode
  adoption, subagent activity, and skill counts when the window has
  compatible data
- **Temporal activity** — active UTC-hour buckets plus the reporter
  timezone
- **Git outcomes** — commit, LOC, file-change, and optional PR
  totals for repos enclosing session working directories
- **Session outcomes** — aggregate counts, grade distribution,
  retry rate, compactions per session, and edit churn. The raw
  [four outcomes](/session-intelligence/#outcome-classification)
  (`completed`, `abandoned`, `errored`, `unknown`) are rolled up
  here into three buckets: `success` (= `completed`), `failure`
  (= `abandoned` or `errored`), and `unknown` (= `unknown` plus
  any unrecognized value). The rollup applies to both the human
  summary and the JSON `outcomes` block.

## Automation Scope

As of 0.25.0, `agentsview stats` uses each session's stored
`is_automated` value as the authority for human versus automation
scope. That classification drives session totals, archetypes, the
`scope_human` distributions, and the human-scoped
`agent_portfolio` fields (`by_sessions_human`, `by_messages_human`,
`by_tokens_human`, and `primary_human`). `scope_all` still includes
every session in the selected window.

For the `user_messages.scope_human` distribution only, sessions with
fewer than two user messages are omitted from that distribution's
mean and buckets because the human bucket set starts at two user
messages. They still contribute to `scope_all` and to the session
totals according to their `is_automated` value.

## Usage

```bash
# Human-readable summary for the last 28 days
agentsview stats

# Fixed date range as JSON
agentsview stats --format json --since 2026-04-01 --until 2026-04-15

# Narrow to one agent and one project
agentsview stats --agent claude --include-project my-app
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--format` | `human` | Output format: `human` or `json` |
| `--since` | `28d` | Start of window, either a compact duration like `28d` or a `YYYY-MM-DD` date |
| `--until` | now | End of window as `YYYY-MM-DD` |
| `--agent` | `all` | Restrict to one agent, or leave as `all` |
| `--include-project` | | Repeatable project allowlist |
| `--exclude-project` | | Repeatable project blocklist |
| `--timezone` | local | Timezone used for temporal reporting |

## Local-Only Behavior

Unlike `agentsview session`, this command currently reads the local
SQLite archive directly. It does **not** proxy through a running
AgentsView daemon.

That means:

- a running `agentsview serve` process is not required
- the command reflects whatever is already present in the local
  archive
- if you want the newest session data first, run
  `agentsview sync` or `agentsview serve` before using it

## Human Output

The default human output is organized into named sections. Depending
on the data available in the selected window, you may see:

- `Totals`
- `Archetypes`
- `Session shape`
- `Velocity`
- `Tool mix`
- `Model mix`
- `Agent portfolio`
- `Cache economics`
- `Adoption`
- `Temporal`
- `Outcome stats`
- `Outcomes`

Some sections are optional. For example, git-based outcome stats are
omitted when AgentsView cannot derive any repos from the sessions in
the selected window, and Claude-only sections are omitted when the
window has no compatible data.

## JSON Output

JSON output currently carries `schema_version: 1` and is divided into
top-level blocks such as:

- `window`
- `filters`
- `totals`
- `distributions`
- `archetypes`
- `velocity`
- `tool_mix`
- `model_mix`
- `agent_portfolio`
- `temporal`
- `generated_at`

Optional blocks may also appear:

- `cache_economics`
- `adoption`
- `outcome_stats`
- `outcomes`

Even though the JSON currently includes a version number, it is still
best to treat it as experimental and additive: expect new fields,
optional blocks, and formatting changes over time.

## Git And PR Aggregation

`agentsview stats` uses session working directories to discover git
repositories and then aggregates git activity for the configured
author in each repo.

That includes:

- commit counts
- lines added and removed
- files changed

Pull-request counts are optional and use CLI-oriented GitHub token
sources:

- If `AGENTSVIEW_GITHUB_TOKEN` is set, AgentsView uses it.
- Otherwise it tries `gh auth token`.
- If neither source yields a token, PR counts are omitted instead of
  being reported as zero.

This distinction matters in JSON output: a missing PR field means
"GitHub lookup not configured", not "configured and zero PRs found".

## Relationship To Session Intelligence

[`Session Intelligence`](/session-intelligence/) is the per-session
surface for health scores, outcomes, and signal inspection.

`agentsview stats` is the aggregate surface: it summarizes the whole
archive over a window rather than explaining one session at a time.
