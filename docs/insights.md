---
title: Session Insights
description: AI-powered analysis of your agent coding sessions
---

AgentsView can generate AI-powered summaries and analysis of your
coding sessions using Claude, Codex, Copilot, or Gemini. Insights
run locally — your session data is sent to the AI agent running on
your machine, and the generated markdown is stored in your local
database.

Navigate to the Insights page by clicking **More → Insights**
in the header navigation bar. Insights, Pinned, and Trash all
live under the **More** dropdown as of 0.21.0, which leaves
**Sessions** and **Usage** as the top-level nav buttons.

![Insights page](/assets/generated/screenshots/insights.png)

## Proactive Issue Review

The top of the Insights page continuously ranks recurring problems and
automation opportunities across the selected chats. It is deterministic and
server-backed; generating an AI insight is not required. The review detects:

- failed commands, edits, builds, tests, migrations, Git/GitHub operations,
  missing files or dependencies, permissions, network errors, timeouts, and
  tool crashes;
- a successful retry after a failed identical call, persistent repeated waits
  or polling, and the
  same substantial workflow repeated across chats or projects;
- slow non-wait tools, exact-normalized user requests repeated across chats,
  explicit user corrections, and assistant-reported blockers;
- allowlisted Codex response, tool-router, hook, session, and PowerShell
  snapshot failures when local telemetry is available.

When an `exec` wrapper calls only one nested tool, Issue Review attributes the
finding and duration to that nested tool. Mixed-tool wrappers remain attributed
to `exec` because the outer result cannot identify one responsible tool.

The global date, project, machine, agent, termination, automation, and
one-shot filters apply first. The panel adds exact chat, folder, category, tool,
outcome, severity, confidence, status, suggested-action, and minimum-occurrence
filters. Each finding keeps at most five redacted evidence excerpts and links
to the exact message ordinal when one exists. Results are returned in pages of
100; **Load more findings** continues through the full filtered result set.

The panel refreshes when its filters or the global scope changes, after a
debounced data-sync event, every hour while open, and on manual retry.
Background refreshes use the one-hour analysis cache so frequent sync events
cannot trigger repeated full-archive scans; **Refresh now** bypasses the cache.
If a refresh fails, the last successful result remains visible with a warning.

Durations are measured only when start and completion events can be paired.
For slow tools, occurrences count calls at least 30 seconds long, while p95 is
calculated from every measured sample for that tool. Coverage is measured
samples divided by all scoped calls for the tool. The “excess” duration is a
triage proxy above 30 seconds, not a claim that all of that time was wasted.
Wait/sleep tools and negative or malformed durations are excluded.

Large tool results retain bounded context from both the beginning and end, so a
stable compiler, test, or command error near the tail remains classifiable.

The optional Codex supplement reads `~/.codex/logs_2.sqlite` in read-only mode
only for scoped session IDs and exact tool-call IDs. This supplement is
available only with the local SQLite store; PostgreSQL and DuckDB use timing
events already mirrored into their own stores. Telemetry and chat/tool
excerpts are redacted before the API returns them. The panel shows a non-blocking
warning when local telemetry is missing or unavailable; chat and tool-result
analysis remains active.

See [Proactive Issue Review handover](issue-review-handover.md) for the detector
architecture, validation matrix, deployment gates, and follow-up roadmap.

## Insight Types

There are three generation modes, selected from the dropdown at
the top of the sidebar:

| Mode | What It Generates |
|------|-------------------|
| **Daily Activity** | A concise summary of what was accomplished on a single day |
| **Date Range Activity** | A summary covering a span of days, with presets for 7 and 30 days |
| **Agent Analysis** | A deeper analysis of patterns, effectiveness, and suggestions for improving your agent workflows. From a session page, this mode can also analyze one selected session. |

Daily Activity and Date Range Activity both produce `daily_activity`
type insights. Agent Analysis produces `agent_analysis` type
insights with more detailed recommendations.

Single-session analysis is also an `agent_analysis` insight. It is started from
the active session header and sends `session_id` to the generation API, which
builds the prompt from that session's messages, timing, token usage, and cost
instead of a date-window session list. `session_id` is only accepted for
`agent_analysis`; daily activity and canned insight modes reject it.

