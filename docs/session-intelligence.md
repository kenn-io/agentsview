---
title: Session Intelligence
description: Health scores, outcomes, and session-quality analytics in AgentsView
---

Use session health scores to spot stalled sessions, repeated tool failures, and
patterns across projects. AgentsView reads the recorded transcript to estimate
each session's outcome and show the evidence behind its score.

!!! note

    These signals are heuristics, not ground truth. Use them for triage and
    pattern-finding rather than as a replacement for your own judgment about
    whether a session succeeded.

## Where it appears

You can view session intelligence in four places:

- **Session detail UI.** The session header displays a health grade badge.
  Click it to open the in-session signal panel.
- **Analytics dashboard.** The dashboard includes a **Session health** section
  with score, outcome, tool-failure, compaction, trend, agent, and project
  breakdowns.
- **Session CLI and API.** `agentsview session get` and
  `agentsview session list` expose health and outcome fields, along with
  filters such as `--health-grade`, `--outcome`, and `--min-tool-failures`.
- **CLI health view.** `agentsview session health` prints either a recent
  session list with grade and outcome columns, or detailed signals for a single
  session.

To check elapsed turn time and measured tool execution, open **Analysis** in a
session. Its [Session Vitals view](/docs/usage/#session-vital-signs) separates
measured tool time from unattributed time and links activity rows to the
transcript. Timing and health scores answer different questions: where time
went, and which patterns deserve attention.

For a model assessment of which calls helped, open **Session actions > Analyze
tool effectiveness**. Its saved report cites the calls behind each conclusion.
See [Tool effectiveness reports](/docs/recall/#tool-effectiveness-reports) for
how to generate and read it, including what AgentsView sends to the model.

## Health score

The health score is a penalty-based score out of 100. AgentsView starts at 100
and subtracts penalties for outcome, tool health, and context pressure, and it
can also subtract small penalties for prompt quality, context quality, and
workflow quality heuristics when those signals are present:

- **Outcome**
- **Tool health**
- **Context pressure**
- **Prompt quality**
- **Context quality**
- **Workflow quality**

When a final score exists, AgentsView maps it to a letter grade:

| Grade | Score |
|------|------:|
| `A` | `90-100` |
| `B` | `75-89` |
| `C` | `60-74` |
| `D` | `40-59` |
| `F` | `0-39` |

Some sessions remain **unscored**. That happens when the outcome is `unknown`
with low confidence and no other scoring category has data, including tool
health, context, or the prompt, context, and workflow quality heuristics.

### Penalty model

The 0.23.0 score model uses these penalties:

| Signal | Penalty |
|------|------:|
| `errored` outcome | `30` |
| `abandoned` outcome | `15` |
| tool failure signal | `3` each, capped at `30` |
| tool retry | `5` each, capped at `25` |
| edit churn | `4` each, capped at `20` |
| consecutive failure streak of 3+ | `10` |
| extra compactions after the first | `5` each, capped at `15` |
| mid-task compaction | `8` each, capped at `18` |
| context pressure above `0.9` | `10` |

The in-session signal panel shows the exact basis and penalties used for a
scored session.

## Outcome classification

AgentsView classifies every session into one of four outcomes:

- `completed`
- `abandoned`
- `errored`
- `unknown`

Each classification also carries a confidence level:

- `high`
- `medium`
- `low`

The 0.23.0 classifier follows these rules:

- Automated sessions default to `unknown`.
- Very short sessions stay `unknown`.
- Recently active sessions stay `unknown` until they have clearly settled.
- Sessions that end on a user turn skew toward `abandoned`.
- Sessions with a final failure streak of 3 or more skew toward `errored`.
- Sessions that end on an assistant turn skew toward `completed`.

The analytics dashboard aggregates these classifications into outcome
distributions and completion rates by agent and by project.

## Tool-health signals

AgentsView derives tool-health signals from tool-call history, tool results,
and tool-result status events:

- **Failure signals.** Counts calls that look failed, either from explicit
  `errored`, `cancelled`, `error`, or `denied` status events or from content
  heuristics such as shell errors and `FAILED` write/edit results.
- **Retries.** Counts repeated identical tool calls when the same tool name and
  identical input run 3 or more times in a row. Calls without a stored input,
  such as every call in a `transcripts` archive, never count as retries.
- **Edit churn.** Counts files edited or written 3 or more times within a tight
  ordinal window, which usually signals rework.
- **Consecutive failure max.** Records the longest run of failed tool calls in
  a session.

These signals feed both the per-session score and the dashboard's tool-health
summary metrics.

## Tool sequences

Open the health badge in the session header to view **Observed tool
sequences**. Each sequence begins with an error or empty tool result and traces
the later calls, including repeated inputs, switches to another tool, and
recovery. Each step links to its call in the transcript.

![Observed tool sequences with transcript evidence](/docs/assets/generated/screenshots/tool-sequences.png)

Expand a step to inspect its recorded input, retained result text, and measured
duration when available. Missing timing stays labeled **Not measured**. The
panel derives sequences from recorded calls and results. Opening it runs no
model and adds no token charges.

Sequence endings describe the recorded trace. **Recovered** means a later call
returned content, **Abandoned** means the session ended after an error or empty
result, and **Open** or **Unknown** means the trace does not establish a
resolved ending. A recovered tool sequence does not prove the overall task
succeeded.

On ClickHouse, run `agentsview replica clickhouse push` with the new version
before serving the mirror. The first push refreshes every session once, and
tool sequences stay unavailable for sessions that push has not reached.

## Context signals

AgentsView also tracks signals related to context pressure:

- **Compaction count.** How often the session crossed a compact boundary.
- **Mid-task compactions.** Compactions that interrupted active work instead of
  happening at a clean boundary.
- **Context pressure max.** The highest context-pressure reading observed for
  the session when the parser can infer it.

Mid-task compactions carry a heavier weight than ordinary compactions because
they are a stronger sign that the agent lost working context and had to
recover.

## UI views

### Session header and signal panel

When a session has intelligence data, the session header shows a grade badge:

![Grade badge in the session header](/docs/assets/generated/screenshots/grade-badge.png)

Click the badge to open the signal panel, which displays:

- grade and numeric score
- outcome icon and confidence
- basis tags showing which categories contributed
- compaction chip with a mid-task indicator when applicable
- penalty chips for the exact deductions applied

![Signal panel with basis tags, compaction chip, and penalty chips](/docs/assets/generated/screenshots/signal-panel.png)

If a session does not have enough usable data, the panel shows a short
empty-state message instead of a score.

### Dashboard health section

The analytics dashboard's **Session health** section aggregates the same
per-session signals into:

- average health score
- outcome distribution
- tool-failure rate
- compaction summaries
- score trend over time
- by-agent and by-project tables

![Session health section on the analytics dashboard](/docs/assets/generated/screenshots/session-health.png)

This section appears only when the current filter window contains scored or
unscored signal-bearing sessions.

## CLI and API commands

<a id="agentsview-health"></a>

### `agentsview session health`

`agentsview session health` gives you a human-first CLI for quick inspection:

```bash
agentsview session health
agentsview session health --limit 50
agentsview session health <session-id>
agentsview session health <session-id> --json
```

Without a session ID, it lists recent sessions with grade and outcome columns.
With a session ID, it prints the detailed signal counts for that session,
including tool failures, retries, edit churn, failure streaks, compactions, and
context pressure.

### `agentsview session`

See the [Session API](/docs/session-api/) docs for the full set of programmatic
commands. For session intelligence, the most useful commands are:

```bash
agentsview session get <id> --format json
agentsview session list --health-grade A,B --outcome completed
agentsview session list --min-tool-failures 0
```

`session get` returns detail-level fields such as `health_score_basis` and
`health_penalties`. `session list` provides health and outcome filters for
automation-friendly scans.

<a id="relationship-to-agentsview-stats"></a>

## Relationship to `agentsview report stats`

Session intelligence works **per session** and powers the session UI, the
session API, and the dashboard's **Session health** section.

[`agentsview report stats`](/docs/stats/) takes a different approach: it is an
**aggregate reporting** command that summarizes windows of session activity,
git activity, tool mix, model mix, and outcome metrics across the whole
workspace.
