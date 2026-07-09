---
title: Usage Guide
description: Complete guide to the AgentsView web interface
---

AgentsView serves a full-featured web application for browsing,
searching, and analyzing your AI agent sessions. This page walks
through every part of the interface.

## Dashboard

When you open AgentsView with no session selected, you see the
analytics dashboard. It provides a high-level overview of your
agent activity across all projects.

![Analytics dashboard](/assets/generated/screenshots/dashboard.png)

The dashboard header includes:

- **Project filter** — typeahead to scope everything to a single
  project. Type to filter by name; each entry shows its session
  count. Navigate with arrow keys, select with Enter, and close
  with Escape.
- **Search bar** — opens the command palette (`Cmd+K`)
- **Sync button** — triggers a manual sync of session files
- **Theme toggle** — switch between light and dark mode
- **Import button** — opens the [Chat Import](/chat-import/)
  dialog for importing Claude.ai or ChatGPT conversations
- **Shortcuts button** (`?`) — shows all keyboard shortcuts

The status bar at the bottom shows session count, message count,
project count, last sync time, and the build version.

On the normal local `agentsview serve` runtime, the session lists in
the sidebar update automatically from a global SSE event stream
(debounced during busy syncs). The dashboard, Usage page, and
Activity page do not refetch their charts on every sync event —
each pairs a manual refresh button with a relative "Updated…"
timestamp, and the Analytics dashboard also refreshes periodically
on its own. Click refresh, or change the date range, to pull the
latest numbers.

For range-based concurrency and agent-minutes reporting, use the
top-level [Activity](/activity/) page.

### Summary Cards

Six cards at the top of the dashboard show key metrics for the
selected date range:

![Summary cards](/assets/generated/screenshots/summary-cards.png)

| Card | Description |
|------|-------------|
| Sessions | Total session count |
| Messages | Total message count |
| Projects | Number of active projects |
| Active Days | Days with at least one session |
| Messages/Session | Average with median and p90 |
| Concentration | Most active project and its share |

### Date Range Picker