![Single-session insight action](/assets/generated/screenshots/session-insight-action.png)

## Generating an Insight

The sidebar panel contains all the controls for generating
insights.

### 1. Select a Mode

Choose **Daily Activity**, **Date Range Activity**, or
**Agent Analysis** from the mode dropdown.

- **Daily Activity** shows a single date picker.
- **Date Range Activity** shows start and end date pickers with
  "Last 7 days" and "Last 30 days" preset buttons.
- **Agent Analysis** shows a single date picker for the analysis
  target.

### 2. Set Filters

- **Project** — scope the insight to a specific project, or
  leave on "All Projects" for a global view.
- **Agent** — choose which AI agent generates the insight:
  Claude, Codex, Copilot, or Gemini. Defaults to Claude.

### 3. Add Context (Optional)

Click **Prompt** to expand a text area where you can provide
additional context to guide the generation. For example:

- "Focus on test coverage improvements"
- "Summarize the refactoring work"
- "What patterns should I change?"

### 4. Generate

Click the **Generate** button (or the `+` icon). The insight
streams in via the agent CLI running on your machine.

While generating, a task appears in the sidebar with a spinner
and phase indicator. You can queue multiple insights at once —
each runs as a separate task. Use **Stop all** to cancel
everything, or dismiss individual tasks.

When generation completes, the insight moves to the completed
list and is automatically selected for viewing.

If generation fails — for example, due to an API error or
timeout — the task shows an error status with the error
message directly in the sidebar. This replaces the previous
behavior where failures were silent.

## Viewing Insights

Select any completed insight from the sidebar list. The content
panel displays:

- **Type badge** — blue for daily activity, purple for agent
  analysis
- **Date or date range** — the time period covered
- **Metadata** — project scope, agent name, model used, and
  when the insight was created
- **Rendered markdown** — the full insight content with
  headings, lists, code blocks, tables, and blockquotes

![Insight content](/assets/generated/screenshots/insight-content.png)

## Managing Insights

- **Delete** — click the trash icon in the insight header to
  remove it permanently.
- **Filter by project** — changing the project dropdown filters
  the completed list to show only insights for that project
  (or global insights when set to "All Projects").
- Insights are stored in your local SQLite database and persist
  across server restarts.

## How It Works

When you click Generate:

1. AgentsView queries your session database for sessions matching
   the date range and optional project filter (up to 50 sessions).
2. It builds a markdown prompt containing session metadata: IDs,
   projects, agents, timestamps, message counts, and first
   message previews.
3. The prompt is sent to the selected agent CLI (`claude -p`,
   `codex exec`, `copilot -p`, `gemini`, or `kiro-cli chat
   --no-interactive`) running locally on your machine.
4. The response streams back via Server-Sent Events, showing
   progress in the sidebar task list.
5. The completed insight is saved to the database and displayed
   in the content panel.

The generation has a 10-minute timeout. Your API keys and
subscription credentials are handled by the agent CLIs
themselves — AgentsView does not manage or store them.

## Configuring Agent Binaries

By default, AgentsView resolves each agent's CLI through your
`PATH` — `claude`, `codex`, `copilot`, `gemini`, `kiro-cli`.
If you keep multiple builds side by side, want to pin a
known-good version, or your CLI isn't on `PATH` (a sandboxed
install, a Homebrew keg-only formula, a custom build directory),
add an
`[agent.<name>]` table to `~/.agentsview/config.toml` and point
`binary` at the executable you want used for insight generation:

```toml
[agent.claude]
binary = "/opt/assets/static/agents/claude-1.7.4/bin/claude"

[agent.gemini]
binary = "/usr/local/bin/gemini"
```

Each known agent (`claude`, `codex`, `copilot`, `gemini`,
`kiro`) has its own table; agents without a `binary` override
fall back to `PATH` resolution. The setting only affects insight
generation — it does not retarget session discovery, which
always reads the on-disk session directories listed in the
[Session Discovery](/configuration/#session-discovery) table.
