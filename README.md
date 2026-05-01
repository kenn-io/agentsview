# agentsview

Browse, search, and track costs across all your AI coding agents. One binary, no
accounts, everything local.

<p align="center">
  <img src="https://agentsview.io/screenshots/dashboard.png" alt="Analytics dashboard" width="720">
</p>

## Install

```bash
# macOS / Linux
curl -fsSL https://agentsview.io/install.sh | bash

# Windows
powershell -ExecutionPolicy ByPass -c "irm https://agentsview.io/install.ps1 | iex"
```

Or download the **desktop app** (macOS / Windows) from
[GitHub Releases](https://github.com/wesm/agentsview/releases).

## Quick Start

```bash
agentsview serve           # start server, open web UI
agentsview usage daily     # print daily cost summary
```

On first run, agentsview discovers sessions from every supported agent on your
machine, syncs them into a local SQLite database, and opens a web UI at
`http://127.0.0.1:8080`.

## Token Usage and Cost Tracking

`agentsview usage` is a fast, local replacement for ccusage and similar tools.
It tracks token consumption and compute costs across **all** your coding agents
-- not just Claude Code. Because session data is already indexed in SQLite,
queries are over 100x faster than tools that re-parse raw session files on every
run.

```bash
# Daily cost summary (default: last 30 days)
agentsview usage daily

# Per-model breakdown
agentsview usage daily --breakdown

# Filter by agent and date range
agentsview usage daily --agent claude --since 2026-04-01

# One-line summary for shell prompts / status bars
agentsview usage daily --all --json
agentsview usage statusline
```

Features:

- Automatic pricing via LiteLLM rates (with offline fallback)
- Prompt-caching-aware cost calculation (cache creation / read tokens)
- Per-model breakdown with `--breakdown`
- Date filtering (`--since`, `--until`, `--all`), agent filtering (`--agent`)
- JSON output (`--json`) for scripting
- Timezone-aware date bucketing (`--timezone`)
- Works standalone -- no server required, just run the command

## Session Stats

`agentsview stats` emits window-scoped analytics over recorded sessions:
totals, archetypes (automation vs. quick/standard/deep/marathon), distributions
for session duration, user-message count, peak context, and tools-per-turn, plus
cache economics, tool/model/agent mix, and a temporal hourly breakdown. The
`--format json` output follows a versioned v1 schema (`schema_version: 1`)
suitable for downstream consumers.

```bash
# Human-readable summary over the last 28 days
agentsview stats

# Machine-readable JSON over a fixed date range
agentsview stats --format json --since 2026-04-01 --until 2026-04-15

# Restrict to one agent and inspect the schema
agentsview stats --format json --agent claude | jq '.schema_version'
```

## Session Browser

| Dashboard                                                     | Session viewer                                                          |
| ------------------------------------------------------------- | ----------------------------------------------------------------------- |
| ![Dashboard](https://agentsview.io/screenshots/dashboard.png) | ![Session viewer](https://agentsview.io/screenshots/message-viewer.png) |

| Search                                                          | Activity heatmap                                          |
| --------------------------------------------------------------- | --------------------------------------------------------- |
| ![Search](https://agentsview.io/screenshots/search-results.png) | ![Heatmap](https://agentsview.io/screenshots/heatmap.png) |

- **Full-text search** across all message content (FTS5)
- **Token usage and cost dashboard** -- per-session and per-model cost
  breakdowns, daily spend charts, all in the web UI
- **Analytics dashboard** -- activity heatmaps, tool usage, velocity metrics,
  project breakdowns
- **Live updates** via SSE as active sessions receive new messages
- **Keyboard-first** navigation (`j`/`k`/`[`/`]`, `Cmd+K` search, `?` for all
  shortcuts)
- **Export** sessions as HTML or publish to GitHub Gist

## Supported Agents

agentsview auto-discovers sessions from all of these:

| Agent          | Session Directory                                  |
| -------------- | -------------------------------------------------- |
| Claude Code    | `~/.claude/projects/`                              |
| Codex          | `~/.codex/sessions/`                               |
| Copilot CLI    | `~/.copilot/`                                      |
| Gemini CLI     | `~/.gemini/`                                       |
| OpenCode       | `~/.local/share/opencode/`                         |
| OpenHands CLI  | `~/.openhands/conversations/`                      |
| Cursor         | `~/.cursor/projects/`                              |
| Amp            | `~/.local/share/amp/threads/`                      |
| iFlow          | `~/.iflow/projects/`                               |
| VSCode Copilot | `~/Library/Application Support/Code/User/` (macOS) |
| Pi             | `~/.pi/agent/sessions/`                            |
| OpenClaw       | `~/.openclaw/agents/`                              |
| Kimi           | `~/.kimi/sessions/`                                |
| Kiro CLI       | `~/.kiro/sessions/cli/`                            |
| Kiro IDE       | `~/Library/Application Support/Kiro/` (macOS)      |
| Cortex Code    | `~/.snowflake/cortex/conversations/`               |

Each directory can be overridden with an environment variable. See the
[configuration docs](https://agentsview.io/configuration/) for details.

## PostgreSQL Sync

Push session data to a shared PostgreSQL instance for team dashboards:

```bash
agentsview pg push       # push local data to PG
agentsview pg serve      # serve web UI from PG (read-only)
```

See [PostgreSQL docs](https://agentsview.io/postgresql/) for setup and
configuration.

## Privacy

No telemetry, no analytics, no accounts. All data stays on your machine. The
server binds to `127.0.0.1` by default. The only outbound request is an optional
update check on startup (disable with `--no-update-check`).

## Documentation

Full docs at **[agentsview.io](https://agentsview.io)**:
[Quick Start](https://agentsview.io/quickstart/) --
[Usage Guide](https://agentsview.io/usage/) --
[CLI Reference](https://agentsview.io/commands/) --
[Configuration](https://agentsview.io/configuration/) --
[Architecture](https://agentsview.io/architecture/)

______________________________________________________________________

## Development

Requires Go 1.26+ (CGO), Node.js 22+.

```bash
make dev            # Go server (dev mode)
make frontend-dev   # Vite dev server (run alongside make dev)
make build          # build binary with embedded frontend
make install        # install to ~/.local/bin
```

```bash
make test           # Go tests (CGO_ENABLED=1 -tags fts5)
make lint           # golangci-lint + NilAway
make nilaway        # NilAway through custom golangci-lint
make e2e            # Playwright E2E tests
```

Pre-commit hooks via [prek](https://github.com/j178/prek): run `make lint-tools`
and `make install-hooks` after cloning (requires `prek` and `uv`).

### Project Layout

```
cmd/agentsview/     CLI entrypoint
internal/           Go packages (config, db, parser, server, sync, postgres)
frontend/           Svelte 5 SPA (Vite, TypeScript)
desktop/            Tauri desktop wrapper
```

## Acknowledgements

Inspired by
[claude-history-tool](https://github.com/andyfischer/ai-coding-tools/tree/main/claude-history-tool)
by Andy Fischer and
[claude-code-transcripts](https://github.com/simonw/claude-code-transcripts) by
Simon Willison.

## License

MIT
