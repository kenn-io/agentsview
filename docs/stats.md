---
title: Stats
description: Experimental workspace analytics with agentsview report stats
---

Run `agentsview report stats` to summarize a selected window of your local
session archive. By default it prints a human-readable summary, and it can also
emit JSON for scripts and downstream tooling. The reporting feature originally
shipped as `agentsview stats` in 0.23.0.

!!! warning "Experimental"

    `agentsview report stats` is experimental. The human-readable output is not
    stable, and the exact JSON output may change in future releases. Parse the
    output defensively.

## What it reports

The command gathers several categories of data:

- **Session totals.** Total sessions, human versus automation sessions, total
  messages, and total user messages.
- **Session archetypes.** Automation, quick, standard, deep, and marathon
  buckets.
- **Session shape.** Mean duration, user-message counts, peak context, and
  tools per turn.
- **Velocity.** Turn-cycle timing, first-response timing, and messages per
  active hour.
- **Tool, model, and agent mix.** Top tool categories, plus token and session
  mix by model and agent.
- **Claude-only optional sections.** Cache economics, plan-mode adoption,
  subagent activity, and skill counts when the window has compatible data.
- **Temporal activity.** Active UTC-hour buckets plus the reporter timezone.
- **Git outcomes.** Optional, opt-in commit, LOC, file-change, and PR totals
  for repositories enclosing session working directories.
