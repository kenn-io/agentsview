# See what your AI coding agents did, and what it cost

AgentsView brings your AI coding sessions into one searchable archive. Browse
recorded conversations from more than 60 agent formats, compare activity and
costs, and reuse lessons from past work. The archive stays on your machine
unless you turn on a feature that shares it.

[New in v0.46.1](/docs/changelog/#0461): Antigravity transcript and read-only
mount fixes, plus repaired container builds.

## Install

On macOS or Linux:

```bash
curl -fsSL https://agentsview.io/install.sh | bash
```

On Windows:

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://agentsview.io/install.ps1 | iex"
```

Desktop app, pip/uvx, and Docker installs are covered in the
[quick start](/docs/quickstart/). Then [follow the guide](/guide/) or read the
[documentation](/docs/).

## Your coding sessions, in one archive

A background server watches the session directories your agents already write
and imports supported formats into a local SQLite archive. It finds the default
directories automatically. You can configure other locations. Supported
harnesses include Claude Code, OpenClaude, Codex, Augure Code, Augure Desktop,
Cline CLI, Crush, CodeBuddy CN, Gemini, Copilot (CLI, VS Code, and Visual
Studio), Cursor, Cursor IDE, IcodeMate, Qwen Code, DeepSeek TUI and Harness,
Mistral Vibe, Zed, Warp, OpenCode, Positron, Posit Assistant, Claude Cowork,
Aider, Antigravity, gptme, Junie, Muse Code, OMO, StepCode, Kilo, Kimi, Kiro,
OpenHands, Goose, Grok, RooCode, Trae, Windsurf, fx, and dozens more. Every
supported source is listed in
[session discovery](/docs/configuration/#session-discovery).

Import [Claude.ai chats through Chrome](/docs/chat-import/#sync-claudeai-chats),
including Google sign-in. Enable [Codex Cloud](/docs/configuration/#codex-cloud)
on one machine to fetch ChatGPT dot, Codex web, and cloud task chats. Cloud
chats without token counts show plan usage; optional live capture records only
turns heard while connected and can send queued messages.

- **60+** agent formats parsed
- **1** binary, zero accounts
- **SQLite** archive of record

## See when your agents are actually working

The [Activity dashboard](/docs/activity/) shows peak concurrency and the exact
moment it happened, active versus idle time, agent-minutes across parallel
sessions, and cost. Scope it to any day, week, month, or custom range, and
filter by project, agent, and machine. Switch from concurrency to user-message
or assistant-message frequency to compare prompts and output. The Analytics
dashboard adds message-role comparisons and Git outcome totals.

## Know what every agent costs

[Token and cost reports](/docs/token-usage/) read from the pre-indexed archive
instead of reparsing raw session files every time. Pricing tracks LiteLLM and
OpenRouter rates with an offline fallback, and cache-aware accounting covers
prompt-cache creation and reads. Compare Total Input with Uncached Input, and
choose Smooth, Lines, or Bars for the daily cost chart. Select a cost breakdown
item to narrow totals and charts, open `hermes-cron` to compare jobs, and group
model variants with configurable aliases.

```bash
agentsview usage daily          # last 30 days, terminal table
agentsview usage statusline     # $9.61 today
agentsview capture run -- claude -p "fix the tests"
```

## Search and score every transcript

Find a conversation by project and date in the
[command palette](/docs/usage/#command-palette), or open it by ID with
`Ctrl/Cmd+G`. Full-text search matches words; opt-in
[semantic and hybrid search](/docs/semantic-search/) match by meaning.
[Health scores](/docs/session-intelligence/) and
[quality signals](/docs/quality/) link back to the transcript so you can check
the evidence. Tool sequences follow errors, retries, switches, and recovery
through the recorded calls without running a model. Tool Usage rates link to
sessions with empty results, repeated calls, and recovery. For an assessment of
one session,
[Analyze tool effectiveness](/docs/recall/#tool-effectiveness-reports) generates
a report with citations to the calls it judged.

## Turn transcripts into durable knowledge

[Recall](/docs/recall/) (experimental) extracts decisions, warnings, and project
facts from your archive. Each entry links to the messages that support it.
Approve useful entries or archive rejected ones; your decisions survive later
extraction changes. Generated Insights write model-authored reports over an
explicit session scope.

## Your agents can read it too

The same archive you browse is available to your agents:

- **CLI:** scriptable reports and session queries.
- **REST:** programmatic [session and usage access](/docs/session-api/).
- **MCP:** [session history as assistant tools](/docs/mcp/).
- **SSE:** live message streams as sessions run.
- **Web:** embedded Svelte UI served from the binary.
- **Desktop:** native app sharing the same data directory.

An agent can check what a previous session already tried, quote its own history,
or watch its spend mid-run. Eight new read-only MCP tools expose file edits,
tool calls, child sessions, Activity reports, tool usage, cost comparisons, and
offline guide search. Native conversation-memory plugins bundle recall
instructions and session-start refresh for Claude Code and Codex. See
[memory setup](/docs/mcp/#focused-memory-profile).

## One machine or the whole team

SQLite is the archive of record. From there:

- [PostgreSQL sync](/docs/pg-sync/) pushes each machine's archive to a shared
  team backend with per-machine labels and a read-only merged server.
- [ClickHouse sync](/docs/clickhouse-sync/) serves the dashboard from a remote
  copy, including semantic and hybrid search when embeddings are pushed.
  Session edits stay in SQLite.
- [DuckDB mirror](/docs/duckdb/) serves analytical reads locally or over the
  Quack protocol.
- [Filesystem sync](/docs/filesystem-sync/) and
  [artifact folder sync](/docs/artifact-sync/) move sessions between machines
  without any database server.
- [Hosted raw sync](/docs/hosted-raw-sync/) keeps original provider files in
  hosted storage with device authentication and resumable uploads. Opt-in
  server parsing makes those sessions browsable in PostgreSQL without a local
  SQLite archive or a separate `pg push`.
- [SSH sync](/docs/remote-access/#ssh-remote-sync) fetches sessions from Linux
  and macOS hosts without a manual AgentsView install or a remote daemon.
- [Local archives](/docs/commands/#agentsview-archive) preserve a retired
  computer's history, annotations, and original Claude and Codex files.
- [Remote access](/docs/remote-access/) stays loopback-only by default, with
  explicit flags for SSH forwards and authenticated exposure.

## Local by default. Shared when you choose

Your agent transcripts are some of the most sensitive data on your machine.
AgentsView starts with one local SQLite archive and a loopback-only server.
Session content leaves the machine only when you choose a feature such as
PostgreSQL or ClickHouse sync, remote DuckDB access, Generated Insights, GitHub
publishing, or hosted raw sync. Each feature documents what it sends and where
it goes.

## Start

Install AgentsView and it finds the sessions that are already on your machine.

- [Follow the guide](/guide/)
- [Run the quickstart](/docs/quickstart/)
- [Read the docs](/docs/)