Quick presets for 7 days, 30 days, 90 days, 1 year, and All,
plus custom start/end date inputs. The **All** preset shows
every session regardless of age. All charts update when the
range changes. The same range picker is used on the dashboard,
the [Usage](/token-usage/#usage-dashboard) page, and the
[Activity](/activity/) page so presets and behavior stay
consistent across panels.

![Date range picker](/assets/generated/screenshots/date-range.png)

Preset ranges are **rolling** by default: a page left open
across midnight rolls the window forward at the next refresh
tick, sync event, or manual refresh, instead of staying
anchored to the day it loaded. Manually editing either date
input pins the range; on the Usage and Activity pages,
navigating to explicit range URLs also pins the range while the
bare page URL returns to rolling mode. The `All` preset always
pins to `(earliest_session, today)`.

### Model Filter

The dashboard toolbar includes a **Model** dropdown that scopes
every panel to one or more AI models. By default the button reads
**Model: All** and nothing is filtered.

![Dashboard model filter](/assets/generated/screenshots/analytics-model-filter.png)

Open the dropdown for a searchable list of the models found in
your sessions, then click models to include them. The button then
shows the chosen model — for example **Model: gpt-4o** — or
**Model: 3 selected** once several are active. Click the
**All models** row at the top of the list to clear the filter and
return to every model. Selected models also appear as removable
chips beneath the toolbar.

While a model filter is active, every dashboard panel reflects
only the selected model(s): the summary cards, activity chart,
heatmap, hour-of-week grid, projects, session shape, velocity,
tools, skills, top sessions, and the Session Health rollup.

Model filtering is **message-grain**, unlike the session-grain
project and agent filters, because a single session can switch
models across turns. A session is included when it has at least
one message from a selected model, and most panels count only the
matching messages. The user turn paired with a matching assistant
turn is kept alongside it — even though a user message carries no
model of its own — so prompts and their responses stay aligned in
the counts and in the top-session evidence.

**Session Health** is the exception. It is scoped to whole
sessions that used the selected model, but its health scores,
outcomes, tool-failure rates, and compaction counts stay
whole-session aggregates — they are not recomputed from only that
model's messages.

!!! note "Dashboard-only scope"
    The model filter applies only to the analytics dashboard. The
    [Session Insights](/insights/) page and the session list are
    not scoped by it, so a model selected here does not silently
    narrow those views. The [Usage](/token-usage/) page keeps its
    own separate model filter.

### Activity Heatmap

A GitHub-style contribution graph showing daily activity. Toggle
between message count and session count.

![Activity heatmap](/assets/generated/screenshots/heatmap.png)

#### Click-to-Filter

Click any heatmap cell to filter all charts and the session list
to that single day. The selected cell gets a highlighted border,
and an active filter chip appears in the toolbar. Click the same
cell again (or dismiss the filter chip) to deselect.

![Heatmap filtered to a single day](/assets/generated/screenshots/heatmap-filtered.png)

### Hour of Week Heatmap

A 7x24 grid showing when you use agents most. Rows are days of
the week, columns are hours. Color intensity represents message
volume.

![Hour of week heatmap](/assets/generated/screenshots/hour-of-week.png)

### Activity Timeline

A stacked chart showing messages, sessions, tool calls, and
thinking blocks over time. Toggle between daily, weekly, and
monthly granularity. Includes breakdown by agent.

![Activity timeline](/assets/generated/screenshots/activity-timeline.png)

### Top Sessions

A ranked list of your longest sessions by message count or
duration. Click any session to jump directly to it in the
session viewer.

![Top sessions](/assets/generated/screenshots/top-sessions.png)

### Project Breakdown

Bar chart of all projects sorted by session or message count.
Shows average and median messages per session for each project.
Click any project bar to filter the dashboard and session list
to that project.

![Project breakdown](/assets/generated/screenshots/project-breakdown.png)

### Session Shape Distribution

Three histograms showing the distribution of:

1. **Session length** — number of messages per session
2. **Session duration** — time in minutes
3. **Session autonomy** — ratio of tool calls to conversation turns

![Session shape](/assets/generated/screenshots/session-shape.png)

### Tool Usage

Total tool call count with breakdowns by category (Read, Edit,
Write, Bash, Search, Web, Task) and by agent. Includes a trend
chart showing tool usage over time.

![Tool usage](/assets/generated/screenshots/tool-usage.png)

### Top Skills

The Top Skills panel ranks skill-backed tool calls by call count,
session count, recency, agent mix, project mix, and week-by-week
trend. It is populated from normalized `skill_name` metadata on
tool calls and from inferred skill names when Codex or Cursor reads
a `SKILL.md` file through a read-like tool call. It appears when
your local transcripts include either explicit skill metadata or
enough `SKILL.md` reads for AgentsView to infer the skill name.

![Top Skills](/assets/generated/screenshots/top-skills.png)

### Velocity Metrics

Performance metrics including:

- Turn cycle time (p50, p90)
- First response time (p50, p90)
- Messages, characters, and tool calls per active minute
- Breakdown by agent and by session complexity

![Velocity metrics](/assets/generated/screenshots/velocity.png)

### Agent Comparison

Side-by-side metrics across agents: session count, total
messages, average response time, tool usage patterns, and
concentration metrics.

![Agent comparison](/assets/generated/screenshots/agent-comparison.png)

### Session Health

The 0.23.0 dashboard adds a **Session Health** section that rolls up
the new session-intelligence signals. It shows:

- average health score and derived grade
- headline counts for the `completed` and `errored` outcomes
  (`abandoned` and `unknown` sessions are not counted here, but are
  visible in the dashboard's outcome-distribution chart alongside
  them)
- tool-failure rate and sessions with failures
- compaction counts, including mid-task compactions
- score trend over time
- by-agent and by-project score tables

![Session Health section on the dashboard](/assets/generated/screenshots/session-health.png)

See [Session Intelligence](/session-intelligence/#outcome-classification)
for the full four-outcome model that both this section and
`agentsview stats` derive from.

### CSV Export

Click **Export CSV** in the dashboard toolbar to download all
analytics data as a CSV file. Includes summary, activity,
projects, tools, and velocity sections.

---

## Session Insights

AgentsView can generate AI-powered summaries and analysis of your
coding sessions using Claude, Codex, Copilot, or Gemini. Click
**More → Insights** in the header navigation to open the Insights
page, where you can generate daily activity digests, multi-day
summaries, and deeper analyses of your agent workflow patterns —
scoped by project or across everything.

See the [Session Insights](/insights/) page for full
documentation.

---

## Trends

Open **More → Trends** in the header navigation for ad-hoc
term-frequency line charts over your session history. Type one
term per line into the textarea, hit **Refresh**, and AgentsView
counts how often each term appears in user and assistant message
content over the selected window.

![Trends page](/assets/generated/screenshots/trends.png)

Use Trends to track topics or technologies as your work shifts
over time — for example, plotting `rust`, `typescript`, and
`python` to see which language you've been spending the most
time in this quarter, or watching whether mentions of a problem
keyword like `flaky` or `timeout` are trending up or down.

The toolbar controls the window and resolution:

- **From / To** — date inputs at the top of the page; defaults
  cover the last year.
- **Granularity** — `day`, `week`, or `month`. Pick coarser
  buckets for longer windows.
- **Normalize by number of messages** — toggle to chart per-term
  rate instead of raw counts, so a busy week doesn't drown out
  a quiet one.

Each line in the **Terms** textarea is a separate term, capped
at 12 terms per chart. Within a line, pipe-separated variants
fold into a single series. AgentsView also adds a simple
plural form (just appending `s`) for each variant, and for
single-word variants ending in `c` it expands the silent-`e`
stem (`slic` matches `slice`, `slices`, `sliced`, `slicing`) —
beyond those two cases, spell out other forms explicitly as
pipe variants:

```text
rust
type|types|typing
docker|kubernetes|k8s
```

Matching is case-insensitive. Single-word matchers honor word
boundaries — `cat` won't match `catalog`. Multi-word matchers
match as substrings. Each line accepts up to 8 variants.

The chart panel uses one color per term. Hover a line to
highlight that term's series and draw point markers at each
bucket; for exact per-bucket counts, switch granularities or
read off the y-axis. The companion table below the chart lists
each term's color swatch, expanded variants, and total — `Count`
in raw mode or `Per 1k messages` in normalized mode.

Page state lives in the URL — `from`, `to`, `granularity`,
`normalized`, and repeated `term=` parameters — so any view is
shareable and bookmarkable.

The same data is available through the
`GET /api/v1/trends/terms` endpoint for scripting, and works
under both local `agentsview serve` and shared
[`pg serve`](/pg-sync/) deployments.

---

## Session Browser

The left sidebar lists all sessions with virtual scrolling for
smooth performance even with thousands of sessions. On desktop,
drag the resize handle between the sidebar and the content pane
to adjust the sidebar width. The width is constrained between
220px and 520px, always leaving at least 480px for the content
area. Your preferred width is saved in localStorage and restored
on next visit.

As of 0.30.0, the sidebar loads from a skinny session-index
endpoint and hydrates the visible rows on demand, so large
refresh storms (e.g. after a bulk import or `resync`) no
longer freeze the list while the full payloads arrive.

![Session list](/assets/generated/screenshots/session-list.png)

Each session item shows:

- **Status indicator** — small dot on the left whose color and
  animation reflect both how recently the session was active and
  whether it ended cleanly. See
  [Session status indicator](#session-status-indicator) for the
  full state set.
- **Session name** — display name if set, otherwise first message
  text. OpenCode sessions use their native session titles. As of
  0.27.0, Copilot CLI sessions use the `name` field from the
  session's `workspace.yaml` when present, falling back to the
  first user message otherwise. As of 0.33.0, labels are no
  longer hard-truncated at 50 characters — the full label is
  clipped responsively to the sidebar width instead.
- **Agent-provided session names** — several agents record a
  session title themselves (Claude Code's `/rename`, Codex
  `session_index.jsonl` thread names, Claude.ai and ChatGPT
  conversation names, Forge, Hermes, Kiro, Piebald, Cortex Code,
  and Command Code's `.meta.json` titles). As of 0.33.0, the
  sidebar shows these titles automatically when
  present. Manual in-app renames always take precedence and are
  never overwritten by an agent-provided name. As of 0.34.0, Codex
  titles renamed by the agent are imported from `session_index.jsonl`
  for both current and archived sessions.
- **Model name** — the AI model used for the session, shown when
  available (including Codex session models).
- **Star button** — click the star icon or press `s` to star
  a session. Starred sessions persist in the SQLite database
  so they survive server restarts.
- **Agent tag** — agent name on the right side, tinted with the
  agent's accent color.
- **Machine label** — when using [PostgreSQL sync](/pg-sync/),
  sessions from other machines show a machine name tag. Only
  visible in shared multi-host deployments.
- **Project name** — abbreviated, right-aligned
- **Relative time** — "2h ago", "Mon", "Dec 1"
- **User prompt count** — number of user messages in the session

When a session has a native resume target, the sidebar also exposes
a direct native session link so supported agents can reopen their own
session instead of only navigating inside AgentsView.

### Session Status Indicator

As of 0.27.0, the small dot at the left edge of each session
row encodes how recently the session was written to and how it
last ended. The same indicator appears in the dashboard's
**Top Sessions** list. Hover the dot for a tooltip explaining
the current state.

| State | Indicator | Meaning |
|-------|-----------|---------|
| Working | Pulsing green | Last write within the last minute |
| Waiting | Tan speech bubble | Session ended on the agent's "your turn" stop reason and was active in the last 10 minutes |
| Idle | Muted green | 1–10 minutes idle, not awaiting user input |
| Stale | Amber | 10–60 minutes idle and last assistant message has an unresolved tool call (or the session file was truncated mid-write) |
| Unclean | Red | Same flagged state as Stale, but idle for more than an hour |
| Quiet | Hidden | Cleanly-ended sessions older than 10 minutes; no dot is rendered |

The "flagged" tier (Stale and Unclean) is meant to surface
sessions that look like an agent crashed mid tool call or had
its session file truncated, so they're easy to pick out from
sessions that simply haven't been touched in a while.

Termination classification currently runs for **Claude Code**
and **Codex** sessions — those parsers read the per-message
`stop_reason` (Claude) or task lifecycle events (Codex). Other
agents render as plain time-based states (Working / Idle /
Quiet) without the flagged tier.

When a parent session has subagents or a continuation chain,
the dot reflects the freshest activity across the group: a
parent in `tool_call_pending` whose subagent is currently
writing rolls up to Working green. The parent's parser status
still wins for the Waiting state — a fork running in parallel
doesn't change that the parent has said *"your turn"*.

### Group by Agent

Click the group-by-agent toggle in the sidebar header to
organize sessions into collapsible sections by agent type.
Each agent group shows a color-coded dot, agent name, and
session count. Click an agent header to expand or collapse
its section. Groups start collapsed when first enabled.

### Sub-Agent Tree

When a session spawns sub-agents or teams, the sidebar
organizes them in a collapsible tree view. Parent sessions
show a disclosure triangle; click to expand and see child
agents nested underneath. This makes complex multi-agent
workflows easier to navigate without leaving the session list.

![Sub-agent tree view](/assets/generated/screenshots/subagent-tree.png)

### Forks and Subagent Sessions

AgentsView automatically detects conversation forks in Claude
Code sessions — for example, when you use "retry from here" to
branch a conversation. Large forks (more than 3 user turns)
appear as separate session entries grouped with their parent.
Small retries fold into the main session.

Subagent sessions spawned by the Task tool are organized
under their parent in the sidebar's
[sub-agent tree](#sub-agent-tree) and are also viewable
inline through the parent session's tool blocks (see
[Subagent Linking](#subagent-linking) below). Claude companion
session layouts are also linked when the parent can be inferred
from the companion directory structure, including externalized tool
result content stored beside the transcript.

### Session Filters

Click the filter icon next to the session count to open a
dropdown with several filter categories that can be combined:

![Session filter dropdown](/assets/generated/screenshots/session-filters.png)

- **Starred** — toggle to show only starred sessions
- **Recently Active** — toggle to show only sessions updated
  within the last 24 hours
- **Agent** — searchable multi-select of
  [supported agents](/configuration/#session-discovery). Click an
  agent to toggle its selection; multiple agents can be active
  at once.
- **Machine** — searchable multi-select of machine names.
  Surfaces in shared [PostgreSQL sync](/pg-sync/) deployments
  when more than five machines have pushed sessions.
- **Status** — three-pill multi-select of the
  [status](#session-status-indicator) recency tiers (**Active**,
  **Stale**, **Unclean**), tinted to match the indicator colors.
  The underlying `?termination=` URL parameter and API also accept
  the parser-side values `clean` and `awaiting_user`, but those
  aren't exposed as sidebar pills.
- **Min Prompts** — filter to sessions with at least 2, 3, 5,
  or 10 user messages
- **Include single-turn** — toggle to include sessions with one
  or fewer user messages (excluded by default to reduce noise)
- **Include automated** — toggle to include sessions classified
  as automation (roborev runs, title generation, AgentsView's own
  internal prompts, and any patterns you've added to
  [`[automated] prefixes`](/configuration/#automated-session-detection))
- **Hide unknown** — toggle to hide sessions whose project
  could not be determined

When any filter is active, a green dot appears on the filter
button. Click **Clear filters** at the bottom of the dropdown
to reset all filters at once.

As of 0.27.0, the sidebar filter store is shared with the
analytics dashboard and the Usage page: agent, machine, project,
min user-message threshold, hide-single-turn, and include-automated
selections applied in the sidebar carry across to the dashboard
panels and the Usage page header, which mounts the same filter
widget. The **Status** filter is sidebar- and dashboard-only —
the Usage page does not currently filter by termination status.
Filter state is also persisted to localStorage and serialized
into the URL.

### Direct Session Links

Each session has a shareable URL. Click the session ID in the
detail header to copy the link, or use the URL bar directly:

```
/sessions/550e8400-e29b-41d4-a716-446655440000
```

Session URLs work as bookmarks and can be shared with teammates
when using [PostgreSQL sync](/pg-sync/) for shared deployments.

### URL Filters

URL parameters are supported for direct linking:

```
/sessions?project=myapp&agent=claude&date_from=2025-01-01
```

Available URL filters:

| Parameter | Value |
|-----------|-------|
| `project` | comma-separated project names |
| `agent` | comma-separated agent ids |
| `machine` | comma-separated machine names |
| `termination` | comma-separated [status](#session-status-indicator) tiers — any of `active`, `stale`, `unclean`, `clean`, `awaiting_user` |
| `date`, `date_from`, `date_to` | ISO date or activity-overlap range bounds |
| `active_since` | `true` to limit to the last 24 hours |
| `min_messages`, `max_messages` | numeric message count bounds |
| `min_user_messages` | numeric user-message threshold |
| `include_one_shot` | `false` to hide single-turn sessions (default) |
| `include_automated` | `true` to include automated sessions |
| `exclude_project` | comma-separated projects to hide (e.g. `unknown`) |

### Navigation

Use `]` and `[` to move between sessions in the list. The
selected session is highlighted with a left border accent.

---

## Message Viewer

Selecting a session opens the message viewer in the main content
area. Messages display in a scrollable list with virtual
rendering for large sessions.

![Message viewer](/assets/generated/screenshots/message-viewer.png)

The session detail header shows the session name, agent, project, a
health grade badge, and a copyable **Session ID**. Click the ID to
copy it to the clipboard for sharing or lookup. Click the grade
badge to toggle the signal panel.

If a parser skipped malformed source lines while still recovering
the session, the header shows a malformed-lines badge with the
persisted count (for example "3 malformed lines"). For Antigravity IDE and
CLI sessions decoded from an unrecognized SQLite schema fingerprint,
the header also shows **Unverified schema**. That badge means the
session was decoded heuristically from a newer schema and may be
incomplete.

### Message Layouts

Four layouts control how messages are rendered. Cycle between
them with the `l` key or the layout button in the header, or pick
one directly in Settings > Appearance:

| Layout | Description |
|--------|-------------|
| Default | Full card layout with colored borders and spacing |
| Compact | Condensed view with minimal spacing |
| Stream | Continuous flow optimized for reading |
| Skim | Collapses tool calls to summary headers for fast skimming |

### Focused Transcript Mode

Focused mode strips intermediate tool calls, thinking blocks,
and partial assistant messages, showing only user prompts and
final assistant responses. This makes long sessions easier to
read as a clean conversation transcript.

![Focused transcript mode](/assets/generated/screenshots/focused-transcript.png)

Toggle between Normal and Focused mode using the transcript
mode button in the session header. The header adjusts
responsively to fit the available space. The mode preference
is saved in localStorage.

### Message Display

Each message has a colored left border indicating role:

- **Blue** — user messages
- **Purple** — assistant messages

The header shows the role label, timestamp, and a **copy
button** that appears on hover. Click it to copy the full
message content to the clipboard — a checkmark confirms the
copy for 1.5 seconds.

Claude Code sessions also show a fork action on each message header
when the local server can launch or return a command. Clicking it
starts a new Claude run from the selected point by rendering the
transcript through that message ordinal into a temporary prompt,
starting `claude` in the session working directory, and removing the
temporary prompt after launch. In read-only local mode the action
copies the command instead of launching it; remote sessions cannot be
forked from the browser.

### Thinking Blocks

Assistant thinking blocks appear as collapsible sections with a
purple left border. Toggle visibility for thinking blocks using
the [block-type filter](#block-type-filtering) in the header.

### Tool Blocks

Tool invocations display as collapsible amber-bordered sections
showing the tool name, arguments, and output. When collapsed,
the header surfaces the most meaningful input field rather than
the first line of the rendered content. `TodoWrite` shows the
in-progress (or last) todo with a `→` prefix; `TaskCreate`
shows the subject; `TaskUpdate` shows `#<id> · <status> ·
<subject>`; `Skill` shows the skill name; `ToolSearch` shows
the first line of the query; and `Task`/`Agent`/subagent calls
show the description (falling back to the prompt). For tools
without a structured preview, the header falls back to the
first line of content, the `command`/`cmd` (Bash), or the
`file_path`/`pattern` (Read, Edit, Write, Glob).

![Tool blocks](/assets/generated/screenshots/tool-blocks.png)

When expanded, tool blocks display structured metadata tags
extracted from the tool call input. For task management tools
(TaskCreate, TaskUpdate, TaskGet), these tags show the task
subject, status, and ID at a glance. Bash tool blocks show
the full command text, including multi-line commands like
heredocs that would otherwise be truncated. Tool result content
is stored alongside the tool call when available, giving a
complete view of input and output.

Hover or focus a tool block to reveal copy buttons for the
structured input and, when present, the tool output.

![Copy buttons on a tool block](/assets/generated/screenshots/tool-block-copy-btn.png)

Codex tool calls receive special formatting: bash commands,
write_stdin operations, and apply_patch calls display with
structured argument previews and categorized detail labels.
Cursor `ApplyPatch` tool calls render as patch/diff content instead
of plain JSON when the Cursor transcript exposes the patch payload.

When a Codex tool call has subagent result events — status
updates captured during execution — an expandable **history**
section appears below the tool output. Click it to see the
full chronological timeline of status changes (e.g. "wait",
"completed", "failed") with source and content for each
event. The latest event summary is shown by default.

### Subagent Linking

When a Task or Agent tool block is linked to a subagent
session, it shows an expandable toggle. Click it to view the
subagent's full transcript inline without leaving the parent
session. Messages load on demand when the section is expanded,
showing the complete subagent conversation with role labels
and timestamps.

### Tool Call Groups

Consecutive tool-only assistant messages are grouped into
compact "N tool calls" sections with a gear icon and timestamp.
Click to expand individual tool blocks within the group.

![Tool call groups](/assets/generated/screenshots/tool-groups.png)

### Code Blocks

Fenced code blocks render with language labels, monospace
formatting, and horizontal scrolling for long lines. Hover or
focus a code block to reveal a **copy button** in the corner;
clicking it copies the raw code (no fences or language tag)
to the clipboard.

As of 0.33.0, labeled code fences get **syntax highlighting**
powered by Shiki. Twelve common languages are bundled —
JavaScript, TypeScript, Python, Bash, JSON, YAML, Markdown,
HTML, CSS, Rust, Go, and SQL (plus their usual aliases like
`py`, `sh`, and `yml`). Unlabeled or unrecognized languages
fall back to plain text. To keep large sessions fast,
highlighting is skipped for blocks over 50 KB or 800 lines,
and the highlighter loads lazily so it costs nothing until the
first code fence renders.

Fenced code blocks labeled `mermaid` render as Mermaid diagrams
in an interactive viewer with source-copy and expanded-view
controls. If the Mermaid runtime cannot load, AgentsView keeps
the escaped diagram source readable in the message. When
in-session search is active, Mermaid fences render as source code
so matches can be highlighted.

![Copy button on a code block](/assets/generated/screenshots/code-block-copy-btn.png)

### Block-Type Filtering

Click the filter icon in the message viewer header to open a
dropdown that toggles visibility of five content categories:

| Category | What It Controls |
|----------|-----------------|
| User | User messages |
| Assistant | Assistant responses |
| Thinking | Thinking/reasoning blocks |
| Tool | Tool call blocks |
| Code | Code blocks |

All categories are visible by default. When any are hidden,
a badge on the filter button shows the count of hidden types.
Click **Show all** to restore visibility.

### Sorting

Toggle between newest-first and oldest-first with the `o` key
or the sort button in the header. The arrow icon indicates the
current direction.

### Message Navigation

- `j` or `↓` — next message
- `k` or `↑` — previous message
- Click a message to select it (blue outline)

### In-Session Search

Press `Cmd+F` (or `Ctrl+F`) to open a search bar within the
current session. Type to find matching text across all visible
messages. The match count and current position are shown in the
search bar.

![In-session search](/assets/generated/screenshots/in-session-search.png)

Use the arrow buttons or `Enter` / `Shift+Enter` to jump
between matches. The matching message scrolls into view and the
search term is highlighted. Press `Esc` to close the search bar.

### Token Usage

The session detail header displays token usage when available,
showing input and output token counts for the session. This
gives a quick view of how much context the agent consumed
and how much it generated.

![Token usage display](/assets/generated/screenshots/token-usage.png)

As of 0.33.0, the header also shows the session's **estimated
cost** next to the token summary, computed from the same data
as [`agentsview session usage`](/session-api/#agentsview-session-usage).
Costs under a cent display as `<$0.01`, costs up to $100 with
two decimals, and larger costs as whole dollars. The badge is
hidden when the session has no token data or its models have
no pricing.

As of 0.37.1, sessions with per-step usage rows also show a
**step count** next to the token summary. Click it to expand a
per-step breakdown: each row lists the prompt or usage event,
the model that served it, its context size (input tokens plus
cache reads and writes), its output tokens, and a per-step cost
estimate when the model is priced. The rows come from the same
session usage API with `?breakdown=true` — see
[`agentsview session usage`](/session-api/#agentsview-session-usage).

For aggregate token usage and estimated cost reports across
all sessions, see the
[Token Usage & Costs](/token-usage/) page and the
[`agentsview usage daily`](/commands/#agentsview-usage-daily)
CLI command.

For a scriptable report on the current session, use
[`agentsview session usage <id>`](/session-api/#agentsview-session-usage)
or the matching
[`GET /api/v1/sessions/{id}/usage`](/session-api/#agentsview-session-usage)
REST endpoint.

### Signal Panel

Click the health grade badge in the session header to open the
signal panel for the current session. The panel shows:

- grade and numeric score
- outcome icon and confidence
- basis tags showing whether outcome, tool health, and context
  pressure contributed to the score
- compaction summary, including mid-task compactions
- penalty chips for the deductions that were applied

When a session does not have enough usable data, the panel shows a
small empty state instead of a score. See
[Session Intelligence](/session-intelligence/) for the full model.

### Session Vital Signs

The right column of an open session shows a **Session Vital
Signs** panel with timing data derived from the message
timestamps. Toggle it from the session header.

![Session Vital Signs in context](/assets/generated/screenshots/session-vital-signs.png)

It has four stacked sections:

- **Session summary** — total wall-clock, turn count, tool call
  count, sub-agent count, and the slowest call as a clickable
  link that scrolls the conversation to that call. Live sessions
  show a `running …+` indicator that ticks forward.
- **Time spent** — per-category aggregate bars across the
  normalized taxonomy (`Read`, `Edit`, `Write`, `Bash`, `Grep`,
  `Glob`, `Task`, `Tool`, `Other`, plus a `Mixed` bucket for
  turns split across categories). Click a row to filter the rest
  of the panel to that category.
- **Timeline** — turns lane plus per-category lanes plus an
  activity lane, with a legend. Hover a turn segment to see its
  primary category and duration (e.g. `Task · 2m`); click to
  scroll the conversation to that turn.
- **Calls** — chronological list of tool calls with horizontal
  duration bars. Parallel `tool_use` runs are bracketed as a
  single group. Sub-agent rows expand inline to show the child
  session's calls.

![Vital Signs panel detail](/assets/generated/screenshots/vital-signs-panel.png)

Inline in the conversation column, each `ToolBlock` header gets
a duration badge, and each assistant message gets a turn-summary
line ("turn 2m 18s · 3 calls"). Parallel non-sub-agent calls
render with a striped bar and a `≤duration` upper bound — the
JSONL source has only one timestamp per assistant message, so
per-call precision inside parallel groups isn't recoverable for
non-sub-agent calls. Tool labels are normalized across agents,
so Codex's `exec_command` and Claude's `Bash` show up under the
same "Bash" category in headers and in the Calls list.

Call duration bars in the Calls list are scaled relative to the
longest call in scope, not total session wall-clock — so even in
long sessions where any single call is a small fraction of the
total, call-vs-call comparison stays legible. Very short calls
floor at 4% width to remain visible.

### Progressive Loading

Sessions with more than 3,000 messages load the most recent
1,000 messages first. Older pages load automatically as you
scroll up. Smaller sessions load all messages at once.

### Live Updates

When viewing an active session, AgentsView uses Server-Sent
Events to stream new messages in real time. The message list
updates incrementally — only new activity is fetched, rather
than reloading the entire session — so updates arrive faster
and with less overhead.

### Follow Latest Message

As of 0.30.0, the session header has a **Follow latest
messages** toggle. When active, the message list auto-scrolls
to the newest message as updates stream in, so a long-running
session stays pinned to the tail without manual scrolling.
The toggle is also handy after a sync that rebuilt the
session — it snaps back to the latest message once the
re-rendered list settles.

![Follow latest toggle in the session header](/assets/generated/screenshots/follow-latest-toggle.png)

Cancellation is automatic: scrolling up by hand or clicking a
specific message turns follow mode off. Clicking the toggle
again re-engages it and jumps to the latest message. The
preference is persisted in localStorage, so a session you
opened in follow mode comes back in follow mode.

---

## Command Palette

Press `Cmd+K` (or `Ctrl+K`) to open the command palette — a
full-screen search overlay.

![Command palette](/assets/generated/screenshots/command-palette.png)

### Recent Sessions

With an empty or short query (under 3 characters), the palette
shows your 10 most recent sessions. Type to filter by project
name or first message.

### Full-Text Search

Type 3 or more characters to search across all message content
and session names. Results appear in real time with 300ms
debounce.

!!! tip
    For deeper searches across full transcripts — including tool
    inputs, tool result content, and regex patterns — use the
    [`agentsview session search`](/session-api/#agentsview-session-search)
    CLI or the `GET /api/v1/search/content` HTTP endpoint added
    in 0.30.0. The command palette indexes message bodies via
    FTS5 for fast scoring; the content-search endpoints add
    substring and regex modes and cover tool I/O as well.

![Search results](/assets/generated/screenshots/search-results.png)

Results are grouped by session — each session shows its best
matching result. This prevents a single long session from
dominating the results list. The search also matches against
session display names and first messages, so you can find
sessions by title.

Use the sort toggle in the palette header to switch between
**Relevance** (best matches first) and **Recency** (newest
sessions first).

Each result shows:

- **Role badge** — U (user) or A (assistant) in a colored box
- **Snippet** — matching text with highlighted search terms
- **Session name** — the session this result belongs to
- **Project name** — right-aligned

Select a result to jump to that session and scroll directly to
the matching message.

### Keyboard Navigation

- `↑` / `↓` — navigate results
- `Enter` — select current result
- `Esc` — close palette

---

## Session Management

### Renaming Sessions

Double-click a session name in the sidebar to rename it inline.
You can also right-click a session to open a context menu and
select **Rename**. Press Enter to save or Escape to cancel. The
custom name persists as a `display_name` in the database and
overrides the default first-message title.

### Trash

Press `Del` (or `Backspace`) with a session selected, or
right-click and select **Delete**, to move it to the trash.
An undo toast appears briefly to let you recover the session
immediately. Trashed sessions are hidden from all listings
and analytics.

Click **More → Trash** in the header navigation to view
trashed sessions. From the trash page you can restore
individual sessions or permanently delete them. Use **Empty
trash** to permanently delete all trashed sessions at once.

### Batch Selection

Click the **Multi-select** toggle in the sidebar header to enter
selection mode. A checkbox appears on each session; click
sessions to check them, or use **All** to select every visible
session and **Clear** to deselect. The batch toolbar shows how
many are selected and a **Delete** action that moves the whole
selection to the trash at once. Toggle multi-select off to return
to normal browsing.

### Pinned Messages

Click the pin icon on any message header to pin it. Pinned
messages are saved to the database and accessible from
**More → Pinned** in the header navigation.

The pinned page shows a gallery-style grid of pinned messages.
Each card shows the message content (expandable for long
messages), the session and project it belongs to, and actions
to copy the content, unpin, or navigate back to the source
message in its session.

### Session Resume Menu

Right-click a session in the sidebar to open a context menu
with three resume actions:

- **Reopen** — reopen the session in the agent that created it
- **Terminal** — launch a terminal in the session's working
  directory
- **Open Directory** — open the session's working directory in
  Finder (macOS) or Explorer (Windows)

GitHub Copilot CLI and Cursor sessions also appear in the
resume dropdown. Cursor resume resolves the original workspace
path automatically and passes it as `--workspace` to
`cursor agent --resume`.

For Claude sessions on macOS, a **Claude Desktop** option
appears at the bottom of the resume menu. It opens the session
in Claude Desktop's Code tab via the `claude://resume` URL
scheme.

The `agentsview session list --resume` and `--active` CLI modes use
the same recent-activity signal to produce a compact terminal table
for picking up in-flight work.

These actions let you quickly pick up where you left off
without manually navigating to the project directory.

---

## Session Export

Press `e` or open the export menu in the header to download the
current session as a standalone HTML file. The exported file
includes styled message rendering and works offline. As of
0.30.0, the export ships with a **Normal / Focused** radio
toggle in the document header so the recipient can flip into
[focused mode](#focused-transcript-mode) — only user prompts
and final assistant responses — without re-running the export.

The same menu also includes **Copy markdown export link**, which
copies a URL for the session's markdown export endpoint. That link
can be used in scripts, notes, or shared internal tooling when you
want a text-oriented representation instead of the standalone HTML
export. When the active session has a source file path, the menu
also offers **Copy source file path** for quick handoff to another
tool or terminal. The markdown export is particularly well-suited
for handing session context to another agent.

The markdown export route is:

```text
GET /api/v1/sessions/{id}/md
```

By default, it exports only the current session. Add the
`depth` query parameter to inline descendants:

- `?depth=1` — include direct child sessions
- `?depth=all` — include the full descendant tree

Subagent children are embedded inline near the tool call that
spawned them. Other child sessions are appended after the parent
transcript in `<child_session>` blocks. The markdown payload also
includes XML-style tags for metadata, thinking blocks, tool
calls, code blocks, and child-session boundaries.

Typical agent handoff flow:

```bash
curl "http://127.0.0.1:8080/api/v1/sessions/<session-id>/md?depth=all" \
  -o session-context.md
```

Then attach `session-context.md` to another agent run or paste
its contents into a prompt so the next agent can inspect the
full session tree without opening the AgentsView UI.

---

## Publish to Gist

Press `p` or click the publish button to share a session via
GitHub Gist. As of 0.33.0, the publish button is a dropdown
with two entries: **Publish public Gist** and **Publish secret
Gist**. The `p` shortcut publishes publicly.

![Publish modal](/assets/generated/screenshots/publish-modal.png)

AgentsView prefers your existing GitHub CLI login for local
publishing. If no token is saved in AgentsView and
`AGENTSVIEW_GITHUB_TOKEN` is not set, it runs `gh auth token` and
uses that token to create the gist. Run `gh auth login` once if
the GitHub CLI is not authenticated yet.

For remote or proxied AgentsView access, save a GitHub token in
AgentsView before publishing. Remote requests do not use the server
process environment or GitHub CLI credential as a fallback.

If neither a saved token, `AGENTSVIEW_GITHUB_TOKEN`, nor
`gh auth token` is available for local publishing, the publish
modal prompts for a GitHub personal access token with the `gist`
scope. That token is saved to your config file and reused for
future publishes.

!!! warning
    Secret gists are unlisted, not access-controlled: they don't
    appear in your public gist list or in search, but anyone who
    has the URL can read them.

After publishing, the modal shows two URLs:

- **View URL** — rendered view of the gist
- **Gist URL** — direct GitHub link

Both have copy buttons and an "Open in Browser" action.

---

## Session Upload

Upload session JSONL files from other machines via the API:

```bash
curl -F "file=@session.jsonl" \
  "http://127.0.0.1:8080/api/v1/sessions/upload?project=myapp"
```

Uploaded files are stored in `~/.agentsview/uploads/` and
synced into the database.

---

## Keyboard Shortcuts

Press `?` to see all shortcuts in a modal overlay.

| Key | Action |
|-----|--------|
| `Cmd+K` | Open command palette |
| `Cmd+F` | Search within current session |
| `Esc` | Close modal / deselect session |
| `j` / `↓` | Next message |
| `k` / `↑` | Previous message |
| `]` | Next session |
| `[` | Previous session |
| `o` | Toggle sort order |
| `l` | Cycle message layout |
| `s` | Star / unstar current session |
| `Del` | Delete / archive selected session |
| `r` | Trigger sync |
| `e` | Export session |
| `p` | Publish to Gist |
| `?` | Show shortcuts |

Shortcuts are disabled when typing in an input field. `Esc`
always works.

---

## Settings

Click the gear icon in the header to open the Settings page.
Settings are organized into sections:

![Settings page](/assets/generated/screenshots/settings.png)

| Section | What You Can Configure |
|---------|----------------------|
| Language | Interface language (English, Simplified Chinese, Traditional Chinese, or Korean) |
| Appearance | Theme (light/dark), high-contrast mode, message layout, text size, block visibility, desktop zoom level |
| Agent Directories | Custom paths for each agent's session files. For Devin CLI, point at the local root that contains `cli/` (for example a redacted `.../Application Support/devin` path), not copied config or OAuth files. |
| Terminal | Default terminal emulator for session resume |
| Worktree Mappings | Map worktree paths back to their main project (see [Worktree Project Mappings](/configuration/#worktree-project-mappings)) |
| GitHub | Personal access token for Gist publishing |
| Remote Access | Remote connections toggle, auth token, connect to remote server |

![Settings remote access section](/assets/generated/screenshots/settings-remote.png)

Changes are persisted to `~/.agentsview/config.toml` and
survive restarts. See [Remote Access](/remote-access/) for
details on the remote access settings.

---

## About Dialog

Click the version number in the status bar or select **About**
from the header menu to open the About dialog. It shows the
current version, build date, git commit, and links to the
changelog and GitHub repository.

![About dialog](/assets/generated/screenshots/about-dialog.png)

---

## Desktop Zoom

In the desktop app, use `Cmd+Plus` and `Cmd+Minus` (or
`Ctrl+Plus` / `Ctrl+Minus` on Windows) to zoom in and out.
`Cmd+0` resets to the default zoom level. The zoom level can
also be set in the Settings page under the **Appearance** tab.

---

## Theme

Click the theme toggle in the header or go to Settings >
Appearance to switch between light and dark mode. The
preference is saved and persists across sessions.

![Light theme](/assets/generated/screenshots/theme-light.png)

![Dark theme](/assets/generated/screenshots/theme-dark.png)

Settings > Appearance also offers a **high-contrast** mode for
greater legibility and a **text size** control (90–130%) that
scales message and interface text. Both preferences are saved
and persist across sessions.

### Iframe Embedding

When embedding AgentsView in an iframe, the parent page can
control the theme via `postMessage`:

```javascript
iframe.contentWindow.postMessage(
  { type: "theme:set", theme: "dark" },
  "*"
);
```

Accepted values for `theme` are `"light"` and `"dark"`.

---

## Sync

AgentsView automatically syncs session files on startup and
watches for changes in real time. The sync status is shown in
the status bar:

- **Syncing indicator** — green text showing progress percentage
  and phase (`Scanning [project]...` or parse progress)
- **Last sync time** — relative timestamp ("synced 2h ago")
  that updates automatically. Hover to see the exact sync
  timestamp.

Press `r` to trigger a manual sync. The sync button in the
header shows a spinning animation while syncing.

The status bar also shows a version mismatch warning (red) if
the frontend and backend versions differ. Click it to reload.

### Full Resync

![Resync modal](/assets/generated/screenshots/resync-modal.png)

Click the **gear icon** in the header to open the Full Resync
modal. This re-parses all session files from scratch using a
non-destructive flow — existing session data is preserved and
orphaned sessions (those no longer present on disk) are carried
forward. This is useful after upgrading AgentsView or when
sessions appear to be parsed incorrectly.

The modal shows live progress as sessions are processed:

1. **Confirm** — describes what the resync does, with Start and
   Cancel buttons
2. **Progress** — live counter and progress bar ("Syncing X / Y
   sessions..."). The modal stays open until the resync finishes.
3. **Done** — shows synced, skipped, and total session counts
4. **Error** — if the resync fails, shows the error with Retry
   and Close buttons
