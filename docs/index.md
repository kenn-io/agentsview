---
title: AgentsView Documentation
description: Documentation map and mental model for AgentsView, the local-first system of record for AI coding agent sessions
---

# AgentsView Documentation

AgentsView is a local-first system of record for AI coding agent sessions. A
background daemon syncs sessions from more than 50 agent harnesses into a
SQLite archive on your machine, and serves them through a web UI, desktop app,
CLI, REST API, and MCP server.

New here? The [product overview](/) explains what AgentsView is for, and the
[five-minute guide](/guide/) walks the whole loop with screenshots. This
documentation is the comprehensive, task-oriented reference.

<p class="hero-actions">
  <a class="md-button md-button--primary" href="/docs/quickstart/">Quick Start</a>
  <a class="md-button" href="https://github.com/kenn-io/agentsview">View on GitHub</a>
</p>

## Start here

| If you want to… | Read… |
| --- | --- |
| Install and see your sessions in under a minute | [Quick Start](/docs/quickstart/) |
| Learn the web interface | [Usage Guide](/docs/usage/) |
| See when agents ran, overlapped, and what it cost | [Activity](/docs/activity/) |
| Get daily token and cost reports | [Token Usage & Costs](/docs/token-usage/) |
| Search transcripts by meaning, not just words | [Semantic Search](/docs/semantic-search/) |
| Score session health and outcomes | [Session Intelligence](/docs/session-intelligence/) |
| Browse extracted, provenance-linked knowledge | [Recall](/docs/recall/) |
| Give agents and scripts access to the archive | [MCP Server](/docs/mcp/) and [Session API](/docs/session-api/) |
| Share sessions across machines or a team | [PostgreSQL Sync](/docs/pg-sync/), [DuckDB Mirror](/docs/duckdb/), [Filesystem Session Sync](/docs/filesystem-sync/) |
| Configure discovery, paths, and settings | [Configuration](/docs/configuration/) |
| Look up a command or flag | [CLI Reference](/docs/commands/) |

## The mental model

**The daemon owns the archive.** The desktop app and freshness-sensitive CLI
commands share one detached local daemon that holds the writable SQLite
connection, watches your agents' session directories, and syncs new messages
as they are written. Read-only commands attach to a warm daemon or fall back
to a direct read-only view, so one-off scripts stay fast. See
[`agentsview daemon`](/docs/commands/#agentsview-daemon).

**SQLite is the system of record.** Every parsed session lives in
`~/.agentsview/` with full-text indexes. Optional backends extend it:
[PostgreSQL](/docs/pg-sync/) for a shared team view and
[DuckDB](/docs/duckdb/) for analytical reads. Both are mirrors pushed from
SQLite, never the source of truth.

**Everything is an interface over the same archive.** The web UI, desktop
app, CLI reports, REST endpoints, SSE streams, and MCP tools all read the same
data, so people and agents see the same history.

## How it works

<img src="/docs/assets/static/architecture.svg" alt="AgentsView architecture: agent sessions sync into SQLite with FTS5 search, served via REST API, SSE events, and embedded Svelte SPA" style="width: 100%; max-width: 960px; margin: 1.5rem auto; display: block;" />

AgentsView watches your agent session directories for changes, parses each
agent's format, and stores structured data in SQLite with full-text search
indexes. The embedded web frontend provides browsing, search, and analytics
over the REST API.

## Privacy

Session data stays on your machine. There are no accounts and no cloud
services; the server binds to `127.0.0.1` unless you explicitly configure
[remote access](/docs/remote-access/). Sharing beyond the machine happens only
through sync targets you configure and control. An anonymous, content-free
daemon liveness ping is the only telemetry, and
`AGENTSVIEW_TELEMETRY_ENABLED=0` disables it.

## Human and machine-readable pages

Every page has an HTML URL and a Markdown twin. For example:

- `https://agentsview.io/docs/token-usage/`
- `https://agentsview.io/docs/token-usage.md`

The complete machine-readable index is available at
[`https://agentsview.io/llms.txt`](https://agentsview.io/llms.txt).
