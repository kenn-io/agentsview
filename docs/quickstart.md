---
title: Quick Start
description: Get AgentsView running in under a minute
---

## Install

### Desktop App

On macOS, the easiest install is via Homebrew Cask:

```bash
brew install --cask agentsview
```

On Windows and Linux (and as an alternative on macOS), download the latest
`.dmg`, `.exe`, or `.AppImage` from
[GitHub Releases](https://github.com/kenn-io/agentsview/releases). The desktop
app is fully bundled — no CLI or dependencies needed — and includes built-in
auto-update support.

!!! note

    The desktop app and CLI share the same data directory (`~/.agentsview/`), so
    you can use one or both. They are fully complementary, not mutually
    exclusive.

On macOS, closing the desktop window hides it instead of quitting AgentsView.
Use the AgentsView menu-bar status item to show the window again, open the logs
folder, check for updates, or quit the desktop app and its managed backend.

### pip / uvx

```bash
pip install agentsview    # install permanently
uvx agentsview            # or run without installing
```

Platform-specific wheels are published to PyPI for Linux (x86_64, aarch64),
macOS (x86_64, arm64), and Windows (x86_64, arm64).

### Shell Script

```bash
curl -fsSL https://agentsview.io/install.sh | bash
```

**Windows (PowerShell):**

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://agentsview.io/install.ps1 | iex"
```

The installer detects your OS and architecture, downloads the latest release
from GitHub Releases, verifies the SHA-256 checksum, and installs the binary.

!!! note

    On Windows on ARM, the installer uses the native `arm64` build when one is
    available and otherwise falls back to the `x86_64` build, which runs under
    Windows' built-in x64 emulation. To get a native binary on a release that
    predates arm64 support, [build from source](#build-from-source).

### Build from source

Requires Go 1.26+ with CGO and Node.js 22+:

```bash
git clone https://github.com/kenn-io/agentsview.git
cd agentsview
make build
make install  # installs to ~/.local/bin
```

!!! note

    CGO is required for the SQLite driver. The `fts5` build tag enables
    full-text search.

#### Windows on ARM

`make` isn't available on Windows, and CGO needs a C compiler that targets
`aarch64`. Install [Go for Windows/ARM64](https://go.dev/dl/),
[Node.js](https://nodejs.org/), and the aarch64
[llvm-mingw](https://github.com/mstorsjo/llvm-mingw/releases) toolchain (a
self-contained clang + runtime; no Visual Studio or Windows SDK required), then
build manually in PowerShell:

```powershell
git clone https://github.com/kenn-io/agentsview.git
cd agentsview

# 1. Build the frontend and embed it
cd frontend; npm ci; npm run build; cd ..
Copy-Item -Recurse -Force frontend/dist internal/web/dist

# 2. Compile with the aarch64 toolchain
$env:CGO_ENABLED = "1"
$env:CC = "C:\path\to\llvm-mingw\bin\aarch64-w64-mingw32-clang.exe"
go build -tags fts5 -o agentsview.exe ./cmd/agentsview
```

The resulting `agentsview.exe` is a native ARM64 binary. Most users don't need
this — the installer and PyPI wheels ship native arm64 builds — but it's useful
for development or to run unreleased changes.

### Docker

Multi-arch images for `linux/amd64` and `linux/arm64` are published to GitHub
Container Registry on every tagged release and on pushes to main:

```bash
docker run --rm -p 127.0.0.1:8080:8080 \
  -v agentsview-data:/data \
  -v "$HOME/.claude/projects:/assets/static/agents/claude:ro" \
  -e CLAUDE_PROJECTS_DIR=/assets/static/agents/claude \
  ghcr.io/kenn-io/agentsview:latest
```

The container's entrypoint runs `agentsview serve` by default. Set `PG_SERVE=1`
to switch to `agentsview pg serve` instead — the same image powers both modes.

A containerized AgentsView only sees agent sessions from directories you
explicitly bind-mount into the container. Mount each agent's session root
read-only and point the matching
[directory env var](/configuration/#session-discovery) at it. The data volume
(`/data`) is owned by root inside the container, so prefer a named Docker volume
over a host bind mount, or pre-create the host directory with the desired
ownership.

A production-style Compose example lives in the repo at
[`docker-compose.prod.yaml`](https://github.com/kenn-io/agentsview/blob/main/docker-compose.prod.yaml).
It persists `/data` in a named volume, mounts Claude, Codex, and OpenCode
session roots read-only, and publishes the UI on `127.0.0.1:8080`:

```bash
docker compose -f docker-compose.prod.yaml up -d
```

The example publishes the UI on loopback only. To expose it beyond the host,
also enable bearer-token [authentication](/remote-access/#authentication) and
publish the port intentionally.

For a PostgreSQL-backed deployment, point the container at your shared database:

```bash
docker run --rm -p 127.0.0.1:8080:8080 \
  -e PG_SERVE=1 \
  -e AGENTSVIEW_PG_URL='postgres://user:password@host:5432/agentsview?sslmode=require' \
  ghcr.io/kenn-io/agentsview:latest
```

## Run

The desktop app handles startup automatically: it attaches to an existing local
daemon or starts one in the background, then opens the discovered URL. The
daemon is shared with CLI commands and may outlive the desktop window, but it
self-exits after an idle period when no client request or daemon-owned job is
active.

CLI users can start the web UI explicitly:

```bash
agentsview serve
```

Bare `agentsview serve` runs in the foreground until you press `Ctrl+C`.

This will:

1. Initialize the SQLite database at `~/.agentsview/sessions.db`
1. Discover and sync sessions from all
   [supported agents](/configuration/#session-discovery)
1. Start watching session directories for changes
1. Launch the web UI at `http://127.0.0.1:8080`

Open `http://127.0.0.1:8080` in your browser. Pass `--no-browser` to disable
automatic browser launch. To keep the server running after your shell exits, use
the canonical daemon lifecycle:

```bash
agentsview daemon start
agentsview daemon status
agentsview daemon restart
agentsview daemon stop
```

`daemon start` and `daemon restart` use the normal effective configuration from
`config.toml` and supported environment variables. They do not accept
serve-specific flags. `daemon restart` also starts the daemon when it is
stopped. A restart stays attached and reports startup phases until the
replacement daemon is ready; press `Ctrl+C` to stop waiting without terminating
the child, then inspect it with `agentsview daemon status`.

The existing `agentsview serve --background`, `agentsview serve status`, and
`agentsview serve stop` commands remain available. Use `serve --background` when
a one-off daemon needs a serve-only flag, such as `--no-sync` or an
unauthenticated non-loopback `--host` override. `--no-sync` is runtime-only and
cannot be set in `config.toml`.

You do not need to keep a server running for every CLI command. Read-only
commands attach to the daemon when it is warm, otherwise they read the local
archive directly in read-only mode. Commands that need fresh data or need to
write, including `sync`, `usage`, `token-use`, `pg push`, and `duckdb push`,
auto-start the detached daemon when needed. Set `AGENTSVIEW_NO_DAEMON=1` for
scripts or CI jobs that must never start a lingering background process.

## Customize

Override the default port or host:

```bash
agentsview serve --port 9090
agentsview serve --host 0.0.0.0 --port 3000
agentsview serve --no-browser  # Disable browser auto-open
agentsview serve --background --no-sync  # One-off flag-driven background run
```

!!! tip "Forwarded development environments"

    If you open AgentsView through exe.dev, Codespaces, Coder, WSL2, SSH port
    forwarding, or a reverse proxy, restart the CLI with `--public-url` set to
    the exact browser origin:
    `agentsview serve --public-url https://<vm>.exe.xyz`.

    A dashboard flash followed by a settings or API error usually means the
    server rejected the forwarded host or origin. It is not a missing auth
    token unless `/api/v1/settings` returns `401`. See
    [Remote Access](/remote-access/#forwarded-dev-environments).

Point to custom session directories with environment variables. Aider has no
default discovery root, so set `AIDER_DIR` to opt into scanning Aider logs:

```bash
export AIDER_DIR=~/code
export AMP_DIR=~/custom/amp/threads # historical local Amp threads only
export ANTIGRAVITY_DIR=~/custom/antigravity
export ANTIGRAVITY_CLI_DIR=~/custom/antigravity-cli
export CLAUDE_PROJECTS_DIR=~/custom/claude/projects
export COWORK_DIR=~/custom/cowork
export CODEX_SESSIONS_DIR=~/custom/codex/sessions
export COMMANDCODE_PROJECTS_DIR=~/custom/commandcode/projects
export COPILOT_DIR=~/custom/copilot
export CORTEX_DIR=~/custom/cortex
export CURSOR_PROJECTS_DIR=~/custom/cursor/projects
export DEEPSEEK_TUI_SESSIONS_DIR=~/custom/deepseek/sessions
export FORGE_DIR=~/custom/forge
export GEMINI_DIR=~/custom/gemini
export GPTME_DIR=~/custom/gptme/logs
export GROK_DIR=~/custom/grok/sessions
export HERMES_SESSIONS_DIR=~/custom/hermes
export IFLOW_DIR=~/custom/iflow/projects
export KILO_DIR=~/custom/kilo
export KIMI_DIR=~/custom/kimi/sessions
export KIMI_WORK_DIR=~/custom/kimi-work/sessions
export KIRO_SESSIONS_DIR=~/custom/kiro
export KIRO_IDE_DIR=~/custom/kiro-ide
export MIMOCODE_DIR=~/custom/mimocode
export VIBE_SESSIONS_DIR=~/custom/vibe/logs/session
export OMP_DIR=~/custom/omp/sessions
export OPENCLAW_DIR=~/custom/openclaw/agents
export OPENCODE_DIR=~/custom/opencode
export OPENHANDS_CONVERSATIONS_DIR=~/custom/openhands
export PI_DIR=~/custom/pi/sessions
export PIEBALD_DIR=~/custom/piebald
export POSITRON_DIR=~/custom/positron
export QCLAW_DIR=~/custom/qclaw/agents
export QODER_PROJECTS_DIR=~/custom/qoder/projects
export QWEN_PROJECTS_DIR=~/custom/qwen/projects
export QWENPAW_DIR=~/custom/qwenpaw
export REASONIX_DIR=~/custom/reasonix
export SHELLEY_DIR=~/custom/shelley
export VISUALSTUDIO_COPILOT_DIR=~/custom/visualstudio-copilot/traces
export VSCODE_COPILOT_DIR=~/custom/vscode
export WINDSURF_DIR=~/custom/windsurf/User
export WARP_DIR=~/custom/warp
export WORKBUDDY_PROJECTS_DIR=~/custom/workbuddy/projects
export ZCODE_DIR=~/custom/zcode/cli
export ZED_DIR=~/custom/zed
export ZENCODER_DIR=~/custom/zencoder/sessions
agentsview serve
```

For Claude and Codex, custom roots may also be `s3://` URIs:

```toml
claude_project_dirs = ["s3://agent-archive/laptop/raw/claude"]
codex_sessions_dirs = ["s3://agent-archive/laptop/raw/codex"]
```

Set `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, and optionally
`AWS_S3_ENDPOINT` before starting AgentsView. See
[S3-compatible session sources](/configuration/#s3-compatible-session-sources)
for the expected object layout and sync behavior.

## What You'll See

Once running, the web UI provides:

- **Session list** with filtering by project, agent, date, and message count
- **Message viewer** with full content, tool calls, and thinking blocks
- **Session intelligence** with health grades, outcomes, and signal panels
- **Full-text search** across all message content
- **Analytics** including activity heatmaps, tool usage, and velocity charts
- **Activity reporting** with concurrency, agent-minutes, cost, and session rows
- **Session export** to standalone HTML, markdown export links for agent
  handoff, or GitHub Gist

Beyond full-text search, opt-in semantic search lets
`agentsview session search --semantic` (or `--hybrid`) match session content by
meaning, backed by a local or hosted embeddings endpoint. See
[Semantic Search](/semantic-search/) for setup.
