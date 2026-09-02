# See what your AI coding agents did, and what it cost

AgentsView is the local-first system of record for AI coding sessions. It merges
sessions from more than 60 agent formats into one searchable archive covering
transcripts, activity, cost, quality, and recall. The archive stays on your
machine unless you turn on a feature that shares it.

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

## Every session from every agent, in one archive

A background daemon watches the session directories your agents already write,
parses each format, and syncs everything into a local SQLite archive with
full-text indexes. Auto-discovered, nothing to configure. Supported harnesses
include Claude Code, OpenClaude, Codex, Gemini, Copilot (CLI, VS Code, and
Visual Studio), Cursor, Cursor IDE, IcodeMate, Qwen Code, DeepSeek TUI and
Harness, Mistral Vibe, Zed, Warp, OpenCode, Positron, Posit Assistant, Claude
Cowork, Aider, Antigravity, gptme, Kilo, Kimi, Kiro, OpenHands, Goose, Grok,
RooCode, Trae, Windsurf, and dozens more. Every supported source is listed in
[session discovery](/docs/configuration/#session-discovery).

- **60+** agent formats parsed
- **1** binary, zero accounts
- **SQLite** archive of record

## See when your agents are actually working

The [Activity dashboard](/docs/activity/) shows peak concurrency and the exact
moment it happened, active versus idle time, agent-minutes across parallel
sessions, and cost. Scope it to any day, week, month, or custom range, and
filter by project, agent, and machine. Live sync streams new messages into the
UI as sessions run.

## Know what every agent costs

[Token and cost reports](/docs/token-usage/) read from the pre-indexed archive
instead of reparsing raw session files every time. Pricing tracks LiteLLM and
OpenRouter rates with an offline fallback, and cache-aware accounting covers
prompt-cache creation and reads.

```bash
agentsview usage daily          # last 30 days, terminal table
agentsview usage statusline     # $9.61 today
agentsview capture run -- claude -p "fix the tests"
```

## Search and score every transcript

Full-text search covers every message across every agent. Opt-in
[semantic and hybrid search](/docs/semantic-search/) match by meaning when you
don't remember the exact words, and every match cites the conversation unit it
came from. [Session intelligence](/docs/session-intelligence/) adds health
scores, outcome classification, and deterministic
[quality signals](/docs/quality/) with evidence links back to the source
transcript.

## Turn transcripts into durable knowledge

[Recall](/docs/recall/) (experimental) extracts provenance-linked knowledge from
your archive: decisions, gotchas, and project facts, each with evidence links
back to the sessions that produced it. Generated Insights write model-authored
reports over an explicit session scope.

## Your agents can read it too

The same archive you browse is available to your agents:

- **CLI:** scriptable reports and session queries.
- **REST:** programmatic [session and usage access](/docs/session-api/).
- **MCP:** [session history as assistant tools](/docs/mcp/).
- **SSE:** live message streams as sessions run.
- **Web:** embedded Svelte UI served from the binary.
- **Desktop:** native app sharing the same data directory.

An agent can check what a previous session already tried, quote its own history,
or watch its spend mid-run.

## One machine or the whole team

SQLite is the archive of record. From there:

- [PostgreSQL sync](/docs/pg-sync/) pushes each machine's archive to a shared
  team backend with per-machine labels and a read-only merged server.
- [DuckDB mirror](/docs/duckdb/) serves analytical reads locally or over the
  Quack protocol.
- [Filesystem sync](/docs/filesystem-sync/) and
  [artifact folder sync](/docs/artifact-sync/) move sessions between machines
  without any database server.
- [Hosted raw sync](/docs/hosted-raw-sync/) keeps original provider files in
  hosted custody with device authentication, resumable uploads, and durable
  checkpoints.
- [Remote access](/docs/remote-access/) stays loopback-only by default, with
  explicit flags for SSH forwards and authenticated exposure.

## Local by default. Shared when you choose

Your agent transcripts are some of the most sensitive data on your machine.
AgentsView starts with one local SQLite archive and a loopback-only server. Data
leaves the machine only when you choose a feature such as PostgreSQL sync,
remote DuckDB access, Generated Insights, GitHub publishing, or hosted raw sync.
Each feature documents what it sends and where it goes.

## Start

Install AgentsView and it finds the sessions that are already on your machine.

- [Follow the guide](/guide/)
- [Run the quickstart](/docs/quickstart/)
- [Read the docs](/docs/)
