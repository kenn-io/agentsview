---
title: AgentsView Documentation
description: Find the guide for browsing, searching, reporting on, and maintaining your agent session archive
---

# AgentsView Documentation

AgentsView lets you browse, search, and track costs across your AI coding
sessions. A background server imports recorded sessions into a SQLite database
on your machine. The web interface, desktop app, command line, and APIs read
that archive.

New here? The [product overview](/) explains what AgentsView is for, and the
[five-minute guide](/guide/) walks the whole loop with screenshots. The pages
here explain how to use and maintain each feature.

These guides follow `main` and may include changes newer than the latest
release, **v0.44.0**. Check the [changelog](/docs/changelog/) for what each
release includes.

<p class="hero-actions">
  <a class="md-button md-button--primary" href="/docs/quickstart/">Quick Start</a>
  <a class="md-button" href="https://github.com/kenn-io/agentsview">View on GitHub</a>
</p>

## Start here

| If you want to…                                   | Read…                                                                                                                                                                                                      |
| ------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Install and see your sessions in under a minute   | [Quick Start](/docs/quickstart/)                                                                                                                                                                           |
| Learn the web interface                           | [Usage Guide](/docs/usage/)                                                                                                                                                                                |
| See when agents ran, overlapped, and what it cost | [Activity](/docs/activity/)                                                                                                                                                                                |
| Get daily token and cost reports                  | [Token Usage & Costs](/docs/token-usage/)                                                                                                                                                                  |
| Search transcripts by meaning, not just words     | [Semantic Search](/docs/semantic-search/)                                                                                                                                                                  |
| Correct project names and worktree assignments | [Data](/docs/data/) |
| Score session health and outcomes                 | [Session Intelligence](/docs/session-intelligence/)                                                                                                                                                        |
| Browse extracted, provenance-linked knowledge     | [Recall](/docs/recall/)                                                                                                                                                                                    |
| Give agents and scripts access to the archive     | [MCP Server](/docs/mcp/) and [Session API](/docs/session-api/)                                                                                                                                             |
| Share sessions across machines                    | [Hosted Raw Sync](/docs/hosted-raw-sync/), [PostgreSQL Sync](/docs/pg-sync/), [ClickHouse Sync](/docs/clickhouse-sync/), [DuckDB Mirror](/docs/duckdb/), [Filesystem Session Sync](/docs/filesystem-sync/) |
| Configure discovery, paths, and settings          | [Configuration](/docs/configuration/)                                                                                                                                                                      |
| Look up a command or flag                         | [CLI Reference](/docs/commands/)                                                                                                                                                                           |

## How the archive fits together

**The daemon imports your sessions.** This background server owns the writable
archive and watches your agents' directories for new messages. The desktop app
and commands that need the server start it when required. Some diagnostic
commands can read the archive directly. See
[`agentsview daemon`](/docs/commands/#agentsview-daemon).

**SQLite keeps your session history.** Parsed sessions live in `~/.agentsview/`
with full-text indexes. Optional backends extend it:
[PostgreSQL](/docs/pg-sync/) or [ClickHouse](/docs/clickhouse-sync/) for a
remote read-only copy and [DuckDB](/docs/duckdb/) for analytical reads. All
three are mirrors pushed from SQLite, never the source of truth.

<img src="/docs/assets/static/architecture.svg" alt="AgentsView architecture: agent sessions sync into SQLite with FTS5 search, served via REST API, SSE events, and embedded Svelte SPA" style="width: 100%; max-width: 960px; margin: 1.5rem auto; display: block;" />

Use [Configuration](/docs/configuration/) to choose source directories and how
much content to retain. Use [Data](/docs/data/#storage-maintenance) to manage
archive size.

## Privacy

Session data stays on your machine by default. The server binds to `127.0.0.1`
unless you explicitly configure [remote access](/docs/remote-access/). Data
leaves the machine only for features you choose, such as hosted raw sync, a
PostgreSQL or ClickHouse target, remote DuckDB access, Generated Insights, or
publishing a session to GitHub. An anonymous, content-free daemon liveness ping
is the only telemetry, and `AGENTSVIEW_TELEMETRY_ENABLED=0` disables it.

## Human and machine-readable pages

Every page has an HTML URL and a Markdown twin. For example:

- `https://agentsview.io/docs/token-usage/`
- `https://agentsview.io/docs/token-usage.md`

The complete machine-readable index is available at
[`https://agentsview.io/llms.txt`](https://agentsview.io/llms.txt).
