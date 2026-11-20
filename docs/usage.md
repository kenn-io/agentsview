---
title: Usage Guide
description: Complete guide to the AgentsView web interface
---

AgentsView serves a web interface for browsing, searching, and analyzing your AI
agent sessions. This guide walks you through each part of the interface.

## Dashboard

When your archive is empty, AgentsView displays **No sessions yet** and lists
the folders it scans for sessions. Click **Choose folders** to open **Settings >
Session providers**, where you can configure those sources.

Once your archive has sessions and none is selected, AgentsView shows the
analytics dashboard. It gives you an overview of your agent activity across all
projects, even when sidebar filters hide every session.

![Analytics dashboard](/docs/assets/generated/screenshots/dashboard.png)

The dashboard header gives you quick access to common controls:

- **Project filter.** Type to filter projects by name and scope the dashboard to
  one project. Each entry shows its session count. Use the arrow keys to
  navigate, press Enter to select, and press Escape to close.
- **Search bar.** Opens the command palette (`Cmd+K`).
- **Sync button.** Runs a manual sync of your session files.
- **Theme toggle.** Switches between light and dark mode.
- **More actions → Import conversations.** Opens the
  [Chat Import](/docs/chat-import/) dialog.
- **Shortcuts button (`?`).** Shows all keyboard shortcuts.

The status bar along the bottom displays your session count, message count,
project count, last sync time, and build version.

On the normal local `agentsview serve` runtime, the sidebar session lists update
automatically from a global SSE event stream, which debounces during busy syncs.
The dashboard and Activity page do not refetch their charts on every sync event.
Instead, each pairs a manual refresh button with a relative "Updated..."
timestamp, and the Analytics dashboard also refreshes periodically on its own.
Click the refresh button or change the date range when you want an immediate
update.

Usage refreshes after session data changes and on its five-minute timer, at most
once a minute while visible. It pauses while hidden and catches up when you
return.

For range-based concurrency and agent-minutes reporting, use the top-level
[Activity](/docs/activity/) page.

### Summary cards

Six cards across the top of the dashboard summarize key metrics for the selected
date range:

![Summary cards](/docs/assets/generated/screenshots/summary-cards.png)

| Card             | Description                       |
| ---------------- | --------------------------------- |
| Sessions         | Total session count               |
| Messages         | Total message count               |
| Projects         | Number of active projects         |
| Active Days      | Days with at least one session    |
| Messages/Session | Average with median and p90       |
| Concentration    | Most active project and its share |

### Git and pull-request totals

The **Outcomes** panel tracks repositories, commits, lines added and removed,
and files changed for the selected dates. It reads Git history from the
repositories tied to your matching sessions. Repositories shared across clones
or worktrees count once, and each commit counts once across their branches.

To include opened and merged pull request totals:

1. Configure GitHub in **Settings**.
2. Choose **Include pull requests**.

AgentsView then requests GitHub data for each repository, which can take
minutes. A **Not counted** list names any repository operations that failed, so
you can see when totals are partial.

![Git and pull-request totals on the dashboard](/docs/assets/generated/screenshots/dashboard-outcomes.png)

If the backend or your current filters prevent AgentsView from calculating
outcome totals, the panel tells you. To use it, clear machine, model,
termination, minimum-message, recent-activity, and time-of-day filters. You can
also get the same metrics from the CLI with
[`agentsview report stats`](/docs/stats/).

### Date range picker

