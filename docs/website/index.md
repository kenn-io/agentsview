# The system of record for your AI coding agents

AgentsView is a local-first daemon that captures every session from more than 50
coding agents into one searchable archive. Browse transcripts, monitor activity,
track cost, measure quality, and feed what you learn back to your agents.
Session data stays on your machine.

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

## Every agent. Every session. One archive.

A background daemon watches the session directories your agents already write,
parses each format, and syncs everything into a local SQLite archive with
full-text indexes. Auto-discovered, nothing to configure. Supported harnesses
include Claude Code, OpenClaude, Codex, Gemini, Copilot (CLI, VS Code, and
Visual Studio), Cursor, Qwen Code, DeepSeek TUI and Harness, Mistral Vibe, Zed,
Warp, OpenCode, Positron, Posit Assistant, Claude Cowork, Aider, Antigravity,
gptme, Kilo, Kimi, Kiro, OpenHands, Goose, Grok, RooCode, Trae, Windsurf, and
dozens more. All 56 harnesses are listed in
[session discovery](/docs/configuration/#session-discovery).

- **56** agent harnesses parsed
- **1** binary, zero accounts
- **80–220×** faster than ccusage on large archives

## See when your agents are actually working

The [Activity dashboard](/docs/activity/) turns timestamped session data into an
operational picture: peak concurrency and the exact moment it happened, active
versus idle time, agent-minutes across parallel sessions, and cost, scoped to
any day, week, month, or custom range and filterable by project, agent, and
machine. Live sync streams new messages into the UI as sessions run.

## Know what every agent costs

[Token and cost reports](/docs/token-usage/) read from the pre-indexed archive,
so they return in well under a second even on histories with tens of thousands
of sessions: 80–220× faster than `npx ccusage` on a 22,000-session database.
Pricing tracks LiteLLM and OpenRouter rates with an offline fallback, and
cache-aware accounting covers prompt-cache creation and reads.

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
- [Remote access](/docs/remote-access/) stays loopback-only by default, with
  explicit flags for SSH forwards and authenticated exposure.

## Not a hosted analytics product

Your agent transcripts are some of the most sensitive data on your machine.
AgentsView stores and serves everything locally, has no accounts, and binds to
loopback unless you say otherwise. Session content never leaves your machine
unless you configure a sync target you control.

## Start

Install AgentsView and it finds the sessions that are already on your machine.

- [Follow the guide](/guide/)
- [Run the quickstart](/docs/quickstart/)
- [Read the docs](/docs/)