- **Session outcomes.** Aggregate counts, grade distribution, retry rate,
  compactions per session, and edit churn. Here AgentsView rolls up the raw
  [four outcomes](/docs/session-intelligence/#outcome-classification)
  (`completed`, `abandoned`, `errored`, `unknown`) into three buckets:
  `success` for `completed`, `failure` for `abandoned` or `errored`, and
  `unknown` for `unknown` plus any unrecognized value. That rollup applies to
  both the human summary and the JSON `outcomes` block.
- **Code attribution.** Optional AI-authored code attribution from host-local
  sources such as Cursor.

## Automation scope

By default, `agentsview report stats` excludes one-shot sessions (one user
message or fewer) and automated sessions. Pass `--include-one-shot` to add
one-shot sessions back, and pass `--include-automated` to add automated
sessions back.

Since 0.25.0, stats reporting uses each session's stored `is_automated` value
as the authority for human versus automation scope. That classification drives
session totals, archetypes, the `scope_human` distributions, and the
human-scoped `agent_portfolio` fields (`by_sessions_human`,
`by_messages_human`, `by_tokens_human`, and `primary_human`). `scope_all`
includes every session the command includes in the selected window.

For the `user_messages.scope_human` distribution only, AgentsView omits
sessions with fewer than two user messages from that distribution's mean and
buckets because the human bucket set starts at two user messages. When those
sessions are included, which by default requires `--include-one-shot`, they
still contribute to `scope_all` and to the session totals according to their
`is_automated` value.

## Usage

```bash
# Human-readable summary for the last 28 days
agentsview report stats

# Fixed date range as JSON
agentsview report stats --format json --since 2026-04-01 --until 2026-04-15

# Narrow to one agent and one project
agentsview report stats --agent claude --include-project my-app
```

## Flags

| Flag                        | Default | Description                                                                                                       |
| --------------------------- | ------- | ----------------------------------------------------------------------------------------------------------------- |
| `--format`                  | `human` | Output format: `human` or `json`                                                                                  |
| `--json`                    | `false` | Alias for `--format json`                                                                                         |
| `--since`                   | `28d`   | Start of window: a duration such as `28d` or `24h` (`Nd` or `Nh`), a `YYYY-MM-DD` date, or an RFC3339 timestamp   |
| `--until`                   | now     | End of window: a duration such as `28d` or `24h` (`Nd` or `Nh`), a `YYYY-MM-DD` date, or an RFC3339 timestamp     |
| `--agent`                   | `all`   | Restrict to one agent, or leave as `all`                                                                          |
| `--include-project`         |         | Repeatable project allowlist                                                                                      |
| `--exclude-project`         |         | Repeatable project blocklist                                                                                      |
| `--include-one-shot`        | `false` | Include one-shot sessions, which are excluded by default                                                          |
| `--include-automated`       | `false` | Include automated sessions, which are excluded by default                                                         |
| `--include-git-outcomes`    | `false` | Include git-derived outcome stats (commits, lines, files)                                                         |
| `--include-github-outcomes` | `false` | Include GitHub pull-request counts; implies `--include-git-outcomes`                                              |
| `--timezone`                | local   | Timezone used for temporal reporting                                                                              |

## Data scope

Session-derived stats come from the daemon API. The command connects to the
local daemon or starts one, and the daemon reads the selected window from its
archive. If a read-only daemon cannot provide stats, the command reports that
error instead of switching to a separate local SQLite archive.

Code attribution is not synced session data. When present, the host answering
the stats request reads the Cursor source in `code_attribution.sources` live from its local Cursor attribution database:
`~/.cursor/ai-tracking/ai-code-tracking.db` by default, or the path in
`AGENTSVIEW_CURSOR_ATTRIBUTION_DB`.

That makes the Cursor source machine-local. AgentsView does not aggregate it
across synced machines or push it to PostgreSQL, and PostgreSQL read-only
serving cannot return it. Because project filters cannot be applied to Cursor's
attribution database, project-filtered stats include a Cursor source with
`status: "unsupported_filter"` instead of silently reporting zero attribution.

## Human output

The default human output uses named sections. Depending on the flags you pass
and the data in the selected window, you may see:

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
- `Code attribution`

Some sections are optional. `Outcome stats` appears only when you pass
`--include-git-outcomes` or `--include-github-outcomes`, and AgentsView also
omits it when it cannot derive any repositories from the sessions in the
selected window. Claude-only sections disappear when the window has no
compatible data.

When a repository lookup fails, the outcome section lists its path, operation,
and reason under `Incomplete:`. Multiline reasons keep their continuation lines
indented. A `pr` entry means only that repository's pull-request counts are
missing, while its commit, line, file, and active-repository counts still
contribute.

## JSON output

JSON output currently carries `schema_version: 2` and uses top-level blocks
such as:

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

Optional blocks can also appear:

- `cache_economics`
- `adoption`
- `outcome_stats`
- `outcomes`
- `code_attribution`

`outcome_stats.skipped` lists repository operations that could not contribute
to the totals, and AgentsView omits it when no lookups were skipped. Each entry
contains:

- `repo`: the local repository's root path
- `op`: the operation that could not contribute, as listed below
- `reason`: the full diagnostic text, which may contain multiple lines

| `op`     | Meaning                           | Counts missing for this repository          |
| -------- | --------------------------------- | ------------------------------------------- |
| `author` | No author email could be resolved | All git and PR counts                       |
| `log`    | The commit lookup failed          | All git and PR counts                       |
| `pr`     | The pull-request lookup failed    | Only PR counts; git counts still contribute |

Other repositories still contribute their available counts. If none can
contribute, `outcome_stats` stays present with zero git counters and the
`skipped` entries explaining why. Check `skipped` before treating zeros as no
activity. The block's presence alone does not mean data was available.
AgentsView omits `outcome_stats` when you did not request git outcomes or when
no repositories can be discovered.

`code_attribution.sources` lists attribution sources that contributed to the
selected stats request. Each source carries `provider`, `scope`, `status`,
optional `warnings`, and provider-specific `metrics`. The Cursor source
currently uses `scope: "machine_local"`.

Current source statuses are `available`, `empty`, `unavailable`, `error`, and
`unsupported_filter`. Check a source's `warnings` before reading missing
metrics or zero counters as zero AI code activity. If an agent filter excludes
Cursor, AgentsView omits the Cursor source. If no source contributes, it omits
the entire `code_attribution` block.

Even though the JSON includes a version number, treat it as experimental and
additive. Expect new fields, optional blocks, and formatting changes over time.

## Git and PR aggregation

Git outcomes are opt-in. Pass `--include-git-outcomes` to include commit, line,
file-change, and active-repository counts in the `Outcome stats` section and
the JSON `outcome_stats` block. With that flag enabled,
`agentsview report stats` uses session working directories to discover git
repositories and aggregates activity for the configured author in each one:

- commit counts
- lines added and removed
- files changed

To include pull-request counts, pass `--include-github-outcomes`, which also
turns on `--include-git-outcomes`. That flag triggers a GitHub token lookup
using CLI-oriented sources:

- If `AGENTSVIEW_GITHUB_TOKEN` is set, AgentsView uses it.
- Otherwise, it tries `gh auth token`.
- If neither source yields a token, AgentsView omits PR counts instead of
  reporting them as zero.

That distinction matters in JSON output. A missing PR field means no repository
returned PR counts, either because PR lookups were not requested or configured
or because repositories could not contribute. Check `outcome_stats.skipped` for
failures. A present PR field containing zero means the successful lookups found
no matching PRs, and you can still check `skipped` to see whether any
repositories are missing from that total.

## Relationship to Session Intelligence

[`Session Intelligence`](/docs/session-intelligence/) works one session at a
time, giving you health scores, outcomes, and signal inspection.

`agentsview report stats` takes an aggregate view instead, summarizing the
whole archive over a window rather than explaining individual sessions.
