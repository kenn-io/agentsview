# agentsview

A local-first desktop and web application for browsing, searching,
and analyzing AI agent coding sessions. Supports Claude Code, Codex,
OpenCode, and 8 other agents.

<p align="center">
  <img src="https://agentsview.io/screenshots/dashboard.png" alt="Analytics dashboard" width="720">
</p>

## Desktop App

Download the desktop installer for macOS or Windows from
[GitHub Releases](https://github.com/wesm/agentsview/releases).
The desktop app includes auto-updates and runs the server as a
local sidecar -- no terminal required.

## CLI Install

```bash
curl -fsSL https://agentsview.io/install.sh | bash
```

**Windows:**

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://agentsview.io/install.ps1 | iex"
```

The CLI installer downloads the latest release, verifies the
SHA-256 checksum, and installs the binary.

**Build from source** (requires Go 1.25+ with CGO and Node.js 22+):

```bash
git clone https://github.com/wesm/agentsview.git
cd agentsview
make build
make install  # installs to ~/.local/bin
```

## Why?

AI coding agents generate large volumes of session data across
projects. agentsview indexes these sessions into a local SQLite
database with full-text search, providing a web interface to
find past conversations, review agent behavior, and track usage
patterns over time.

## Features

- **Full-text search** across all message content, instantly
- **Analytics dashboard** with activity heatmaps, tool usage,
  velocity metrics, and project breakdowns
- **Multi-agent support** for Claude Code, Codex, OpenCode, and
  8 other agents ([full list](#supported-agents))
- **Live updates** via SSE as active sessions receive new messages
- **Keyboard-first** navigation (vim-style `j`/`k`/`[`/`]`)
- **Export and publish** sessions as HTML or to GitHub Gist
- **Local-first** -- all data stays on your machine, single binary,
  no accounts

## Usage

```bash
agentsview              # start server, open browser
agentsview -port 9090   # custom port
agentsview -no-browser  # headless mode
```

On startup, agentsview discovers sessions from all supported
agents, syncs them into a local SQLite database with FTS5
full-text search, and opens a web UI at `http://127.0.0.1:8080`.

For hostname or reverse-proxy access, explicitly allow the browser
origin you expect. This preserves the default DNS-rebinding and CSRF
protections while enabling remote or TLS-terminated access.

```bash
# Direct HTTP on a custom hostname/port
agentsview -host 0.0.0.0 -port 8004 \
  -public-origin http://viewer.example.test:8004

# HTTPS behind a reverse proxy such as Caddy
agentsview -host 127.0.0.1 -port 8004 \
  -public-origin https://viewer.example.test
```

You can also persist the same setting in `~/.agentsview/config.json`:

```json
{
  "public_origins": [
    "https://viewer.example.test",
    "http://viewer.example.test:8004"
  ]
}
```

## Screenshots

| Dashboard | Session viewer |
|-----------|---------------|
| ![Dashboard](https://agentsview.io/screenshots/dashboard.png) | ![Session viewer](https://agentsview.io/screenshots/message-viewer.png) |

| Search | Activity heatmap |
|--------|-----------------|
| ![Search](https://agentsview.io/screenshots/search-results.png) | ![Heatmap](https://agentsview.io/screenshots/heatmap.png) |

## Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `Cmd+K` | Open search |
| `j` / `k` | Next / previous message |
| `]` / `[` | Next / previous session |
| `o` | Toggle sort order |
| `t` | Toggle thinking blocks |
| `e` | Export session as HTML |
| `p` | Publish to GitHub Gist |
| `r` | Sync sessions |
| `?` | Show all shortcuts |

## Documentation

Full documentation is available at
[agentsview.io](https://agentsview.io):

- [Quick Start](https://agentsview.io/quickstart/) --
  installation and first run
- [Usage Guide](https://agentsview.io/usage/) --
  dashboard, session browser, search, export
- [CLI Reference](https://agentsview.io/commands/) --
  commands, flags, and environment variables
- [Configuration](https://agentsview.io/configuration/) --
  data directory, config file, session discovery
- [Architecture](https://agentsview.io/architecture/) --
  how the sync engine, parsers, and server work

## Development

```bash
make dev            # run Go server in dev mode
make frontend-dev   # run Vite dev server (use alongside make dev)
make desktop-dev    # run Tauri desktop app in dev mode
make test           # Go tests (CGO_ENABLED=1 -tags fts5)
make lint           # golangci-lint
make e2e            # Playwright E2E tests
```

## Desktop Development

The desktop app is a Tauri wrapper under `desktop/`. It launches
the `agentsview` Go binary as a local sidecar and loads
`http://127.0.0.1:<port>` in a native webview.

```bash
make desktop-dev                 # run desktop app in dev mode
make desktop-build               # build desktop bundles (.app/.exe)
make desktop-macos-app           # build macOS .app only
make desktop-windows-installer   # build Windows installer (.exe)
```

Desktop env escape hatch: `~/.agentsview/desktop.env` (for
PATH/API keys overrides).

### Project Structure

```
cmd/agentsview/     CLI entrypoint
internal/config/    Configuration loading
internal/db/        SQLite operations (sessions, search, analytics)
internal/parser/    Session parsers (all supported agents)
internal/server/    HTTP handlers, SSE, middleware
internal/sync/      Sync engine, file watcher, discovery
frontend/           Svelte 5 SPA (Vite, TypeScript)
```

## Supported Agents

| Agent | Session Directory | Env Override |
|-------|-------------------|--------------|
| Claude Code | `~/.claude/projects/` | `CLAUDE_PROJECTS_DIR` |
| Codex | `~/.codex/sessions/` | `CODEX_SESSIONS_DIR` |
| Copilot | `~/.copilot/` | `COPILOT_DIR` |
| Gemini | `~/.gemini/` | `GEMINI_DIR` |
| OpenCode | `~/.local/share/opencode/` | `OPENCODE_DIR` |
| Cursor | `~/.cursor/projects/` | `CURSOR_PROJECTS_DIR` |
| Amp | `~/.local/share/amp/threads/` | `AMP_DIR` |
| iFlow | `~/.iflow/projects/` | `IFLOW_DIR` |
| VSCode Copilot | `~/Library/Application Support/Code/User/` (macOS) | `VSCODE_COPILOT_DIR` |
| Pi | `~/.pi/agent/sessions/` | `PI_DIR` |
| OpenClaw | `~/.openclaw/agents/` | `OPENCLAW_DIR` |

## Acknowledgements

Inspired by
[claude-history-tool](https://github.com/andyfischer/ai-coding-tools/tree/main/claude-history-tool)
by Andy Fischer and
[claude-code-transcripts](https://github.com/simonw/claude-code-transcripts)
by Simon Willison.

## License

MIT
