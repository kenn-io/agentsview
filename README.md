# agentsview

A local-first desktop and web application for browsing, searching, and analyzing
AI agent coding sessions. Supports Claude Code, Codex, OpenCode, and 11 other
agents.

<p align="center">
  <img src="https://agentsview.io/screenshots/dashboard.png" alt="Analytics dashboard" width="720">
</p>

## Desktop App

Download the desktop installer for macOS or Windows from
[GitHub Releases](https://github.com/wesm/agentsview/releases). The desktop app
includes auto-updates and runs the server as a local sidecar -- no terminal
required.

## CLI Install

```bash
curl -fsSL https://agentsview.io/install.sh | bash
```

**Windows:**

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://agentsview.io/install.ps1 | iex"
```

The CLI installer downloads the latest release, verifies the SHA-256 checksum,
and installs the binary.

**Build from source** (requires Go 1.25+ with CGO and Node.js 22+):

```bash
git clone https://github.com/wesm/agentsview.git
cd agentsview
make build
make install  # installs to ~/.local/bin
```

## Why?

AI coding agents generate large volumes of session data across projects.
agentsview indexes these sessions into a local SQLite database with full-text
search, providing a web interface to find past conversations, review agent
behavior, and track usage patterns over time.

## Features

- **Full-text search** across all message content, instantly
- **Analytics dashboard** with activity heatmaps, tool usage, velocity metrics,
  and project breakdowns
- **Multi-agent support** for Claude Code, Codex, OpenCode, and 11 other agents
  ([full list](#supported-agents))
- **Live updates** via SSE as active sessions receive new messages
- **Keyboard-first** navigation (vim-style `j`/`k`/`[`/`]`)
- **Export and publish** sessions as HTML or to GitHub Gist
- **Local-first** -- all data stays on your machine, single binary, no accounts

## Privacy and Telemetry

agentsview has **no telemetry and no analytics**. No usage data, crash reports,
or diagnostics are collected or sent anywhere.

- All session data stays on your machine in a local SQLite database
- The server binds to `127.0.0.1` by default and is not network-accessible
- No accounts, no sign-ups, no tracking
- The optional PostgreSQL sync is explicit and user-initiated (`pg push`),
  connecting only to a server you configure

The only automatic outbound requests are update checks on startup. The CLI/web
UI fetches release metadata from the GitHub API, while the desktop app uses its
own native updater to check for new releases. Neither check sends analytics or
session data. To disable:

- **Desktop app**: set `AGENTSVIEW_DESKTOP_AUTOUPDATE=0`
- **CLI/web UI**: set `AGENTSVIEW_DISABLE_UPDATE_CHECK=1`, pass
  `-no-update-check`, or set `disable_update_check = true` in
  `~/.agentsview/config.toml`

## Usage

```bash
agentsview              # start server
agentsview -port 9090   # custom port
```

On startup, agentsview discovers sessions from all supported agents, syncs them
into a local SQLite database with FTS5 full-text search, and opens a web UI at
`http://127.0.0.1:8080`.

For hostname or reverse-proxy access, set a `public_url`. This preserves the
default DNS-rebinding and CSRF protections while explicitly trusting the
external browser origin you expect.

```bash
# Direct HTTP on a custom hostname/port
agentsview -host 0.0.0.0 -port 8004 \
  -public-url http://viewer.example.test:8004

# HTTPS behind your own reverse proxy
agentsview -host 127.0.0.1 -port 8004 \
  -public-url https://viewer.example.test
```

agentsview can also manage a Caddy frontend for you. In managed-Caddy mode, keep
the backend on loopback and let Caddy terminate TLS and optionally restrict
client IP ranges. By default, managed Caddy binds to `127.0.0.1` and exposes the
public URL on port `8443`. To expose it on a non-loopback interface, set
`-proxy-bind-host` explicitly and provide at least one `-allowed-subnet`.

Managed Caddy mode requires the `caddy` CLI to already be installed. This patch
does not automate Caddy installation. Use your normal OS package manager or ask
your coding agent to install Caddy for your platform first. Caddy supports
Linux, macOS, and Windows.

For privileged ports such as `443` or `80`, prefer leaving `agentsview` itself
unprivileged and granting the Caddy binary permission to bind low ports. On
Linux, that typically means:

```bash
sudo setcap cap_net_bind_service=+ep "$(command -v caddy)"
```

Then run `agentsview` normally as your user with `-public-port 443` or
`-public-port 80`. This avoids running the session viewer as root, which would
otherwise change which home directory and agent session data it can see. If you
do not need a privileged port, the default `8443` is the simpler option.

```bash
agentsview -host 127.0.0.1 -port 8080 \
  -public-url https://viewer.example.test \
  -proxy caddy \
  -proxy-bind-host 0.0.0.0 \
  -public-port 8443 \
  -tls-cert ~/.certs/viewer.crt \
  -tls-key ~/.certs/viewer.key \
  -allowed-subnet 10.0/16 \
  -allowed-subnet 192.168.1.0/24
```

You can persist the same settings in `~/.agentsview/config.toml`:

```toml
public_url = "https://viewer.example.test"

[proxy]
mode = "caddy"
bind_host = "0.0.0.0"
public_port = 8443
tls_cert = "/home/user/.certs/viewer.crt"
tls_key = "/home/user/.certs/viewer.key"
allowed_subnets = ["10.0/16", "192.168.1.0/24"]
```

`public_origins` remains available as an advanced override when you need to
allow additional browser origins beyond the main `public_url`.

## Screenshots

| Dashboard                                                     | Session viewer                                                          |
| ------------------------------------------------------------- | ----------------------------------------------------------------------- |
| ![Dashboard](https://agentsview.io/screenshots/dashboard.png) | ![Session viewer](https://agentsview.io/screenshots/message-viewer.png) |

| Search                                                          | Activity heatmap                                          |
| --------------------------------------------------------------- | --------------------------------------------------------- |
| ![Search](https://agentsview.io/screenshots/search-results.png) | ![Heatmap](https://agentsview.io/screenshots/heatmap.png) |

## Keyboard Shortcuts

| Key       | Action                  |
| --------- | ----------------------- |
| `Cmd+K`   | Open search             |
| `j` / `k` | Next / previous message |
| `]` / `[` | Next / previous session |
| `o`       | Toggle sort order       |
| `t`       | Toggle thinking blocks  |
| `e`       | Export session as HTML  |
| `p`       | Publish to GitHub Gist  |
| `r`       | Sync sessions           |
| `?`       | Show all shortcuts      |

## PostgreSQL Sync

agentsview can push session data from the local SQLite database to a remote
PostgreSQL instance, enabling shared team dashboards and centralized search
across multiple machines.

### Push Sync (SQLite to PG)

Configure `pg` in `~/.agentsview/config.toml`:

```toml
[pg]
url = "postgres://user:pass@host:5432/dbname?sslmode=require"
machine_name = "my-laptop"
```

Use `sslmode=require` (or `verify-full` for CA-verified connections) for
non-local PostgreSQL instances. Only use `sslmode=disable` for trusted
local/loopback connections.

The `machine_name` identifies which machine pushed each session (must not be
`"local"`, which is reserved).

CLI commands:

```bash
agentsview pg push          # push now
agentsview pg push --full   # force full re-push (bypasses heuristic)
agentsview pg status        # show sync status
```

Push is on-demand — run `pg push` whenever you want to sync to PostgreSQL. There
is no automatic background push.

### PG Read-Only Mode

Serve the web UI directly from PostgreSQL with no local SQLite. Configure
`[pg].url` in config (as shown above), then:

```bash
agentsview pg serve              # default: 127.0.0.1:8080
agentsview pg serve -port 9090   # custom port
```

To have `pg serve` manage a Caddy TLS frontend directly:

The same managed-Caddy prerequisites and backend-loopback requirement described
earlier for normal `serve` mode also apply here.

```bash
agentsview pg serve \
  -host 127.0.0.1 \
  -port 18080 \
  -public-url https://viewer.example.test \
  -proxy caddy \
  -proxy-bind-host 0.0.0.0 \
  -public-port 8443 \
  -tls-cert ~/.certs/viewer.crt \
  -tls-key ~/.certs/viewer.key \
  -allowed-subnet 10.0/16
```

This mode is useful for shared team viewers where multiple machines push to a
central PG database and one or more read-only instances serve the UI. Uploads,
file watching, and local sync are disabled. For managed-Caddy mode, keep the
backend `-host` on loopback and use `-proxy-bind-host` / `-public-port` to
expose the public listener. If you run plain `pg serve` without `-proxy caddy`,
then using a non-loopback `-host` enables token-authenticated remote access and
prints the auth token on startup.

The normal SQLite-backed `serve` mode and PostgreSQL-backed `pg serve` mode keep
separate managed-Caddy state, so both can coexist on one host.

### Known Limitations

- **Deleted sessions**: Sessions permanently pruned from SQLite (via
  `agentsview prune`) are not propagated as deletions to PG. Sessions
  soft-deleted with `deleted_at` are synced correctly.
- **Change detection**: Push uses aggregate length statistics rather than
  content hashes. Use `-full` to force a complete re-push if content was
  rewritten in-place.

## Documentation

Full documentation is available at [agentsview.io](https://agentsview.io):

- [Quick Start](https://agentsview.io/quickstart/) -- installation and first run
- [Usage Guide](https://agentsview.io/usage/) -- dashboard, session browser,
  search, export
- [CLI Reference](https://agentsview.io/commands/) -- commands, flags, and
  environment variables
- [Configuration](https://agentsview.io/configuration/) -- data directory,
  config file, session discovery
- [Architecture](https://agentsview.io/architecture/) -- how the sync engine,
  parsers, and server work

## Development

```bash
make dev            # run Go server in dev mode
make frontend-dev   # run Vite dev server (use alongside make dev)
make desktop-dev    # run Tauri desktop app in dev mode
make test           # Go tests (CGO_ENABLED=1 -tags fts5)
make lint           # golangci-lint (auto-fix)
make e2e            # Playwright E2E tests
make install-hooks  # install pre-commit hooks via prek
```

Pre-commit hooks are managed with [prek](https://github.com/j178/prek) and
require [uv](https://docs.astral.sh/uv/) for the Markdown formatting hook.
Install both, then run `make install-hooks` after cloning:

```bash
# macOS
brew install prek uv

# Linux (or any platform; requires Go on PATH)
curl -LsSf https://astral.sh/uv/install.sh | sh
go install github.com/j178/prek/cmd/prek@latest
# ensure ~/.local/bin and ~/go/bin are on PATH
```

The hooks run `make lint` on every commit and `mdformat` when Markdown files are
staged, auto-fixing formatting issues. If a hook rewrites files, re-stage and
re-commit.

## Desktop Development

The desktop app is a Tauri wrapper under `desktop/`. It launches the
`agentsview` Go binary as a local sidecar and loads `http://127.0.0.1:<port>` in
a native webview.

```bash
make desktop-dev                 # run desktop app in dev mode
make desktop-build               # build desktop bundles (.app/.exe)
make desktop-macos-app           # build macOS .app only
make desktop-windows-installer   # build Windows installer (.exe)
```

Desktop env escape hatch: `~/.agentsview/desktop.env` (for PATH/API keys
overrides).

### Project Structure

```
cmd/agentsview/     CLI entrypoint
internal/config/    Configuration loading
internal/db/        SQLite operations (sessions, search, analytics)
internal/postgres/  PostgreSQL support (push sync, read-only store, schema)
internal/parser/    Session parsers (all supported agents)
internal/server/    HTTP handlers, SSE, middleware
internal/sync/      Sync engine, file watcher, discovery
frontend/           Svelte 5 SPA (Vite, TypeScript)
```

## Supported Agents

| Agent          | Session Directory                                  | Env Override          |
| -------------- | -------------------------------------------------- | --------------------- |
| Claude Code    | `~/.claude/projects/`                              | `CLAUDE_PROJECTS_DIR` |
| Codex          | `~/.codex/sessions/`                               | `CODEX_SESSIONS_DIR`  |
| Copilot        | `~/.copilot/`                                      | `COPILOT_DIR`         |
| Gemini         | `~/.gemini/`                                       | `GEMINI_DIR`          |
| OpenCode       | `~/.local/share/opencode/`                         | `OPENCODE_DIR`        |
| Cursor         | `~/.cursor/projects/`                              | `CURSOR_PROJECTS_DIR` |
| Amp            | `~/.local/share/amp/threads/`                      | `AMP_DIR`             |
| iFlow          | `~/.iflow/projects/`                               | `IFLOW_DIR`           |
| VSCode Copilot | `~/Library/Application Support/Code/User/` (macOS) | `VSCODE_COPILOT_DIR`  |
| Pi             | `~/.pi/agent/sessions/`                            | `PI_DIR`              |
| OpenClaw       | `~/.openclaw/agents/`                              | `OPENCLAW_DIR`        |
| Kimi           | `~/.kimi/sessions/`                                | `KIMI_DIR`            |
| Kiro CLI       | `~/.kiro/sessions/cli/`                            | `KIRO_SESSIONS_DIR`   |
| Kiro IDE       | `~/Library/Application Support/Kiro/` (macOS)      | `KIRO_IDE_DIR`        |
| Cortex Code    | `~/.snowflake/cortex/conversations/`               | `CORTEX_DIR`          |

## Acknowledgements

Inspired by
[claude-history-tool](https://github.com/andyfischer/ai-coding-tools/tree/main/claude-history-tool)
by Andy Fischer and
[claude-code-transcripts](https://github.com/simonw/claude-code-transcripts) by
Simon Willison.

## License

MIT
