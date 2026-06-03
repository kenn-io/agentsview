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
[GitHub Releases](https://github.com/kenn-io/agentsview/releases) or via
homebrew: `brew install --cask agentsview`

Or run the published Docker image:

```bash
docker run --rm -p 127.0.0.1:8080:8080 \
  -v agentsview-data:/data \
  -v "$HOME/.claude/projects:/agents/claude:ro" \
  -v "$HOME/.forge:/agents/forge:ro" \
  -e CLAUDE_PROJECTS_DIR=/agents/claude \
  -e FORGE_DIR=/agents/forge \
  ghcr.io/kenn-io/agentsview:latest
```

## Quick Start

```bash
agentsview serve           # start server, open web UI
agentsview usage daily     # print daily cost summary
```

On first run, agentsview discovers sessions from every supported agent on your
machine, syncs them into a local SQLite database, and opens a web UI at
`http://127.0.0.1:8080`.

## Remote / forwarded access

agentsview binds to loopback and validates the request `Host` header to guard
against DNS-rebinding attacks. When you reach it through SSH port-forwarding, a
reverse proxy, or a remote dev environment (exe.dev, Codespaces, Coder, WSL2),
the browser sends a `Host` that the server does not recognize, so API requests
such as `/api/v1/settings` are rejected with `403 Forbidden`.

To fix this, restart the server with `--public-url` set to the exact origin you
open in the browser:

```bash
# Browser opens http://127.0.0.1:18080 via `ssh -L 18080:127.0.0.1:8080 host`
agentsview serve --public-url http://127.0.0.1:18080

# Browser opens a forwarded hostname
agentsview serve --public-url https://your-workspace.exe.dev
```

Use `--public-origin` (repeatable or comma-separated) to trust additional
browser origins. If you expose the UI beyond loopback, also enable
`--require-auth`.

## Docker

The container image defaults to local `agentsview serve`. Set `PG_SERVE=1` to
switch the startup command to `agentsview pg serve` instead.

`docker-compose.prod.yaml` is included as a production example:

```bash
docker compose -f docker-compose.prod.yaml up -d
```

The included compose file persists the agentsview data directory in a named
volume and mounts Claude, Codex, Forge, and OpenCode session roots read-only.
The container runs as root, so prefer a named volume for `/data` over a host
bind mount; if you do bind-mount, pre-create the directory with the desired
ownership to avoid root-owned files in your home directory.

The examples publish the UI on loopback only (`127.0.0.1`). If you need to
expose it beyond localhost, enable `--require-auth` and publish the port
intentionally.

Important: a containerized agentsview instance can only discover agent sessions
from directories you explicitly mount into the container. If you do not mount an
agent's session directory and point the matching env var at it, that agent will
not appear in the UI.

Example PostgreSQL-backed startup:

```bash
docker run --rm -p 127.0.0.1:8080:8080 \
  -e PG_SERVE=1 \
  -e AGENTSVIEW_PG_URL='postgres://user:password@postgres.example.com:5432/agentsview?sslmode=require' \
  ghcr.io/kenn-io/agentsview:latest
```

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

## Per-Session Details

`agentsview session usage <id>` prints per-session token statistics plus a cost
estimate for a single session. The output reports the session's total output
tokens and peak context tokens, plus a cost estimate in USD (`cost_usd`) when
pricing is available for the session's model(s) (`has_cost`). Cost is computed
from input/output and cache tokens internally, but only the output-token and
peak-context totals are reported alongside the cost.

```bash
# Print token usage and cost for a specific session
agentsview session usage <id>

# JSON output for scripting
agentsview session usage <id> --format json
```

The same per-session usage data is available from the REST API:

```bash
GET /api/v1/sessions/{id}/usage
```

The response includes the `session_id`, `agent`, `project`,
`total_output_tokens`, `peak_context_tokens`, `has_token_data`, `cost_usd`,
`has_cost`, `models`, and `unpriced_models` fields from the CLI JSON schema.
HTTP responses also include `server_running: true`. Existing sessions return
`200` even when token or cost data is absent; missing sessions return `404`.

The deprecated alias `agentsview token-use <id>` remains available for
compatibility and now also reports cost estimates.

## Session Stats

`agentsview stats` emits window-scoped analytics over recorded sessions: totals,
archetypes (automation vs. quick/standard/deep/marathon), distributions for
session duration, user-message count, peak context, and tools-per-turn, plus
cache economics, tool/model/agent mix, and a temporal hourly breakdown. The
`--format json` output follows a versioned v1 schema (`schema_version: 1`)
suitable for downstream consumers.

By default, `stats` only reads the local SQLite archive. Git-derived outcome
metrics are opt-in because they can be slow or brittle on large/missing repos:
use `--include-git-outcomes` for commits/LOC/files changed, and
`--include-github-outcomes` for GitHub PR counts via `gh` (this also enables git
outcomes).

```bash
# Human-readable summary over the last 28 days
agentsview stats

# Machine-readable JSON over a fixed date range
agentsview stats --format json --since 2026-04-01 --until 2026-04-15

# Restrict to one agent and inspect the schema
agentsview stats --format json --agent claude | jq '.schema_version'

# Include expensive local git outcome metrics explicitly
agentsview stats --include-git-outcomes
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

| Agent              | Session Directory                                      |
| ------------------ | ------------------------------------------------------ |
| Claude Code        | `~/.claude/projects/`                                  |
| Codex              | `~/.codex/sessions/`                                   |
| Copilot CLI        | `~/.copilot/`                                          |
| Gemini CLI         | `~/.gemini/`                                           |
| OpenCode           | `~/.local/share/opencode/`                             |
| OpenHands CLI      | `~/.openhands/conversations/`                          |
| Cursor             | `~/.cursor/projects/`                                  |
| Amp                | `~/.local/share/amp/threads/`                          |
| iFlow              | `~/.iflow/projects/`                                   |
| Zencoder           | `~/.zencoder/sessions/`                                |
| VSCode Copilot     | `~/Library/Application Support/Code/User/` (macOS)     |
| Pi                 | `~/.pi/agent/sessions/`                                |
| Qwen Code          | `~/.qwen/projects/`                                    |
| OpenClaw           | `~/.openclaw/agents/`                                  |
| QClaw              | `~/.qclaw/agents/`                                     |
| Kimi               | `~/.kimi/sessions/`                                    |
| Kiro CLI           | `~/.kiro/sessions/cli/`, `~/.local/share/kiro-cli/`    |
| Kiro IDE           | `~/Library/Application Support/Kiro/` (macOS)          |
| Cortex Code        | `~/.snowflake/cortex/conversations/`                   |
| Hermes Agent       | `~/.hermes/sessions/`                                  |
| WorkBuddy          | `~/.workbuddy/projects/`                               |
| Forge              | `~/.forge/`                                            |
| Piebald            | `~/.local/share/piebald/`                              |
| Warp               | `~/.warp/` (platform-dependent)                        |
| Positron Assistant | `~/Library/Application Support/Positron/User/` (macOS) |
| Antigravity        | `~/.gemini/antigravity/`                               |
| Antigravity CLI    | `~/.gemini/antigravity-cli/` (see note below)          |

Each directory can be overridden with an environment variable. See the
[configuration docs](https://agentsview.io/configuration/) for details.

### Antigravity CLI: high-resolution transcripts

Antigravity CLI sessions now appear in two on-disk formats. Newer releases
store conversation trajectories as SQLite `.db` files, which agentsview indexes
directly. Older releases stored assistant turns and tool calls in
AES-GCM-encrypted `.pb` files; for those sessions, agentsview falls back to
**summary mode** using your prompts from `history.jsonl` plus any plain-text
artifacts under `brain/` (plans, walkthroughs, checkpoints).

To unlock full transcripts for older `.pb` sessions, run
[agy-reader](https://github.com/mjacobs/agy-reader) alongside agentsview.
agy-reader talks to the local Antigravity daemon, decrypts each conversation,
and writes a `<uuid>.trajectory.json` sidecar next to the encrypted `.pb` file.
agentsview's file watcher detects the sidecar automatically and parses it in
place of summary mode -- no agentsview restart needed.

```bash
go install github.com/mjacobs/agy-reader@latest

# Generate sidecars for existing sessions...
agy-reader --sync

# ...or keep them fresh as you work.
agy-reader --watch
```

agy-reader auto-discovers the Antigravity daemon URL by parsing
`~/.gemini/antigravity-cli/cli.log`. If discovery fails (e.g. the log has
rotated), the command prints platform-specific instructions for locating the
port and exporting `ANTIGRAVITY_DAEMON_URL` manually.

Sidecars stay on your machine. agentsview makes no outbound request to produce
or read them, and treats sidecars as untrusted structured input -- see
[SECURITY.md](SECURITY.md) for the trust model.

## PostgreSQL Sync

Push session data to a shared PostgreSQL instance for team dashboards:

```bash
agentsview pg push       # push local data to PG
agentsview pg serve      # serve web UI from PG (read-only)
```

### Automatic push (background service)

To keep a shared PostgreSQL database current without running `pg push` by
hand, run the auto-push daemon. It watches your session directories and
pushes shortly after new sessions are recorded, with a periodic floor as a
safety net:

```bash
agentsview pg push --watch                 # foreground, Ctrl-C to stop
agentsview pg push --watch --debounce 1m   # custom coalesce window
agentsview pg push --watch --interval 5m   # custom floor interval
```

The daemon reads the same `[pg]` config as `pg push`, so the PostgreSQL
DSN must be set in your config file (or an environment variable it
expands). Protect the config file, since it holds credentials:

```bash
chmod 600 ~/.agentsview/config.toml
```

To run it unattended as an OS service (launchd on macOS,
`systemd --user` on Linux):

```bash
agentsview pg service install     # generate the unit, enable + start it
agentsview pg service status      # show manager status
agentsview pg service logs -f     # follow the service log
agentsview pg service uninstall   # stop and remove
```

**Linux headless machines:** systemd `--user` services stop at logout and
do not start at boot unless lingering is enabled for your user. `install`
detects this and prints the command; you can also run it yourself:

```bash
loginctl enable-linger "$USER"
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
