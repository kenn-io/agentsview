---
title: Quick Start
description: Get AgentsView running in under a minute
---

CLI examples use the commands from 0.47.0 and later. For older releases, use
the legacy names in the [command migration](commands.md#command-migration).

## Install

### Desktop app

On macOS, the easiest way to install the desktop app is with Homebrew Cask:

```bash
brew install --cask agentsview
```

On Windows and Linux (and as an alternative on macOS), download the latest
`.dmg`, `.exe`, or `.AppImage` from
[GitHub Releases](https://github.com/kenn-io/agentsview/releases). The desktop
app is fully bundled, needs no separate CLI or dependencies, and includes
built-in auto-update support.

!!! note

    The desktop app and CLI share the same data directory (`~/.agentsview/`), so
    you can use one or both together.

On macOS, closing the desktop window hides it instead of quitting AgentsView.
Use the AgentsView menu-bar status item to show the window again, open the logs
folder, check for updates, or quit the desktop app. You can also restore a
hidden window by clicking its Dock icon or selecting it with Cmd-Tab.

To keep AgentsView out of the Dock and Cmd-Tab while its window is closed, turn
on **Hide from Dock and Cmd-Tab when window closed** in the menu-bar menu. This
option is off by default and stays set across relaunches.

#### macOS: use the bundled CLI

The macOS app includes the `agentsview` CLI at
`/Applications/AgentsView.app/Contents/MacOS/agentsview`. Homebrew Cask links it
into Homebrew's `bin` directory automatically.

If you installed the `.app` from a downloaded `.dmg`:

1. Move `AgentsView.app` to Applications.
1. Add its CLI to your current shell's `PATH`:

   ```bash
   export PATH="/Applications/AgentsView.app/Contents/MacOS:$PATH"
   ```

1. Add that same `export` line to `~/.zshrc` (or your shell's startup file if
   you use a different shell) to keep it in new Terminal windows. Adjust the
   path if you installed the app elsewhere.

The CLI follows updates to the app at that location; update it through the
desktop app or Homebrew.

Before you run the bundled CLI for the first time, open AgentsView from
Applications in a graphical login session and complete the macOS first-launch
dialogs. A fresh Homebrew Cask install over SSH has been reported to leave the
CLI hanging without output until you complete this step, and adding it to `PATH`
does not replace that first launch. For an unattended Mac, use the
[standalone CLI installer](#shell-script) instead.

To confirm which CLI your shell finds and check its version, run:

```bash
command -v agentsview
agentsview --version
```

### pip / uvx

```bash
pip install agentsview    # install permanently
uvx agentsview            # or run without installing
```

PyPI publishes platform-specific wheels for Linux (x86_64, aarch64), macOS
(x86_64, arm64), and Windows (x86_64, arm64).

### Shell script

On macOS and Linux, this script installs the standalone CLI without the desktop
app. Use it when you set up a Mac over SSH without a graphical login session.

```bash
curl -fsSL https://agentsview.io/install.sh | bash
```

**Windows (PowerShell):**

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://agentsview.io/install.ps1 | iex"
```

The installer detects your OS and architecture, downloads the latest release
from GitHub Releases, verifies the SHA-256 checksum, and installs the binary.

On macOS and Linux, it installs to `/usr/local/bin` when that directory is
writable, or `~/.local/bin` otherwise. Follow the installer's `PATH`
instructions and run `command -v agentsview` to check which binary your shell
uses. If the standalone CLI and bundled CLI live in different directories, put
the standalone install directory first in `PATH` to select it.

!!! note

    On Windows on ARM, the installer uses the native `arm64` build when one is
    available and otherwise falls back to the `x86_64` build under Windows'
    built-in x64 emulation. To get a native binary on a release that predates
    arm64 support, [build from source](#build-from-source).

### Build from source

Building from source requires Go 1.27.4+ with CGO and Node.js 24.22.1 or a
later 24.x release, or 26.12.1 or later:

```bash
git clone https://github.com/kenn-io/agentsview.git
cd agentsview
make build
make install  # installs to ~/.local/bin if it exists, otherwise Go's bin directory
```

!!! note

    CGO is required for the SQLite driver, and the `fts5` build tag enables
    full-text search.

#### Windows on ARM

`make` is not available on Windows, and CGO needs a C compiler targeting
`aarch64`. Install [Go for Windows/ARM64](https://go.dev/dl/),
[Node.js](https://nodejs.org/), and the aarch64
[llvm-mingw](https://github.com/mstorsjo/llvm-mingw/releases) toolchain (a
self-contained clang and runtime that needs no Visual Studio or Windows SDK),
then build manually in PowerShell:

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

This produces a native ARM64 `agentsview.exe`. Most users do not need to build
manually, since the installer and PyPI wheels ship native arm64 builds, but it
is useful for development or running unreleased changes.

### Docker

AgentsView publishes multi-arch images for `linux/amd64` and `linux/arm64` to
GitHub Container Registry on every tagged release and on pushes to main:

```bash
docker run --rm -p 127.0.0.1:8080:8080 \
  -v agentsview-data:/data \
  -v "$HOME/.claude/projects:/agents/claude:ro" \
  -e CLAUDE_PROJECTS_DIR=/agents/claude \
  ghcr.io/kenn-io/agentsview:latest
```

The image runs `agentsview serve` by default. To read PostgreSQL instead, set
`AGENTSVIEW_MODE=pg-serve` or pass `replica pg serve` as the command; the older
`PG_SERVE=1` selector still works. Inside the container, the image listens on
`0.0.0.0` and requires a bearer token, and flags you add, such as `--port 9000`,
keep those defaults. The generated token is stored in `/data/config.toml`, or
you can supply one with `AGENTSVIEW_AUTH_TOKEN_FILE`. See
[container deployment](/docs/configuration/#container-deployment) for every
variable.

A containerized AgentsView sees only agent sessions from directories you
explicitly bind-mount into the container. Mount each agent's session root
read-only and point the matching
[directory env var](/docs/configuration/#session-discovery) at it. Because root
owns the data volume (`/data`) inside the container, prefer a named Docker
volume over a host bind mount, or pre-create the host directory with the
ownership you want.

A production-style Compose example lives in the repo at
[`docker-compose.prod.yaml`](https://github.com/kenn-io/agentsview/blob/main/docker-compose.prod.yaml).
It persists `/data` in a named volume, mounts Claude, Codex, Forge, and OpenCode
session roots read-only, and publishes the UI on `127.0.0.1:8080`:

```bash
docker compose -f docker-compose.prod.yaml up -d
```

The example publishes the UI on loopback only. The image already requires
bearer-token [authentication](/docs/remote-access/#authentication), and you
should publish the port beyond the host only intentionally.

For a PostgreSQL-backed deployment, point the container at your shared database:

```bash
docker run --rm -p 127.0.0.1:8080:8080 \
  -e AGENTSVIEW_MODE=pg-serve \
  -e AGENTSVIEW_PG_URL='postgres://user:password@host:5432/agentsview?sslmode=require' \
  ghcr.io/kenn-io/agentsview:latest
```

To ingest and publish sessions from the same container, enable the commented
PostgreSQL and push-watch block in `docker-compose.prod.yaml` and set your
database URL. That file mounts the session roots and `/data` and gives the
container 40 seconds to finish its final push.

## Run

The desktop app handles startup automatically: it attaches to an existing local
daemon or starts one in the background, then opens the discovered URL. CLI
commands share the same daemon, which may outlive the desktop window. Daemons
started automatically exit on their own after an idle period when no client
request or daemon-owned job is active.

From the CLI, start the web UI explicitly:

```bash
agentsview serve
```

Bare `agentsview serve` runs in the foreground until you press `Ctrl+C`. If a
compatible server is already running, it reports that server's URL and exits
with an error (status 1).

Running `agentsview serve`:

1. Initializes the SQLite database at `~/.agentsview/sessions.db`.
1. Discovers and syncs sessions from all
   [supported agents](/docs/configuration/#session-discovery).
1. Starts watching session directories for changes.
1. Launches the web UI at `http://127.0.0.1:8080`.

Open `http://127.0.0.1:8080` in your browser. Pass `--no-browser` to disable
automatic browser launch. Alternatively, start the same server in the background
so it can keep running after your shell exits:

```bash
agentsview daemon start
agentsview daemon status
agentsview daemon restart
agentsview daemon stop
```

The daemon includes the web UI, API, session sync, and file watchers. You do not
need to run `serve` after `daemon start`. Stopping it with `daemon stop` or
`serve stop` shuts down the web UI and sync together, and `serve stop` also
stops read-only mirror servers for the same data directory.

`daemon start` and `daemon restart` use the effective configuration from
`config.toml` and supported environment variables, and they do not accept
serve-specific flags. `daemon restart` also starts the daemon if it is stopped.
A restart stays attached and reports startup phases until the replacement daemon
is ready; press `Ctrl+C` to stop waiting without terminating the child, then
inspect it with `agentsview daemon status`.

The existing `agentsview serve --background`, `agentsview serve status`, and
`agentsview serve stop` commands remain available. Use `serve --background` when
a one-off daemon needs a serve-only flag, such as `--no-sync` or an
unauthenticated non-loopback `--host` override. `--no-sync` is runtime-only and
cannot be set in `config.toml`.

You do not need to keep a server running for every CLI command. Read-only
commands attach to the daemon when it is warm, and otherwise read the local
archive directly in read-only mode. Commands that need fresh data or need to
write, including `sync`, `report usage`, `session usage`, `replica pg push`, and
`replica clickhouse push`, auto-start the detached daemon when needed, and the
server remains running after they finish. For scripts or CI jobs that must leave
no background server, stop the daemon first, then run
`AGENTSVIEW_NO_DAEMON=1 agentsview source sync`. That setting disables
auto-start; it does not stop a running daemon or bypass its archive lock.

### Search by meaning

To turn on search by meaning, choose **Settings → Embeddings → Turn on** or run
`agentsview index embeddings build` and approve the download. Embeddings run
locally by default, and agents use keyword search until indexing is ready. Use
`agentsview session search --semantic` or `--hybrid` to search by meaning. See
[Semantic Search](/docs/semantic-search/#enabling-vector) to configure other
providers.

## Customize

Override the default port or host with flags:

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

    If the dashboard flashes and then shows a settings or API error, the server
    usually rejected the forwarded host or origin. It is not a missing auth
    token unless `/api/v1/settings` returns `401`. See
    [Remote Access](/docs/remote-access/#forwarded-dev-environments).

Use environment variables to point to custom session directories. Aider has no
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
export DEEPSEEK_HARNESS_SESSIONS_DIR=~/custom/deepseek-harness/sessions
export FACTORY_DIR=~/custom/factory/sessions
export FORGE_DIR=~/custom/forge
export GEMINI_DIR=~/custom/gemini
export GOOSE_PATH_ROOT=~/custom/goose
export GPTME_DIR=~/custom/gptme/logs
export GROK_DIR=~/custom/grok/sessions
export HERMES_SESSIONS_DIR=~/custom/hermes
export IFLOW_DIR=~/custom/iflow/projects
export JUNIE_DIR=~/custom/junie/sessions
export KILO_DIR=~/custom/kilo
export KILO_LEGACY_DIR=~/custom/kilo-legacy
export KIMI_DIR=~/custom/kimi/sessions
export KIMI_WORK_DIR=~/custom/kimi-work/sessions
export KIRO_SESSIONS_DIR=~/custom/kiro
export KIRO_IDE_DIR=~/custom/kiro-ide
export MIMOCODE_DIR=~/custom/mimocode
export MUSE_SESSIONS_DIR=~/custom/muse/sessions
export VIBE_SESSIONS_DIR=~/custom/vibe/logs/session
export OMP_DIR=~/custom/omp/sessions
export OPENCLAW_DIR=~/custom/openclaw/agents
export OPENCODE_DIR=~/custom/opencode
export OPENCODEREVIEW_DIR=~/custom/opencodereview/sessions
export OPENHANDS_CONVERSATIONS_DIR=~/custom/openhands
export PI_DIR=~/custom/pi/sessions
export PIEBALD_DIR=~/custom/piebald
export POOLSIDE_DIR=~/custom/poolside/trajectories
export POSITRON_DIR=~/custom/positron
export QCLAW_DIR=~/custom/qclaw/agents
export QODER_PROJECTS_DIR=~/custom/qoder/projects
export QWEN_PROJECTS_DIR=~/custom/qwen/projects
export QWENPAW_DIR=~/custom/qwenpaw
export REASONIX_DIR=~/custom/reasonix
export ROOCODE_DIR=~/custom/roocode
export SHELLEY_DIR=~/custom/shelley
export TRAE_DIR=~/custom/trae/User
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

For Claude, Codex, Cursor, and IcodeMate, custom roots may also be `s3://` URIs:

```toml
[agents.claude]
dirs = ["s3://agent-archive/laptop/raw/claude"]
[agents.codex]
dirs = ["s3://agent-archive/laptop/raw/codex"]
[agents.cursor]
dirs = ["s3://agent-archive/laptop/raw/cursor"]
```

Set `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, and optionally
`AWS_S3_ENDPOINT` before you start AgentsView. For the expected object layout
and sync behavior, see
[S3-compatible session sources](/docs/configuration/#s3-compatible-session-sources).

## What you see

Once AgentsView is running, the web UI gives you:

- **Session list.** Filter by project, agent, date, and message count.
- **Message viewer.** Inspect full content, tool calls, and thinking blocks.
- **Session intelligence.** View health grades, outcomes, and signal panels.
- **Full-text search.** Press `Ctrl/Cmd+K` to open search with project and date
  filters.
- **Open session.** Jump to a session by full ID or UUID with `Ctrl/Cmd+G`.
- **Analytics.** Explore activity heatmaps, tool usage, and velocity charts.
- **Activity reporting.** Track concurrency, agent-minutes, cost, and session
  rows.
- **Session export.** Export to standalone HTML, markdown export links for agent
  handoff, or GitHub Gist.

For navigation and resume controls, see the [Usage Guide](/docs/usage/). To
correct project assignments, see the [Data page](/docs/data/) and its opt-in
project workspace.