Use the date range picker to select a quick preset (7 days, 30 days, 90 days, 1
year, or **All**) or enter custom start and end dates. The **All** preset
includes every session regardless of age, and every chart updates when you
change the range. The dashboard, the
[Usage](/docs/token-usage/#usage-dashboard) page, and the
[Activity](/docs/activity/) page all share this picker, so presets and behavior
stay consistent across panels.

![Date range picker](/docs/assets/generated/screenshots/date-range.png)

Preset ranges are **rolling** by default. If you leave a page open past
midnight, the window moves forward on the next refresh tick, sync event, or
manual refresh instead of staying anchored to the day the page loaded. Editing
either date input manually pins the range. On Usage and Activity, explicit date
parameters in the URL always take precedence. A bare Usage URL returns to its
rolling 30-day default, and a bare Activity URL returns to its current-day
calendar default unless an enabled shared range supplies another selection. The
`All` preset always pins to `(earliest_session, today)`.

Bare pages start with independent defaults: the Sessions dashboard opens to a
rolling 1-year range, Usage opens to a rolling 30-day range, and Activity opens
to the current day. Cross-page linking is off by default because carrying a wide
range across pages automatically can make some pages run much more expensive
queries.

To share your date selection across Sessions, Usage, Activity, and Quality, turn
on **Settings > Date ranges > Link date ranges across pages**. An explicit dated
URL always controls the page you open. With linking enabled, that selection can
then carry over to date-aware pages you open later at their bare URLs.

### Model filter

The **Model** dropdown in the dashboard toolbar scopes every panel to one or
more AI models. By default, the button reads **Model: All** and applies no
filter.

![Dashboard model filter](/docs/assets/generated/screenshots/analytics-model-filter.png)

Open the dropdown to browse a searchable list of the models found in your
sessions, then click any models you want to include. The button updates to show
your selection, such as **Model: gpt-4o** or **Model: 3 selected** when multiple
models are active. Selected models also appear as removable chips beneath the
toolbar. To clear the filter and include every model again, click **All models**
at the top of the list.

When a model filter is active, every dashboard panel reflects only the selected
models: the summary cards, activity chart, heatmap, hour-of-week grid, projects,
session shape, velocity, tools, skills, top sessions, and the Session Health
rollup.

Unlike the session-grain project and agent filters, model filtering works at
**message grain** because a single session can switch models across turns. A
session is included if it has at least one message from a selected model, and
most panels count only the matching messages. Even though user messages carry no
model of their own, AgentsView keeps the user turn paired with each matching
assistant turn so prompts and responses stay aligned in the counts and in the
top-session evidence.

**Session Health** is the exception. It scopes to whole sessions that used a
selected model, but its health scores, outcomes, tool-failure rates, and
compaction counts remain whole-session aggregates rather than being recomputed
from only that model's messages.

!!! note "Dashboard-only scope"

    The model filter applies only to the analytics dashboard. The
    [Generated insights](/docs/recall/?tab=generated) and the session list are not
    scoped by it, so a model selected here does not silently narrow those views. The
    [Usage](/docs/token-usage/) page keeps its own separate model filter.

### Activity heatmap

This GitHub-style contribution graph shows your daily activity. Switch between
**Messages** (message count), **Sessions** (session count), and **Output
tokens**.

![Activity heatmap](/docs/assets/generated/screenshots/heatmap.png)

#### Click-to-filter

Click any cell in the heatmap to filter every chart and the session list to that
single day. The selected cell gets a highlighted border, and an active filter
chip appears in the toolbar. Click the same cell again or dismiss the filter
chip to clear the filter.

![Heatmap filtered to a single day](/docs/assets/generated/screenshots/heatmap-filtered.png)

### Hour of week heatmap

This 7x24 grid shows when you use agents most. Rows represent days of the week,
columns represent hours, and color intensity reflects message volume.

![Hour of week heatmap](/docs/assets/generated/screenshots/hour-of-week.png)

### Activity timeline

Compare message or session counts over time. Switch between **Day**, **Week**,
and **Month** to change how the chart groups activity.

Message bars split into **User**, **Assistant**, and **Other** entries, where
**Other** includes system entries and tool results. Choose **Absolute** to
compare raw counts or **Percent** to compare each role's share of message
volume. Session counts stay a separate metric.

![Activity timeline](/docs/assets/generated/screenshots/activity-timeline.png)

![Analytics timeline comparing message roles](/docs/assets/generated/screenshots/analytics-message-roles.png)

### Top sessions

This ranked list shows your largest sessions by message count, duration, or
output tokens. Click any session to open it in the message viewer.

![Top sessions](/docs/assets/generated/screenshots/top-sessions.png)

### Project breakdown

This bar chart sorts all of your projects by session count or message count, and
shows the average and median messages per session for each one. Click any
project bar to filter the dashboard and session list to that project.

![Project breakdown](/docs/assets/generated/screenshots/project-breakdown.png)

### Session shape distribution

Three histograms show how your sessions are distributed across three dimensions:

1. **Session length.** Number of messages per session.
1. **Session duration.** Time in minutes.
1. **Session autonomy.** Ratio of tool calls to conversation turns.

![Session shape](/docs/assets/generated/screenshots/session-shape.png)

### Tool usage

This panel displays your total tool call count broken down by category (Read,
Edit, Write, Bash, Grep, Glob, Task, Tool, and Other), along with a trend chart
of tool usage over time. Each tool also shows empty, repeat, and recovery
rates. Click any nonzero rate to see the sessions behind it and how many
matching calls each session contains. Each link opens the session at the message
that holds the first matching call.

- **Empty rate.** Divides empty results by calls with a known outcome.
- **Repeat rate.** Divides calls that repeat the previous call's tool and input,
  either verbatim or after normalizing the JSON, by analyzed calls. The previous
  call's outcome does not matter, and calls in the same message do not count. A
  sequence's first call can repeat a previous successful call.
- **Recovery rate.** Divides recovered sequences by recovered plus abandoned
  sequences. A sequence starts with an empty result or an error and recovers
  when a call in a later message returns content. The tool that started the
  sequence owns it. Sequences that are still open or have an unknown ending stay
  out of the denominator.

If a rate has no denominator, it displays **Unavailable**. Rates follow the same
filters as the rest of the Analytics page and count individual calls, so a date
or model filter selects the calls made in that window or by that model.

The **Analyzed** column shows how many calls have been analyzed out of all
calls. After an upgrade, AgentsView recomputes older sessions in the background.
Until a session finishes recomputing, its calls count as not analyzed. Archives
that do not store tool results never analyze calls. PostgreSQL and ClickHouse
replicas receive the analyzed results through their normal pushes.

![Tool usage](/docs/assets/generated/screenshots/tool-usage.png)

![Sessions and calls behind a selected tool rate](/docs/assets/generated/screenshots/tool-rate-evidence.png)

For a model's assessment of one session, see
[Analyze tool effectiveness](/docs/recall/#tool-effectiveness-reports).

### Top skills

The **Top Skills** panel ranks skill-backed tool calls by call count, session
count, recency, agent mix, and project mix. Next to it, the **Skill Usage Over
Time** chart plots the leading skills, groups the long tail into **Other**, and
lets you switch between day, week, and month buckets or hide individual series.
Both views follow your active Analytics date and session filters.

AgentsView populates skill analytics from normalized `skill_name` metadata on
tool calls and from inferred skill names when Codex or Cursor reads a `SKILL.md`
file through a read-like tool call. The panel appears when your local
transcripts contain either explicit skill metadata or enough `SKILL.md` reads
for AgentsView to infer the skill name.

![Top Skills](/docs/assets/generated/screenshots/top-skills.png)

![Skill usage trends](/docs/assets/generated/screenshots/skill-trends.png)

### Velocity metrics

Track performance metrics across your sessions:

- Turn cycle time (p50, p90)
- First response time (p50, p90)
- Messages, characters, and tool calls per active minute
- Breakdown by agent and by session complexity

![Velocity metrics](/docs/assets/generated/screenshots/velocity.png)

### Agent comparison

Compare agents side by side across session count, total messages, average
response time, tool usage patterns, and concentration metrics.

![Agent comparison](/docs/assets/generated/screenshots/agent-comparison.png)

### Session health

The 0.23.0 dashboard adds a **Session Health** section that summarizes your
session-intelligence signals:

- Average health score and derived grade
- Headline counts for the `completed` and `errored` outcomes (`abandoned` and
  `unknown` sessions are excluded here, though they appear alongside them in the
  dashboard's outcome-distribution chart)
- Tool-failure rate and sessions with failures
- Compaction counts, including mid-task compactions
- Score trend over time
- By-agent and by-project score tables

![Session Health section on the dashboard](/docs/assets/generated/screenshots/session-health.png)

For the full four-outcome model behind both this section and
`agentsview report stats`, see
[Session Intelligence](/docs/session-intelligence/#outcome-classification).

### CSV export

To download all analytics data as a CSV file, click **Export CSV** in the
dashboard toolbar. The export includes the summary, activity, projects, tools,
and velocity sections.

______________________________________________________________________

## Generated insights

AgentsView can generate model-written reports over an explicit session scope
using Claude, Codex, Copilot, Gemini, or Kiro. Open **Recall → Generated
insights** to choose the date range, project, session agent, automated-session
scope, template, generator, and optional focus.

See [Recall](/docs/recall/#current-surface) for generation, archive, and privacy
details.

______________________________________________________________________

## Session browser

The left sidebar lists your sessions with virtual scrolling so it stays
responsive even with thousands of entries. On desktop, drag the resize handle
between the sidebar and the content pane to adjust the sidebar width. The width
stays between 220px and 520px and always leaves at least 480px for the content
area. Your browser saves your chosen width in localStorage and restores it on
your next visit.

As of 0.30.0, the sidebar loads from a skinny session-index endpoint and
hydrates visible rows on demand, so large refresh storms (for example, after a
bulk import or `resync`) no longer freeze the list while full payloads arrive.

![Session list](/docs/assets/generated/screenshots/session-list.png)

Each session item shows:

- **Status indicator.** A small dot on the left whose color and animation show
  how recently the session was active and whether it ended cleanly. See
  [Session status indicator](#session-status-indicator) for the full state set.
- **Session name.** The name you gave the session in AgentsView comes first. If
  there is none, AgentsView uses the agent's own title for it, and if there is
  no title, the first message. As of 0.33.0, labels are no longer hard-truncated
  at 50 characters and instead clip to fit the sidebar width.
- **Agent-provided session names.** AgentsView shows the title each agent keeps
  for a session. When an agent records both a name you chose (for example, with
  `/rename`) and a title it generated, your name wins. Agents with titles:
  Claude Code (`/rename` names and generated titles), Codex
  (`session_index.jsonl` thread names), Qwen Code, Copilot CLI (`workspace.yaml`
  names), Gemini CLI (generated summaries), Kimi CLI, OpenClaw, OpenCode, Kilo,
  MiMo Code, Amp, VS Code Copilot, Positron, Windsurf, Kiro, Claude.ai and
  ChatGPT conversation names, Forge, Hermes, Piebald, Cortex Code, WorkBuddy,
  and Command Code. The first message stays the first thing you typed, and a
  title stands in for it only when a session has no user message. Manual in-app
  renames always take precedence and are never overwritten by an agent-provided
  name. As of 0.34.0, Codex titles renamed by the agent are imported from
  `session_index.jsonl` for both current and archived sessions. Qwen Code,
  Gemini CLI, Kimi CLI, and OpenClaw titles, current Claude Code `/rename`
  names, and keeping titles out of the first message are included in 0.45.0.
- **Star button.** Click the star icon or press `s` to star a session. Starred
  sessions are stored in the SQLite database so they survive server restarts.
- **Agent tag.** The agent name on the right side, tinted with the agent's
  accent color.
- **Machine label.** In shared multi-host deployments using
  [PostgreSQL sync](/docs/pg-sync/), sessions from other machines show a machine
  name tag.
- **Project name.** The project associated with the session.
- **Session time.** Relative time such as "2h ago", or the date and time with
  the year when **Always show year** is on.
- **User prompt count.** Number of user messages in the session.

Antigravity sessions show the conversation title Antigravity saves in
`conversation_summaries.db`. Renames appear shortly after that file changes, or
on the next `agentsview source sync`. Names set in AgentsView still win.

Qoder's database title lookup applies to `~/.qoder/projects` and
`~/.qoder-cn/projects`. The Qoder app stores titles in `main.sqlite` under its
data directory: `com.qoder.app.stable` (or `com.qodercn.app.stable` for Qoder
CN) inside `~/Library/Application Support` on macOS, `$XDG_CONFIG_HOME` or
`~/.config` on Linux, and `%APPDATA%` on Windows. If you moved the app's data
directory, AgentsView does not find it. Names take precedence in this order:

1. A name set in AgentsView.
2. A non-blank database title.
3. A file-side name, with a sidecar title ahead of an in-chat rename.

Renames appear at the next sync pass: a watched change in any agent root, a
manual `agentsview source sync`, or startup. For these two roots, blank database
titles or read errors preserve the archived name. A missing database, schema, or
session row falls back to non-empty file-side names, while an empty file-side
name preserves the archived title. Other Qoder roots use file-side names and
clear the archived title when empty.

When a session has a native resume target, the sidebar also shows a direct
native session link so supported agents can reopen their own session instead of
only navigating inside AgentsView.

### Session status indicator

As of 0.27.0, the small dot at the left edge of each session row shows how
recently the session was written to and how it last ended. The same indicator
appears in the dashboard's **Top Sessions** list. Hover over the dot to see a
tooltip explaining the current state.

| State   | Indicator         | Meaning                                                                                                                 |
| ------- | ----------------- | ----------------------------------------------------------------------------------------------------------------------- |
| Working | Pulsing green     | Last write within the last minute                                                                                       |
| Waiting | Tan speech bubble | Session ended on the agent's "your turn" stop reason and was active in the last 10 minutes                              |
| Idle    | Muted green       | 1–10 minutes idle, not awaiting user input                                                                              |
| Stale   | Amber             | 10–60 minutes idle and last assistant message has an unresolved tool call (or the session file was truncated mid-write) |
| Unclean | Red               | Same flagged state as Stale, but idle for more than an hour                                                             |
| Quiet   | Hidden            | Cleanly-ended sessions older than 10 minutes; no dot is rendered                                                        |

The flagged tier (**Stale** and **Unclean**) highlights sessions where an agent
looks like it crashed mid-tool-call or had its session file truncated, so you
can tell them apart from sessions that have not been touched in a while.

Termination classification runs for Claude Code, OpenClaude, Claude Cowork,
Codex, Cline, Codebuff, DeepSeek Harness, IcodeMate, Kilo (legacy), Open Code
Review, Poolside, and RooCode. The Claude Code parser reads the per-message
`stop_reason`, and the Codex parser reads task lifecycle events. Other agents
render as plain time-based states (Working, Idle, and Quiet) without the flagged
tier.

When a parent session has subagents or a continuation chain, the dot reflects
the most recent activity across the group. If a parent is in `tool_call_pending`
while its subagent is actively writing, the dot rolls up to Working green. The
parent's parser status still wins for the Waiting state. A fork running in
parallel does not change that the parent has said *"your turn"*.

### Group by agent

To organize sessions into collapsible sections by agent type:

1. Click the filter icon next to the session count to open the filter dropdown.
2. In the **Display** section, turn on **Group by agent**.

Each agent group displays a color-coded dot, the agent name, and a session
count. Click any agent header to expand or collapse its section. Groups start
collapsed when you first turn grouping on.

### Sub-agent tree

When a session spawns sub-agents or teams, the sidebar organizes them in a
collapsible tree view. Parent sessions show a disclosure triangle that you can
click to expand and view child agents nested underneath, making complex
multi-agent workflows easier to navigate without leaving the session list.

![Sub-agent tree view](/docs/assets/generated/screenshots/subagent-tree.png)

### Forks and subagent sessions

AgentsView automatically detects conversation forks in Claude Code sessions, for
example when you use "retry from here" to branch a conversation. Large forks
(more than 3 user turns) appear as separate session entries grouped with their
parent, while small retries fold into the main session.

Subagent sessions spawned by the Task tool appear under their parent in the
sidebar's [sub-agent tree](#sub-agent-tree) and can also be viewed inline
through the parent session's tool blocks (see
[Subagent linking](#subagent-linking)). Claude companion session layouts are
also linked when the parent can be inferred from the companion directory
structure, including externalized tool result content stored beside the
transcript.

### Session filters

Click the filter icon next to the session count to open a dropdown with
combinable filter options:

![Session filter dropdown](/docs/assets/generated/screenshots/session-filters.png)

![Label and pull request filters](/docs/assets/generated/screenshots/session-label-filters.png)

Label and pull-request filters can find child and automated sessions. The
sidebar displays the containing tree, and an explicit single-turn exclusion
still applies. Use [launcher links](/docs/session-api/#agentsview-session-parent)
to attach independently launched workers to their manager and roll up their
costs.

- **Starred.** Show only starred sessions.
- **Recently Active.** Show only sessions updated within the last 24 hours.
- **Agent.** Searchable multi-select of
  [supported agents](/docs/configuration/#session-discovery). Click an agent to
  toggle it, and select as many agents as you want at once.
- **Machine.** Multi-select of machine labels in shared
  [PostgreSQL sync](/docs/pg-sync/) deployments. The section appears whenever
  the machine list is not empty, and a search box appears inside it when more
  than five machines are listed.
- **Status.** Three-pill multi-select of the [status](#session-status-indicator)
  recency tiers (**Active**, **Stale**, **Unclean**), tinted to match the
  indicator colors. The underlying `?termination=` URL parameter and API also
  accept the parser-side values `clean` and `awaiting_user`, though those are
  not exposed as sidebar pills.
- **Labels.** Enter an exact label such as `ticket=example` and press Enter. Add
  multiple labels to require all of them. You can create labels through the
  [CLI or API](/docs/session-api/#agentsview-session-label).
- **Pull request.** Enter `owner/repo#123` and press Enter to find sessions
  whose retained prompts mention that pull request or whose transcript has a
  structured link. A mention does not establish that the session worked on it.
- **Min Prompts.** Filter to sessions with at least 2, 3, 5, or 10 user
  messages.
- **Hide single-turn.** Hide sessions with one or fewer user messages (shown by
  default).
- **Include automated sessions.** Include sessions classified as automation
  (roborev runs, title generation, AgentsView's own internal prompts, and any
  patterns you added to
  [`[automated] prefixes`](/docs/configuration/#automated-session-detection)).
- **Hide unknown.** Hide sessions whose project could not be determined.

A green dot appears on the filter button while any filter is active. To reset
every filter at once, click **Clear filters** at the bottom of the dropdown.

As of 0.27.0, the sidebar shares its filter store with the analytics dashboard
and the Usage page. Agent, machine, project, minimum user-message threshold,
hide-single-turn, and include-automated selections carry across to the dashboard
panels and the Usage page header, which mounts the same filter widget. The
**Status** filter applies only to the sidebar and dashboard; the Usage page does
not currently filter by termination status. AgentsView persists filter state in
localStorage and serializes it into the URL.

### Direct session links

To copy a shareable URL for the current session, click **Copy link to session**
in the detail header. To copy only the ID, click **Session ID**. Session links
put the provider and native ID in separate path segments, for example:

```
/sessions/codex/550e8400-e29b-41d4-a716-446655440000
```

Older links with an encoded provider prefix still open. IDs without a provider
prefix use a single segment after `/sessions/`.

Session URLs work as bookmarks and can be shared with teammates in shared
deployments using [PostgreSQL sync](/docs/pg-sync/).

A link from another tool that carries an agent's bare session UUID (Pi, OMP, or
Codex) opens the matching session. If the ID is ambiguous, use the full ID with
its provider prefix.

### Open a session by ID

To open a session directly without changing your sidebar filters:

1. Press `Cmd+G` on macOS or `Ctrl+G` on Windows and Linux.
2. Paste a complete session ID or UUID.
3. Press Enter.

If the UUID matches more than one session, use its full ID with the provider
prefix, such as `codex:550e8400-e29b-41d4-a716-446655440000`.

![Open a session by ID or UUID](/docs/assets/generated/screenshots/open-session.png)

### URL filters

You can link directly to filtered views using URL parameters:

```
/sessions?project=myapp&agent=claude&date_from=2025-01-01
```

Available URL filters:

| Parameter                      | Value                                                                                                                     |
| ------------------------------ | ------------------------------------------------------------------------------------------------------------------------- |
| `project`                      | comma-separated project names                                                                                             |
| `agent`                        | comma-separated agent ids                                                                                                 |
| `machine`                      | exact IDs or aliases, comma-separated ID/alias lists, or one unique whole display label                                   |
| `termination`                  | comma-separated [status](#session-status-indicator) tiers — any of `active`, `stale`, `unclean`, `clean`, `awaiting_user` |
| `date`, `date_from`, `date_to` | ISO date or activity-overlap range bounds                                                                                 |
| `active_since`                 | `true` to limit to the last 24 hours                                                                                      |
| `min_messages`, `max_messages` | numeric message count bounds                                                                                              |
| `min_user_messages`            | numeric user-message threshold                                                                                            |
| `include_one_shot`             | `false` to hide single-turn sessions; when omitted, they are shown                                                        |
| `include_automated`            | `true` to include automated sessions                                                                                      |
| `exclude_project`              | comma-separated projects to hide (e.g. `unknown`)                                                                         |

Unknown or ambiguous machine selectors, including stale saved filters, return
HTTP 400 with the known choices. The full selector stays active until you edit
or clear it.

When a URL specifies a single machine's display name, such as
`?machine=Workstation`, the URL switches to that machine's ID once the server
resolves it so the picker shows it checked. Unknown or ambiguous names show an
error instead of earlier results.

### Navigation

Press `]` and `[` to move between sessions in the list. A left border accent
marks the selected session.

______________________________________________________________________

## Message viewer

Selecting a session opens the message viewer in the main content area, which
renders messages in a scrollable virtualized list for large transcripts.

![Message viewer](/docs/assets/generated/screenshots/message-viewer.png)

The session detail header displays the session name, agent, project, a health
grade badge, and a copyable **Session ID**. Click the ID to copy it to your
clipboard for sharing or lookup, or click the grade badge to toggle the signal
panel.

The model badge shows the model used most often in assistant messages. For
Claude Code and Codex, it also shows the recorded reasoning effort, such as
`high`, when that value is the most common effort for the displayed model.
Effort is a model setting rather than a measured token count. Upgrading resyncs
existing sessions to populate it when the source files remain available.

If a parser skipped malformed source lines, records larger than 64 MiB, or
database rows it could not decode while still recovering the session, the header
shows a malformed-lines badge with the persisted count (for example, "3
malformed lines"). For Antigravity IDE and CLI sessions decoded from an
unrecognized SQLite schema fingerprint, the header also shows **Unverified
schema**. That badge means the session was decoded heuristically from a newer
schema and may be incomplete.

AgentsView stores your reading progress for the 500 most recent sessions in the
current browser. When a transcript changes after you have read it, the sidebar
shows an unread dot and the transcript inserts a **New messages** boundary at
the first unread region. In newest-first order, the same boundary is labeled
**Earlier messages**. The marker clears once you move past the new region. It is
browser-local and does not sync through SQLite or PostgreSQL.

### Message layouts

Four layouts control how messages appear. Press `l` to cycle through them,
choose one under **Layout** in the session header's **View** menu, or set one in
**Settings > Appearance**:

| Layout  | Description                                |
| ------- | ------------------------------------------ |
| Default | Expanded messages                          |
| Compact | The same content with tighter spacing      |
| Stream  | Chat without speaker labels                |
| Skim    | One line per message and tool call         |

### Focused transcript mode

Focused mode hides intermediate tool calls, thinking blocks, and partial
assistant messages so you see only user prompts and final assistant responses.
This makes long sessions easier to read as a clean conversation. Task
notifications and stop hook feedback count as part of the turn they interrupt
rather than as new prompts, so a session waiting on background agents still
collapses to its prompts and final answers.

![Focused transcript mode](/docs/assets/generated/screenshots/focused-transcript.png)

To switch between Normal and Focused mode, open the **View** menu in the session
header and, under **Content**, choose **All messages and tool calls** or **Only
your prompts and final answers**. Your choice is saved in localStorage.

### Message display

A colored left border identifies each message's role:

- **Blue.** User messages.
- **Purple.** Assistant messages.

Each message header displays the role label, a timestamp, and a **copy button**
that appears on hover. Click the copy button to copy the full message content to
your clipboard; a checkmark confirms the copy for 1.5 seconds.

Inline `<teammate-message>` content and messages from teammate session ancestry
carry a **Teammate** label, keeping peer-agent replies separate from user
prompts and ordinary subagent output.

Claude Code sessions also include a fork action in each message header when the
local server can launch or return a command. Click it to start a new Claude run
from that point: AgentsView renders the transcript up through that message
ordinal into a temporary prompt, starts `claude` in the session working
directory, and deletes the temporary prompt after launch. In read-only local
mode, the action copies the command instead of launching it. Remote sessions
cannot be forked from the browser.

### Thinking blocks

Assistant thinking blocks appear as collapsible sections with a purple left
border. To show or hide thinking blocks, use the
[block-type filter](#block-type-filtering) in the header.

### Tool blocks

Tool calls appear as collapsible amber-bordered sections containing the tool
name, arguments, and output. When collapsed, the header displays the most useful
input field rather than the first line of rendered content: `TodoWrite` shows
the in-progress (or last) todo with a `→` prefix; `TaskCreate` shows the
subject; `TaskUpdate` shows `#<id> · <status> · <subject>`; `Skill` shows the
skill name; `ToolSearch` shows the first line of the query; and
`Task`/`Agent`/subagent calls show the description (falling back to the prompt).
For tools without a structured preview, the header falls back to the first line
of content, the `command`/`cmd` (Bash), or the `file_path`/`pattern` (Read,
Edit, Write, Glob).

![Tool blocks](/docs/assets/generated/screenshots/tool-blocks.png)

When expanded, tool blocks show structured metadata tags extracted from the tool
call input. For task management tools (`TaskCreate`, `TaskUpdate`, `TaskGet`),
these tags surface the task subject, status, and ID. Bash tool blocks show the
full command text, including multi-line commands such as heredocs that would
otherwise be truncated. Tool result content is stored alongside the tool call
when available so you can inspect both input and output together.

Expanded outputs start in **Raw** mode, which keeps the exact escaped text
recorded by the agent. Switch to **Formatted** to render Markdown, including
headings, lists, tables, and highlighted fenced code; AgentsView sanitizes HTML
before display. The mode changes presentation only, so the stored result and
copy button continue to use the raw output.

![Formatted tool output](/docs/assets/generated/screenshots/tool-output-formatted.png)

Hover over or focus a tool block to reveal copy buttons for the structured input
and, when present, the tool output.

![Copy buttons on a tool block](/docs/assets/generated/screenshots/tool-block-copy-btn.png)

Codex tool calls get dedicated formatting: bash commands, `write_stdin`
operations, and `apply_patch` calls display structured argument previews and
categorized detail labels. Cursor `ApplyPatch` tool calls render as patch or
diff content instead of plain JSON when the Cursor transcript exposes the patch
payload.

When a Codex tool call has subagent result events (status updates captured
during execution), an expandable **history** section appears below the tool
output. The latest event summary appears by default. Click **history** to view
the full chronological timeline of status changes (for example, "wait",
"completed", and "failed") with the source and content for each event.

### Subagent linking

When a Task or Agent tool block links to a subagent session, it shows an
expandable toggle. Click the toggle to read the subagent's full transcript
inline without leaving the parent session. Messages load on demand when you
expand the section, showing the full subagent conversation with role labels and
timestamps.

### Tool call groups

Consecutive tool-only assistant messages collapse into compact "N tool calls"
sections with a gear icon and timestamp. Click a group to expand its individual
tool blocks.

![Tool call groups](/docs/assets/generated/screenshots/tool-groups.png)

### Code blocks

Fenced code blocks render with language labels, monospace formatting, and
horizontal scrolling for long lines. Hover over or focus a code block to reveal
a **copy button** in the corner, then click it to copy the raw code (without
fences or the language tag) to your clipboard.

As of 0.33.0, labeled code fences receive **syntax highlighting** powered by
Shiki. Twelve common languages are bundled: JavaScript, TypeScript, Python,
Bash, JSON, YAML, Markdown, HTML, CSS, Rust, Go, and SQL, along with common
aliases such as `py`, `sh`, and `yml`. Unlabeled or unrecognized languages fall
back to plain text. To keep large sessions fast, highlighting is skipped for
blocks over 50 KB or 800 lines, and the highlighter loads lazily so it has no
cost until the first code fence renders.

Fenced code blocks labeled `mermaid` render as Mermaid diagrams in an
interactive viewer with source-copy and expanded-view controls. If the Mermaid
runtime cannot load, AgentsView keeps the escaped diagram source readable in the
message. While in-session search is active, Mermaid fences render as source code
so matches can be highlighted.

![Copy button on a code block](/docs/assets/generated/screenshots/code-block-copy-btn.png)

### Block-type filtering

Open the **View** menu in the session header to find the **Block Visibility**
group, which toggles seven content categories:

| Category          | What It Controls          |
| ----------------- | ------------------------- |
| User messages     | User messages             |
| Assistant text    | Assistant responses       |
| Thinking blocks   | Thinking/reasoning blocks |
| Tool calls        | Tool call blocks          |
| Code blocks       | Code blocks               |
| System boundaries | System boundary cards     |
| Hooks             | Hook output cards         |

Turning off **Code blocks** collapses each fenced block into an inline
placeholder with its language and an **Expand** button. You can expand or
collapse individual blocks without changing the global filter. Messages that
contain only code keep their placeholder. Filtered code is excluded from
in-session search until you turn **Code blocks** back on.

System boundary cards are the compact rows that mark a session continuation or
resume, an interrupted request, a task notification, or stop hook feedback.
Hiding **System boundaries** removes all of them without affecting the rest of
the transcript.

All categories are visible by default. To restore them all at once, click **Show
all** in the menu.

### Sorting

Press `o` or click the sort button in the header to toggle between newest-first
and oldest-first order. The arrow icon shows the current direction.

### Message navigation

- **`j` or `↓`.** Next message.
- **`k` or `↑`.** Previous message.
- **`Shift+J`.** Next user prompt.
- **`Shift+K`.** Previous user prompt.
- **Click a message.** Select it (marked with a blue outline).

Prompt navigation follows the visible transcript, so it respects Focused mode
and your active block-type filter.

### Jump to a prompt

Press `t` or click **Jump to prompt** in the session header to search the
prompts typed in the session and jump straight to one, even if it sits in a part
of the transcript that has not loaded yet.

### In-session search

Press `Cmd+F` (or `Ctrl+F`) to open a search bar for the current session and
type to find matching text across all visible messages. The search bar displays
the match count and your current position.

![In-session search](/docs/assets/generated/screenshots/in-session-search.png)

Use the arrow buttons or press `Enter` and `Shift+Enter` to step through
matches. AgentsView scrolls each matching message into view and highlights the
search term. Press `Esc` to close the search bar.

Click **Show search results** to open matching snippets grouped by message, then
select any snippet to jump to that occurrence. The overview rail beside the
transcript shows where matches fall throughout the session.

![In-session search results and overview rail](/docs/assets/generated/screenshots/in-session-search-results.png)

Search respects the transcript's active scope. Block-type filters (see
[Block-type filtering](#block-type-filtering)) and Focused mode both narrow what
search matches: hidden categories produce no matches, counts, badges, or
highlights, and turning a category back on or returning to Normal mode makes
that content searchable again without retyping your query. Folded content is
still searched, including collapsed tool output, thinking blocks, `<details>`
sections, and rows not currently scrolled into view; only the active match
expands and scrolls into view. Because the match count, result list, and
overview rail read the same filtered index, they update right away if you toggle
a filter during a search.

### Token usage

When token data is available, the session detail header displays the input and
output token counts for the session. This shows how much context the agent
consumed and how much output it generated.

![Token usage display](/docs/assets/generated/screenshots/token-usage.png)

As of 0.33.0, the header also shows the session's **estimated cost** beside the
token summary, computed from the same data as
[`agentsview session usage`](/docs/session-api/#agentsview-session-usage). Costs
under a cent display as `<$0.01`, costs up to $100 show two decimals, and larger
costs display as whole dollars. The badge is hidden when the session has no
token data or its models have no pricing.

When the selected session has explicit `subagent` descendants, the automatic
header request adds `rollup=true`. A complete priced aggregate displays a
localized total marker and the descendant count. If any contributing row is
unpriced, the header keeps the root session's own cost when available and does
not label it as a total. Sessions without explicit subagent descendants keep the
existing badge.

As of 0.37.1, sessions with per-step usage rows also display a **step count**
next to the token summary. Click it to expand a per-step breakdown where each
row lists the prompt or usage event, the model that served it, its context size
(input tokens plus cache reads and writes), its output tokens, and a per-step
cost estimate when the model is priced. These rows come from the same session
usage API with `?breakdown=true`; see
[`agentsview session usage`](/docs/session-api/#agentsview-session-usage).

For aggregate token usage and estimated cost reports across all sessions, see
the [Token Usage & Costs](/docs/token-usage/) page and the
[`agentsview report usage daily`](/docs/commands/#agentsview-usage-daily) CLI
command.

For a scriptable report on the current session, run
[`agentsview session usage <id>`](/docs/session-api/#agentsview-session-usage)
or call the matching
[`GET /api/v1/sessions/{id}/usage`](/docs/session-api/#agentsview-session-usage)
REST endpoint.

### Signal panel

Click the health grade badge in the session header to open the signal panel for
the current session. It shows:

- Grade and numeric score
- Outcome icon and confidence
- Basis tags showing whether outcome, tool health, and context pressure
  contributed to the score
- Compaction summary, including mid-task compactions
- Penalty chips for applied deductions

When a session does not have enough usable data, the panel shows a small empty
state instead of a score. See
[Session Intelligence](/docs/session-intelligence/) for the full model.

The signal panel also includes **Observed tool sequences**, where you can follow
a failed or empty tool result through later retries, tool switches, and
recovery, and open each call in the transcript. See
[Tool sequences](/docs/session-intelligence/#tool-sequences) for the evidence
and timing details.

### Session vital signs

Click **Analysis** in the session header to open Session Vitals in the right
column. It shows elapsed turn time and measured tool durations.

![Session Vital Signs in context](/docs/assets/generated/screenshots/session-vital-signs.png)

The panel shows these sections, including experimental Recall when available:

- **Session.** Repository and worktree context recorded by the trace, total
  wall-clock time, turn count, tool call count, sub-agent count, and the slowest
  call as a clickable link that scrolls the conversation to that call. Live
  sessions show a `running …+` indicator that ticks forward.
- **Recall.** Provenance-linked entries whose evidence comes from the current
  session. Evidence-range links jump to the supporting transcript message. An
  empty state appears when the local archive has no matching entries.
- **Turn activity.** Each visible user prompt starts a window that ends at the
  next prompt or the session boundary. Bars separate measured tool execution
  from unattributed time, which has no measured phase boundaries. Thinking and
  response generation are not measured. Click a row to jump to its prompt in the
  transcript.
- **Time spent.** Per-category aggregate bars across the normalized taxonomy
  (`Read`, `Edit`, `Write`, `Bash`, `Grep`, `Glob`, `Task`, `Tool`, `Other`, and
  other provider categories). Click a row to filter the rest of the panel to
  that category.
- **Timeline.** A turns lane, per-category lanes, and an activity lane with a
  legend. Hover over a turn segment to see its primary category and duration
  (for example, `Task · 2m`), or click it to scroll the conversation to that
  turn.
- **Calls.** Chronological list of tool calls with horizontal duration bars.
  Parallel `tool_use` runs are bracketed as a single group. Call details start
  collapsed for faster transcript navigation. Sub-agent rows expand inline to
  show the child session's calls.

![Vital Signs panel detail](/docs/assets/generated/screenshots/vital-signs-panel.png)

Tool durations come from paired execution events or a closed linked child
session. A call without either source shows `unknown`, and the summary and
category totals show **Not measured** when no calls have measured durations. A
measured zero stays `0ms`. Categories stay available as filters even without
timing. An open child needs a completed execution interval before its call can
show a duration; otherwise it stays unknown until the child closes.

Session tool and category totals count only time inside the session boundaries,
while individual calls keep their full measured durations, including any time
outside the parent session. Overlapping calls count once in the session total
and once per category, so category totals can overlap each other.

In the conversation column, tool headers show measured durations when available,
and assistant messages display turn-summary lines. AgentsView normalizes tool
labels across agents, so Codex's `exec_command` and Claude's `Bash` appear under
the same "Bash" category in headers and in the Calls list.

Call duration bars in the Calls list scale relative to the longest call in
scope, not total session wall-clock time. Even in long sessions where any single
call is a small fraction of the total, you can still compare call durations.
Very short calls floor at 4% width so they stay visible.

### Progressive loading

Sessions with more than 3,000 messages load the 1,000 most recent messages
first, and older pages load automatically as you scroll up. Smaller sessions
load all messages at once.

### Live updates

When you view an active session, AgentsView streams new messages in real time
using Server-Sent Events. The message list updates incrementally, fetching only
new activity rather than reloading the entire session, so updates arrive faster
and with less overhead.

### Follow latest message

As of 0.30.0, the session header includes a **Follow latest messages** toggle.
When enabled, the message list auto-scrolls as updates stream in, so a
long-running session stays pinned to the newest message without manual
scrolling. The toggle also snaps back to the latest message after a sync
rebuilds the session once the re-rendered list settles.

![Follow latest toggle in the session header](/docs/assets/generated/screenshots/follow-latest-toggle.png)

Scrolling up manually or clicking a specific message turns follow mode off
automatically. Click the toggle again to re-enable it and jump to the latest
message. AgentsView saves this preference in localStorage, so a session you
opened in follow mode reopens in follow mode.

______________________________________________________________________

## Command palette

Press `Cmd+K` (or `Ctrl+K`) to open the command palette, a full-screen search
overlay.

![Command palette](/docs/assets/generated/screenshots/command-palette.png)

### Recent sessions

When the query is empty, the palette lists up to 10 recent sessions from the
sidebar's current list. Typing a short query filters that list by project,
session name, or first message. Search starts at 3 characters, or 2 characters
for Chinese, Japanese, and Korean text.

### Search modes

Choose from three search modes:

- **Full text.** Searches indexed message content with FTS5 and also matches
  session display names and first messages.
- **Semantic.** Ranks message content by meaning using the active embeddings
  index.
- **Hybrid.** Combines semantic and full-text rankings so both conceptual and
  exact-term matches can surface.

The palette remembers your last selected mode across openings and browser
sessions. Results update after a 300ms pause in typing.

If a Full text search hits a transient failure, the palette retries once
automatically. If it still fails, select **Retry** to try again.

!!! tip

    For deeper searches across full transcripts — including thinking, tool inputs, tool result
    content, and regex patterns — use the
    [`agentsview session search`](/docs/session-api/#agentsview-session-search) CLI
    or the `GET /api/v1/search/content` HTTP endpoint added in 0.30.0. The command
    palette indexes message bodies via FTS5 for fast scoring; the content-search
    endpoints add substring and regex modes and cover tool I/O as well.

![Search results](/docs/assets/generated/screenshots/search-results.png)

Results are grouped by session so each session shows its best match and no
single long session dominates the list. Full text also matches session display
names and first messages, so you can find sessions by title.

In Full text mode, use the sort toggle in the palette header to switch between
**Relevance** (best matches first) and **Recency** (newest sessions first).
Semantic and Hybrid use their backend rankings, so the toggle is hidden in those
modes.

Semantic and Hybrid require an enabled
[`[vector]`](semantic-search.md#enabling-vector) configuration and an active
embeddings index. If setup, index state, or the embeddings service prevents a
search, the palette stays in the selected mode and shows remediation steps
instead of silently falling back to Full text.

Each result uses a compact row:

- **Full text.** May show the session name and a sanitized snippet with
  highlighted search terms.
- **Semantic and Hybrid.** Lead with a plain-text matching snippet. Content
  search does not return a session name or highlight markup.
- **All modes.** Show an agent-colored dot, project and result time, and a
  copyable session ID.

Select a result to open that session and scroll straight to the matching
message.

### Project and date filters

Once your query is long enough to search, the palette shows project and date
controls. Search starts with the sidebar's selected project, and you can pick
another project or **All Projects** without changing the sidebar.

Choose a relative period, calendar period, or custom dates. These filters apply
to Full text, Semantic, and Hybrid searches.

![Command palette with project and date filters](/docs/assets/generated/screenshots/search-filters.png)

### Keyboard navigation

- **`↑` / `↓`.** Navigate results.
- **`Enter`.** Select the current result.
- **`Esc`.** Close the palette.

______________________________________________________________________

## Session management

### Renaming sessions

To rename a session inline, double-click its name in the sidebar, or right-click
the session and select **Rename**. Press Enter to save or Escape to cancel.
AgentsView saves the custom name as `display_name` in the database and uses it
instead of the default first-message title.

### Trash

To move a session to the trash:

- With a session selected, press `Del` or `Backspace` to open the **Delete
  session** confirmation, then choose **Move to Trash**.
- Or right-click the session and select **Delete** to move it to the trash
  directly.

An undo toast appears briefly so you can recover the session right away. Trashed
sessions are hidden from all listings and analytics.

Click **Trash** in the header navigation to view trashed sessions. From the
trash page, you can restore individual sessions or delete them permanently.
Click **Empty Trash** to permanently delete every trashed session at once.

### Batch selection

Click the **Multi-select** toggle in the sidebar header to enter selection mode.
A checkbox appears beside each session. Click sessions to check them, click
**All** to select every visible session, or click **Clear** to deselect. The
batch toolbar shows how many sessions are selected and provides a **Delete**
action that moves the entire selection to the trash at once. Turn off
**Multi-select** to return to normal browsing.

### Pinned messages

Click the pin icon in any message header to pin that message. Pinned messages
are saved to the database and accessible from **Pinned** in the header
navigation.

The pinned page lays out your pinned messages in a gallery-style grid. Each card
shows the message content (expandable for long messages), its session and
project, and actions to copy the content, unpin it, or jump back to the source
message in its session.

### Session resume menu

Select a session and open **Resume** in the detail header. For supported local
agents, you can launch the configured terminal, use the default terminal, or
copy the exact resume command. Cursor resume resolves the original workspace
path and passes it as `--workspace` to `cursor agent --resume`. From the same
menu, you can copy the session directory and open it in detected editors or file
browsers.

Pi sessions use `pi --session` with the transcript path when available, or the
native session ID. For Pi, **Copy command** includes the working directory so
you can paste the complete command into a shell. Augure Code sessions use
`augure resume`, and Open Interpreter sessions use `interpreter resume`. Copying
a session's directory path works even if that directory has been deleted.

Local Codex sessions add **Open in Codex Desktop**, which deep-links to the
stored thread. Local Claude sessions add **Open in Claude Code**, which opens a
new Code session for the stored working directory; when the native Claude
Desktop opener is detected, it stays available as a separate resume target.

![Session resume menu](/docs/assets/generated/screenshots/session-resume-menu.png)

For supported remote sessions, choose **Copy command** and paste the resume
command into a shell on the machine that owns the transcript. Launching a
terminal, opening an editor, and native agent desktop links are local-session
actions.

![Copy command in a remote session's Resume menu](/docs/assets/generated/screenshots/remote-resume-command.png)

The `agentsview session list --resume` and `--active` CLI modes use the same
recent-activity signal to print a compact terminal table for picking up
in-flight work.

These actions let you resume where you left off without manually navigating to
the project directory.

### The agentsview:// URL scheme

The desktop app registers the `agentsview://` URL scheme so other tools can open
a session directly in the app:

```bash
open "agentsview://sessions/<session-id>"
```

Add an optional `?msg=<ordinal>` or `?msg=last` query parameter to scroll to a
specific message. If the app is already running, its window comes forward to
that session; otherwise the app starts and opens the session once the backend is
ready. See the [Session API](/docs/session-api/) guide for the full contract.

______________________________________________________________________

## Session export

To download the current session as a standalone HTML file, press `e`, or open
the **More actions** menu in the header and choose **Download HTML export**. The
exported file includes styled message rendering and works offline. It also
includes a **Normal / Focused** radio toggle in the document header so the
recipient can switch to [focused mode](#focused-transcript-mode) (only user
prompts and final assistant responses) without re-running the export.

The same menu also includes **Copy markdown export link**, which copies a URL
for the session's markdown export endpoint. You can use that link in scripts,
notes, or shared internal tooling when you want plain text instead of standalone
HTML. When the active session has a source file path, the menu also offers
**Copy source file path** for handoff to another tool or terminal. The markdown
export works well for passing session context to another agent.

The markdown export route is:

```text
GET /api/v1/sessions/{id}/md
```

By default, it exports only the current session. Add the `depth` query parameter
to inline descendants:

- **`?depth=1`.** Include direct child sessions.
- **`?depth=all`.** Include the full descendant tree.

Subagent children appear inline near the tool call that spawned them, while
other child sessions are appended after the parent transcript in
`<child_session>` blocks. The markdown payload also includes XML-style tags for
metadata, thinking blocks, tool calls, code blocks, and child-session
boundaries.

A typical agent handoff flow:

```bash
curl "http://127.0.0.1:8080/api/v1/sessions/<session-id>/md?depth=all" \
  -o session-context.md
```

Attach `session-context.md` to another agent run or paste its contents into a
prompt so the next agent can inspect the full session tree without opening the
AgentsView UI.

### Secrets in exports

HTML and Markdown exports mask detected secrets such as API keys, tokens, and
private keys. They use the same detector and masked format as search results,
for example `AKIA…PLJM` for an AWS access key. Message text, thinking text, and
tool input and output are all masked. Detection is best-effort; see
[SECURITY.md](https://github.com/kenn-io/agentsview/blob/main/SECURITY.md#secrets-subsystem)
for its limits.

To export the stored text unchanged, add `reveal=true`:

```bash
curl "http://127.0.0.1:8080/api/v1/sessions/<session-id>/md?depth=all&reveal=true" \
  -o session-context.md
```

Only a request made directly from localhost can use `reveal`. A remote or
proxied request that asks for it receives HTTP 403, the same rule as revealing
secrets in search and the secrets list.

______________________________________________________________________

## Publish to Gist

To share a session on GitHub Gist, press `p` or open the **More actions** menu
in the header. The menu offers **Publish public Gist** and **Publish secret
Gist**; pressing `p` publishes publicly.

![Publish modal](/docs/assets/generated/screenshots/publish-modal.png)

For local publishing, AgentsView prefers your existing GitHub CLI login. If no
token is saved in AgentsView and `AGENTSVIEW_GITHUB_TOKEN` is not set, it runs
`gh auth token` and uses that token to create the gist. If the GitHub CLI is not
authenticated yet, run `gh auth login` once.

For remote or proxied AgentsView access, save a GitHub token in AgentsView
before publishing. Remote requests do not fall back to the server process
environment or GitHub CLI credentials.

If neither a saved token, `AGENTSVIEW_GITHUB_TOKEN`, nor `gh auth token` is
available for local publishing, the publish modal asks for a GitHub personal
access token with the `gist` scope. AgentsView saves that token to your config
file and reuses it for future publishes.

!!! warning

    Secret gists are unlisted, not access-controlled: they don't appear in your
    public gist list or in search, but anyone who has the URL can read them.

After publishing, the modal displays two URLs:

- **View URL.** Rendered view of the gist.
- **Gist URL.** Direct GitHub link.

Both URLs include copy buttons and an **Open in Browser** action.

The published gist masks detected secrets in the transcript and in the gist
description, matching [session export](#secrets-in-exports). The publish
endpoint, `POST /api/v1/sessions/{id}/publish`, accepts the same localhost-only
`reveal=true` parameter, though the UI does not use it.

______________________________________________________________________

## Session upload

Upload session JSONL files from other machines through the API:

```bash
curl -F "file=@session.jsonl" \
  "http://127.0.0.1:8080/api/v1/sessions/upload?project=myapp"
```

AgentsView stores uploaded files in `~/.agentsview/uploads/` and syncs them into
the database.

Replacing an existing session with fewer messages returns `409 Conflict` without
changing the stored transcript. To allow an intentional shorter rewrite, add
`allow_shorter=true` to the upload query.

______________________________________________________________________

## Keyboard shortcuts

Press `?` to open a modal listing all keyboard shortcuts.

| Key       | Action                            |
| --------- | --------------------------------- |
| `Cmd+K`   | Open command palette              |
| `Cmd+G`   | Open a session by ID or UUID       |
| `Cmd+F`   | Search within current session     |
| `Esc`     | Close modal / deselect session    |
| `j` / `↓` | Next message                      |
| `k` / `↑` | Previous message                  |
| `Shift+J` | Next user prompt                  |
| `Shift+K` | Previous user prompt              |
| `t`       | Jump to a prompt                  |
| `]`       | Next session                      |
| `[`       | Previous session                  |
| `o`       | Toggle sort order                 |
| `l`       | Cycle message layout              |
| `s`       | Star / unstar current session     |
| `Del`     | Delete / archive selected session |
| `r`       | Trigger sync                      |
| `e`       | Export session                    |
| `p`       | Publish to Gist                   |
| `?`       | Show shortcuts                    |

Use `Ctrl` instead of `Cmd` on Windows and Linux. Most shortcuts are disabled
while you are typing in an input field, though `Esc` always works.

______________________________________________________________________

## Settings

Click the gear icon in the header to open the Settings page, which is organized
into these sections:

![Settings page](/docs/assets/generated/screenshots/settings.png)

| Section            | What You Can Configure                                                                                   |
| ------------------ | -------------------------------------------------------------------------------------------------------- |
| Language           | Interface language (Azerbaijani, English, French, Japanese, Korean, Spanish, Simplified Chinese, or Traditional Chinese)       |
| Appearance         | Theme (light/dark), high-contrast mode, chart colors, message colors, message layout, reading width, zoom, always show year, block visibility |
| Date ranges        | Browser-local checkbox for linking date selections across Sessions, Usage, Activity, and Quality          |
| Session Providers  | Enable session providers, inspect their session directories, and add alternate agent homes               |
| Tool-result images | Choose whether future imports keep, drop, or offload tool-result images; preview and remove images already stored |
| Terminal           | Default terminal emulator for session resume                                                             |
| Embeddings         | Current semantic-index build phase, progress, throughput, ETA, last result, and local generations        |
| GitHub             | Personal access token for Gist publishing                                                                |
| Remote Access      | Remote connections toggle, auth token, connect to remote server                                          |

Choose **Español** for Spanish. A browser set to Spanish selects it on first
load, and a saved language preference takes precedence.

![Settings with Spanish selected](/docs/assets/generated/screenshots/settings-spanish.png)

![Embedding build progress](/docs/assets/generated/screenshots/settings-embeddings.png)

![Settings remote access section](/docs/assets/generated/screenshots/settings-remote.png)

![Chart color palette setting](/docs/assets/generated/screenshots/settings-chart-colors.png)

Language, theme, high contrast, message colors, message layout, reading width,
zoom, always show year, block visibility, and Date ranges preferences are stored
in local storage for the current browser or desktop webview profile. Each
profile keeps its own choices. The optional `zoom_level` setting supplies a
default when no local zoom or text-size preference exists.

Chart colors use the server-wide `chart_palette` setting in
`~/.agentsview/config.toml`. Agent directory overrides, terminal settings, the
saved GitHub token, and the local server's remote-access authentication settings
live in that file too. Worktree mapping rules moved to the
[Data page](/docs/data/#rules) and live in the local archive database. See
[Remote Access](/docs/remote-access/) for details on remote access settings.

______________________________________________________________________

## About dialog

To open the About dialog, click the version number in the status bar, or choose
**About AgentsView** from the desktop build's menu. The dialog shows the author,
version, commit, and build date.

![About dialog](/docs/assets/generated/screenshots/about-dialog.png)

______________________________________________________________________

## Zoom

**Settings > Appearance** has one **Zoom** setting for the entire interface in
both the browser and desktop app, with options for 67%, 75%, 80%, 90%, 100%,
110%, 120%, 125%, 130%, 150%, 175%, and 200%. Existing text-size preferences
migrate to Zoom: a saved non-default zoom takes precedence, and otherwise a
saved non-default text size takes precedence over 100%.

To set a default zoom level for clients without a local preference, edit
`~/.agentsview/config.toml` and add `zoom_level = 120`. The field accepts any of
the twelve values listed above. Restart the daemon and reload the client after
editing the file. Zoom changes made in the interface stay local to that browser
or desktop webview and do not write to `config.toml`.

The desktop status bar and keyboard shortcuts change the same local setting.
Press `Cmd+Plus` and `Cmd+Minus` (or `Ctrl+Plus` and `Ctrl+Minus` on Windows) to
zoom in and out, and press `Cmd+0` (or `Ctrl+0` on Windows) to reset Zoom to
100%.

______________________________________________________________________

## Theme

Click the theme toggle in the header or open **Settings > Appearance** to switch
between light and dark mode. Your choice persists across sessions.

![Light theme](/docs/assets/generated/screenshots/theme-light.png)

![Dark theme](/docs/assets/generated/screenshots/theme-dark.png)

**Settings > Appearance** also includes a **high-contrast** mode for better
legibility. Both theme and high-contrast preferences persist across sessions.

The **Chart colors** control picks the categorical palette for the dashboard
skill trend and Usage charts:

- **Agentsview.** The project palette.
- **Matplotlib.** Matplotlib's categorical families without their grays.
- **Color-blind safe.** Petroff's color sequences, chosen so series stay
  distinguishable with common forms of color blindness. Past the tenth series,
  colors repeat with a pattern: Usage bars, areas, and treemap tiles are
  hatched, then cross-hatched, then dotted. Legends and lists show the same
  pattern.

This setting is server-wide rather than browser-local, so every client connected
to the same server uses the same palette. You can change it even when the server
reads from a read-only store such as PostgreSQL or ClickHouse.

### Message colors

**Message colors** in **Settings > Appearance** sets the color for each message
type: user messages, assistant text, thinking blocks, tool calls, and code
blocks. It helps readers who have trouble telling the default blue and purple
apart.

- **Color-blind safe.** Applies hues that stay distinct with common forms of
  color blindness: blue for user messages and vermillion for assistant text. The
  hues are the Okabe-Ito colors behind matplotlib's `seaborn-v0_8-colorblind`
  style. Thinking and code use darker versions of its pink and yellow because
  the originals are too faint on light themes. This preset also turns on line
  patterns.
- **Mark message types with line patterns.** Gives each type its own bar: solid
  for user messages, double for assistant text, dotted for thinking, and dashed
  for tool calls. Code blocks get a dashed frame when they have a custom color.
  Types then differ by shape as well as color, and the patterns stay visible in
  the Stream layout, which hides the role header.
- Each type has a color picker, a hex field, and quick-pick swatches. One color
  applies to light, dark, and high-contrast modes. The message background uses a
  light tint of the color, and the label text mixes the color toward the theme's
  text color.
- When a color makes label text hard to read (below 4.5:1) or the color bar hard
  to see (below 3:1) in any mode, the setting names the modes and the measured
  ratio. The color still applies so you can decide.
- **Reset.** Restores one type's theme color. **Reset to defaults** restores
  every type and turns off patterns.

### Iframe embedding

When you embed AgentsView in an iframe, the parent page can control the theme
using `postMessage`:

```javascript
iframe.contentWindow.postMessage(
  { type: "theme:set", theme: "dark" },
  "*"
);
```

Accepted values for `theme` are `"light"` and `"dark"`.

______________________________________________________________________

## Sync

AgentsView syncs session files automatically on startup and watches for changes
in real time. The status bar displays sync status:

- **Syncing indicator.** Green text showing progress percentage and phase
  (`Scanning [project]...` or parse progress).
- **Last sync time.** Relative timestamp ("synced 2h ago") that updates
  automatically. Hover over it to see the exact sync timestamp.

Press `r` to run a manual sync. The sync button in the header spins while
syncing.

If the frontend and backend versions differ, the status bar displays a red
version mismatch warning. Click it to reload.

### History from another machine

[Attach a saved archive](/docs/configuration/#history-from-a-saved-archive) to
browse a retired machine's Claude and Codex sessions beside your current ones.
Choose a backup snapshot or an existing archive directory, and use
`machine_name` to label a single-machine archive without changing its captured
identity. Ordinary sync reads pending archive sources after local work. Once
indexed, the history stays browsable with the drive disconnected. Reparsing
requires the archive to be available again.

### Full resync

![Resync modal](/docs/assets/generated/screenshots/resync-modal.png)

To open the Full Resync modal:

1. Click the gear icon in the header to open **Settings**.
2. Click **Full Resync** at the bottom of the Settings page.

Full resync re-parses all session files from scratch in a non-destructive flow,
preserving existing session data and carrying forward orphaned sessions that are
no longer on disk. Use it after upgrading AgentsView or when sessions appear to
be parsed incorrectly.

The modal tracks live progress through four states:

1. **Confirm.** Describes what the resync does, with **Start** and **Cancel**
   buttons.
1. **Progress.** Live counter and progress bar ("Syncing X / Y sessions...").
   The modal stays open until the resync finishes.
1. **Done.** Shows synced, skipped, and total session counts.
1. **Error.** If the resync fails, shows the error with **Retry** and **Close**
   buttons.
