# agentsview

Browse, search, and track costs across all your AI coding agents. One binary, no
accounts, everything local.

<p align="center">
  <img src="https://agentsview.io/assets/generated/screenshots/dashboard.png" alt="Analytics dashboard" width="720">
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
agentsview serve           # start the server in the foreground
agentsview daemon start    # start the writable SQLite daemon
agentsview daemon status   # show daemon status
agentsview daemon restart  # restart from current configuration
agentsview daemon stop     # stop the writable daemon
agentsview session list    # read from the daemon if warm, otherwise SQLite
agentsview usage daily     # print daily cost summary
```

On first run, agentsview discovers sessions from every supported agent on your
machine, syncs them into a local SQLite database, and serves a web UI at
`http://127.0.0.1:8080`.

For Devin CLI, point `DEVIN_DIR` or `devin_dirs` at the local root that contains
`cli/` — for example `~/Library/Application Support/devin` on macOS,
`~/.local/share/devin` on Linux, or a redacted path like
`.../Application Support/devin`. AgentsView reads session data under
`<root>/cli/...` and intentionally ignores copied config or OAuth paths. Do not
paste tokens, OAuth files, or other secrets into bug reports.

Claude and Codex sources can also be configured as `s3://` roots, so a central
AgentsView instance can read sessions that other machines push to S3-compatible
object storage. Add those roots to `claude_project_dirs` or
`codex_sessions_dirs`; AgentsView lists object metadata and only downloads
changed sessions during sync. S3 change detection uses size, modified time, and
available object fingerprints such as ETag, version ID, or checksums.

The desktop app and freshness-sensitive CLI commands share a detached local
daemon. Read-only CLI commands attach to it when it is already running, but fall
back to direct read-only SQLite on a cold archive so one-off scripts stay fast.
Commands that need fresh data or need to write, such as `sync`, `usage`,
`token-use`, `pg push`, and `duckdb push`, auto-start the daemon when needed.

Use `agentsview daemon start` when you want to start the writable SQLite daemon
explicitly. It loads the normal effective configuration from `config.toml` and
supported environment variables; `daemon start` and `daemon restart` accept no
serve-specific flags. Background daemons self-exit after an idle period unless a
client request or daemon-owned job is active.

The existing `agentsview serve --background`, `agentsview serve status`, and
`agentsview serve stop` commands remain available. Use `serve --background` when
a one-off daemon needs a serve-only flag, such as `--no-sync` or an
unauthenticated non-loopback `--host` override.

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

Example DuckDB mirror startup:

```bash
# Populate /data/sessions.duckdb from the mounted SQLite archive.
docker run --rm \
  -v agentsview-data:/data \
  -v "$HOME/.claude/projects:/agents/claude:ro" \
  -e CLAUDE_PROJECTS_DIR=/agents/claude \
  ghcr.io/kenn-io/agentsview:latest duckdb push --full

# Serve the populated mirror read-only.
docker run --rm -p 127.0.0.1:8080:8080 \
  -v agentsview-data:/data \
  ghcr.io/kenn-io/agentsview:latest duckdb serve
```

Example Quack startup:

```bash
# Expose the local DuckDB mirror over Quack from the host/container.
QUACK_TOKEN="$(openssl rand -base64 32)"
docker run --rm -p 127.0.0.1:9494:9494 \
  -v agentsview-data:/data \
  ghcr.io/kenn-io/agentsview:latest \
  duckdb quack serve \
    --bind quack:0.0.0.0:9494 \
    --token "$QUACK_TOKEN" \
    --allow-insecure

# Serve the web UI from a remote Quack endpoint.
# The client url uses the native quack:HOST:PORT form; the Quack extension
# does not accept quack:http:// or quack:https:// urls. A non-loopback host
# needs --allow-insecure.
docker run --rm -p 127.0.0.1:8080:8080 \
  -e AGENTSVIEW_DUCKDB_URL='quack:duckdb.example.com:9494' \
  -e AGENTSVIEW_DUCKDB_TOKEN="$QUACK_TOKEN" \
  ghcr.io/kenn-io/agentsview:latest duckdb serve --allow-insecure
```

Keep Quack on loopback or behind TLS. Plain HTTP Quack on a non-loopback bind
requires `--allow-insecure` and should only be used behind a trusted tunnel or
reverse proxy.

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
- Authoritative Copilot CLI billing totals when session logs provide them
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

| Dashboard                                                                      | Session viewer                                                                           |
| ------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------- |
| ![Dashboard](https://agentsview.io/assets/generated/screenshots/dashboard.png) | ![Session viewer](https://agentsview.io/assets/generated/screenshots/message-viewer.png) |

| Search                                                                           | Activity heatmap                                                           |
| -------------------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| ![Search](https://agentsview.io/assets/generated/screenshots/search-results.png) | ![Heatmap](https://agentsview.io/assets/generated/screenshots/heatmap.png) |

- **Full-text search** across all message content (FTS5)
- **Semantic search** (opt-in) -- index session content with any
  OpenAI-compatible embeddings endpoint and search by meaning with
  `agentsview session search --semantic` or `--hybrid`; every content-search
  match cites the conversation unit it came from
  ([docs](https://agentsview.io/semantic-search/))
- **Token usage and cost dashboard** -- per-session and per-model cost
  breakdowns, daily spend charts, all in the web UI
- **Analytics dashboard** -- activity heatmaps, tool usage, velocity metrics,
  project breakdowns
- **Recent Edits feed** -- the files your agents changed most recently across
  every session, grouped by project and path, each linking to the message that
  made the change
- **Live updates** via SSE as active sessions receive new messages
- **Keyboard-first** navigation (`j`/`k`/`[`/`]`, `Cmd+K` search, `?` for all
  shortcuts)
- **Export** sessions as HTML or publish to GitHub Gist

## Supported Agents

agentsview discovers sessions from all of these. Aider is opt-in because it has
no central session directory; set `AIDER_DIR` or `aider_dirs` to enable it. Amp
support is deprecated because current Amp releases may store threads server-side
and leave only local stubs; agentsview can still parse historical local Amp
thread JSON files.

| Agent                 | Session Directory                                                                                                                                                       |
| --------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Aider                 | `<repo>/.aider.chat.history.md` (per repo; opt in with `AIDER_DIR` or `aider_dirs`)                                                                                     |
| Amp (deprecated)      | `~/.local/share/amp/threads/` (historical local thread JSON only)                                                                                                       |
| Antigravity           | `~/.gemini/antigravity/`                                                                                                                                                |
| Antigravity CLI       | `~/.gemini/antigravity-cli/` (see note below)                                                                                                                           |
| Claude Code           | `~/.claude/projects/`                                                                                                                                                   |
| OpenClaude            | `~/.openclaude/projects/`                                                                                                                                               |
| Claude Cowork         | `~/Library/Application Support/Claude/local-agent-mode-sessions/` (macOS)                                                                                               |
| Codex                 | `~/.codex/sessions/`                                                                                                                                                    |
| Copilot CLI           | `~/.copilot/`                                                                                                                                                           |
| Devin CLI             | `~/.local/share/devin/` (Linux), `~/Library/Application Support/devin/` (macOS); point `DEVIN_DIR` / `devin_dirs` at the root that contains `cli/`                      |
| Cortex Code           | `~/.snowflake/cortex/conversations/`                                                                                                                                    |
| Cursor                | `~/.cursor/projects/`                                                                                                                                                   |
| DeepSeek TUI          | `~/.codewhale/sessions/`, `~/.deepseek/sessions/`                                                                                                                       |
| Forge                 | `~/.forge/`                                                                                                                                                             |
| Gemini CLI            | `~/.gemini/`                                                                                                                                                            |
| gptme                 | `~/.local/share/gptme/logs/`                                                                                                                                            |
| Grok                  | `~/.grok/sessions/`                                                                                                                                                     |
| Hermes Agent          | `~/.hermes/sessions/`                                                                                                                                                   |
| iFlow                 | `~/.iflow/projects/`                                                                                                                                                    |
| Kilo                  | `~/.local/share/kilo/`                                                                                                                                                  |
| Kimi                  | `~/.kimi/sessions/`                                                                                                                                                     |
| Kiro CLI              | `~/.kiro/sessions/cli/`, `~/.local/share/kiro-cli/`                                                                                                                     |
| Kiro IDE              | `~/Library/Application Support/Kiro/` (macOS)                                                                                                                           |
| MiMoCode              | `~/.local/share/mimocode/`                                                                                                                                              |
| Mistral Vibe          | `~/.vibe/logs/session/`                                                                                                                                                 |
| OpenClaw              | `~/.openclaw/agents/`                                                                                                                                                   |
| OpenCode              | `~/.local/share/opencode/`                                                                                                                                              |
| OpenHands CLI         | `~/.openhands/conversations/`                                                                                                                                           |
| OhMyPi                | `~/.omp/agent/sessions/`                                                                                                                                                |
| Pi                    | `~/.pi/agent/sessions/`                                                                                                                                                 |
| Piebald               | `~/.local/share/piebald/`                                                                                                                                               |
| Posit Assistant       | `~/.posit/assistant/workspaces/`                                                                                                                                        |
| Positron Assistant    | `~/Library/Application Support/Positron/User/` (macOS)                                                                                                                  |
| QClaw                 | `~/.qclaw/agents/`                                                                                                                                                      |
| Qoder                 | `~/.qoder/projects/`, `~/.qoderwork/projects/`                                                                                                                          |
| Qwen Code             | `~/.qwen/projects/`                                                                                                                                                     |
| QwenPaw               | `~/.copaw/workspaces/`, `~/.qwenpaw/workspaces/`                                                                                                                        |
| Reasonix              | `~/.reasonix/`, `%APPDATA%\\reasonix\\` (Windows)                                                                                                                       |
| RooCode               | `~/Library/Application Support/Code/User/globalStorage/rooveterinaryinc.roo-cline/` (macOS), `~/.config/Code/User/globalStorage/rooveterinaryinc.roo-cline/` (Linux), `%APPDATA%\\Code\\User\\globalStorage\\rooveterinaryinc.roo-cline\\` (Windows) |
| VSCode Copilot        | `~/Library/Application Support/Code/User/` (macOS)                                                                                                                      |
| Visual Studio Copilot | `%LOCALAPPDATA%\\Temp\\VSGitHubCopilotLogs\\traces\\` (Windows), `~/Library/Caches/VSGitHubCopilotLogs/traces/` (macOS), `~/.cache/VSGitHubCopilotLogs/traces/` (Linux) |
| Windsurf              | `~/Library/Application Support/Windsurf/User/` (macOS), `~/.config/Windsurf/User/` (Linux), `%APPDATA%\\Windsurf\\User\\` (Windows)                                     |
| Trae                  | `%APPDATA%\\Trae\\User\\` (Windows), `~/Library/Application Support/Trae/User/` (macOS), `~/.config/Trae/User/` (Linux)                                                                                             |
| Warp                  | `~/.warp/` (platform-dependent)                                                                                                                                         |
| WorkBuddy             | `~/.workbuddy/projects/`                                                                                                                                                |
| ZCode                 | `~/.zcode/cli/db/`, `~/.zcode/cli/`                                                                                                                                     |
| Zed                   | `~/Library/Application Support/Zed/` (macOS)                                                                                                                            |
| Zencoder              | `~/.zencoder/sessions/`                                                                                                                                                 |

Grok sessions are read from `summary.json` (title, timestamps, project),
optional `signals.json` (token counters), and `chat_history.jsonl` when present
for the full transcript (user turns, assistant replies, thinking, and tool
calls). If `chat_history.jsonl` is missing, AgentsView falls back to
summary-only mode. Set `GROK_DIR` or `grok_dirs` to override the default
directory.

Each directory can be overridden with an environment variable. See the
[configuration docs](https://agentsview.io/configuration/) for details. Cursor
attribution stats are a live, machine-local read from
`~/.cursor/ai-tracking/ai-code-tracking.db` by default and can be redirected
with `AGENTSVIEW_CURSOR_ATTRIBUTION_DB`; they are not synced or aggregated
through PostgreSQL.

### Aider: per-repo Markdown logs

Aider has no central session store; it writes one `.aider.chat.history.md`
Markdown log per repository, and one log accumulates many runs (one per `aider`
launch, delimited by `# aider chat started at ...` headers). agentsview indexes
**each run as its own session**.

AgentsView does not scan for Aider logs by default. Earlier builds attempted an
always-on bounded scan of the home directory, but that was not trustworthy:
desktop launches and background usage refreshes could still trigger macOS
privacy prompts for protected folders. To enable Aider, point `AIDER_DIR` (or
the `aider_dirs` config key) at a code root you explicitly want scanned. The
scan descends at most four levels below each configured root, skips
vendor/build/VCS directories by name (`node_modules`, `target`, `.git`,
`Library`, `go`, `.cargo`, and similar), and stops after a two-second wall-clock
budget. On macOS, broad home roots still skip protected top-level folders unless
one of those folders is configured directly. The live file watcher only watches
configured Aider roots shallowly; new repos are picked up by the periodic sync,
which runs every 15 minutes.

Because the format is Markdown-derived, roles are reconstructed from line
prefixes and there are no per-message timestamps; a run's start time comes from
its `# aider chat started at ...` header (written in local time, assumed UTC).

### JetBrains Copilot via exporter

JetBrains IDEs store Copilot chat in a Nitrite database that agentsview does not
read directly. The supported path today is to export those sessions to Copilot
JSONL with
[copilot-jetbrains-exporter](https://github.com/MCBoarder289/copilot-jetbrains-exporter),
then point agentsview at that output directory.

```bash
# Export JetBrains Copilot sessions to JSONL
copilot-jetbrains-exporter --output ~/.copilot/jetbrains-sessions

# Tell agentsview to index the exported sessions
export COPILOT_DIR=~/.copilot/jetbrains-sessions
```

Or in `~/.agentsview/config.toml`:

```toml
copilot_dirs = ["~/.copilot/jetbrains-sessions"]
```

Re-run the exporter after new JetBrains Copilot sessions if you want agentsview
to pick up fresh conversations from that source.

### Antigravity CLI: high-resolution transcripts

Antigravity CLI sessions appear in two on-disk formats: newer releases store
conversation trajectories as SQLite `.db` files, older releases used
AES-GCM-encrypted `.pb` files. For either format, the full transcript --
structured tool calls, results, reasoning, and diffs -- comes from a
`<uuid>.trajectory.json` sidecar. Without a covering sidecar, agentsview falls
back to **summary mode**: a heuristic decode of the raw `.db` steps (prompts and
tool-call names only), or for `.pb` sessions your prompts from `history.jsonl`
plus any plain-text artifacts under `brain/` (plans, walkthroughs, checkpoints).
Summary-mode sessions show a "Summary mode" badge in the detail header that
links here.

To unlock full transcripts for `.db` and `.pb` sessions alike, run
[agy-reader](https://github.com/mjacobs/agy-reader) alongside agentsview.
agy-reader talks to the local Antigravity daemon, decrypts each conversation,
and writes a `<uuid>.trajectory.json` sidecar next to the source file.
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
agentsview pg push             # push local data to the default PG target
agentsview pg push archive     # push to one named PG target
agentsview pg push --all       # push every configured PG target sequentially
agentsview pg status           # show status for the default PG target
agentsview pg status archive   # show status for one named PG target
agentsview pg status --all     # show status for every configured PG target
agentsview pg serve            # serve web UI from the default PG target (read-only)
```

Single-target configs still use the legacy `[pg]` block. To manage more than one
PostgreSQL destination, define named `[pg.NAME]` blocks and set `default_pg`
when more than one target exists:

```toml
default_pg = "work"

[pg.work]
url = "postgres://user:pass@work-db/agentsview"
machine_name = "laptop"

[pg.archive]
url = "postgres://user:pass@archive-db/agentsview"
machine_name = "laptop-archive"
exclude_projects = ["scratch"]
```

Named target names are normalized case-insensitively. `all`, `local`, and the
legacy `[pg]` field names `url`, `schema`, `machine_name`, `allow_insecure`,
`projects`, and `exclude_projects` cannot be used for `[pg.NAME]`.

`AGENTSVIEW_PG_URL`, `AGENTSVIEW_PG_SCHEMA`, and `AGENTSVIEW_PG_MACHINE` still
work, but in named-target mode they apply only to the effective default target.
They do not rewrite every named `[pg.NAME]` entry.

### Automatic push (background service)

To keep a shared PostgreSQL database current without running `pg push` by hand,
run the auto-push daemon. It watches your session directories and pushes shortly
after new sessions are recorded, with a periodic floor as a safety net:

```bash
agentsview pg push --watch                 # foreground, Ctrl-C to stop
agentsview pg push archive --watch         # watch one named PG target
agentsview pg push --watch --debounce 1m   # custom coalesce window
agentsview pg push --watch --interval 5m   # custom floor interval
```

`--watch` follows the default PG target unless you pass one target name.
`--all --watch` is rejected; multi-target background watch remains out of scope
for now.

The daemon reads the same `[pg]` config as `pg push`, so the PostgreSQL DSN must
be set in your config file (or an environment variable it expands). Protect the
config file, since it holds credentials:

```bash
chmod 600 ~/.agentsview/config.toml
```

To run it unattended as an OS service (launchd on macOS, `systemd --user` on
Linux):

```bash
agentsview pg service install     # generate the unit, enable + start it
agentsview pg service status      # show manager status
agentsview pg service logs -f     # follow the service log
agentsview pg service uninstall   # stop and remove
```

`pg serve` and `pg service` always use the effective default PG target. In
named-target mode, set `default_pg` to choose which target those long-running
commands use.

**Linux headless machines:** systemd `--user` services stop at logout and do not
start at boot unless lingering is enabled for your user. `install` detects this
and prints the command; you can also run it yourself:

```bash
loginctl enable-linger "$USER"
```

See [PostgreSQL docs](https://agentsview.io/postgresql/) for setup and
configuration.

## DuckDB Mirror and Quack

DuckDB support is a mirror backend, not a replacement for the local SQLite
archive. `agentsview serve` still performs primary ingestion into SQLite. Use
DuckDB when you want a portable analytics file, read-only local serving from a
mirror, or remote read access through DuckDB's Quack protocol.

```bash
agentsview duckdb push          # mirror SQLite into DuckDB
agentsview duckdb status        # show mirror sync status
agentsview duckdb serve         # serve web UI from DuckDB (read-only)
agentsview duckdb quack serve   # expose the local DuckDB file over Quack
```

`agentsview duckdb serve` reads `[duckdb].path` or `AGENTSVIEW_DUCKDB_PATH`. To
serve from a remote Quack endpoint, set `AGENTSVIEW_DUCKDB_URL` and
`AGENTSVIEW_DUCKDB_TOKEN` instead. Quack is still a new DuckDB protocol, so
agentsview keeps conservative defaults: local Quack serving binds to loopback,
requires a token, and rejects non-loopback plain HTTP unless `--allow-insecure`
is explicit. For remote use, prefer a TLS URL or put Quack behind an
authenticated tunnel/proxy.

Backend modes:

- SQLite: primary local archive, file sync, FTS5 search, and writable UI.
- PostgreSQL: optional shared team backend; push from SQLite, serve read-only.
- DuckDB: optional mirror file or Quack endpoint; push from SQLite, serve
  read-only.

Troubleshooting:

- If `duckdb push` fails to open the mirror, confirm the binary was built with
  the DuckDB Go driver for your platform and that `AGENTSVIEW_DUCKDB_PATH`
  points to a writable file location.
- If Quack commands fail with extension errors, update the agentsview binary so
  the embedded DuckDB runtime includes the Quack extension.
- If a remote attach fails, check the token, the `quack:` URL, TLS/proxy
  termination, and whether the server was intentionally started with
  `--allow-insecure` for plain non-loopback binds.
- DuckDB search currently uses substring/regex fallback behavior. SQLite FTS5
  remains the indexed search path for primary local serving.

## Privacy

agentsview sends a limited anonymous `daemon_active` telemetry ping to PostHog
when the server starts and every 24 hours while it runs, using a stable random
install ID as the event `DistinctId`. The event includes
`application=agentsview`, app version, commit, OS, and CPU architecture, with
`$process_person_profile=false` and `$geoip_disable=true`. It does not include
session, project, prompt, file path, account, or machine identity. Disable
telemetry with `AGENTSVIEW_TELEMETRY_ENABLED=0` or `TELEMETRY_ENABLED=0`.
Telemetry is also hard-disabled in Go test binaries, regardless of environment.

All session data stays on your machine. The server binds to `127.0.0.1` by
default. The update check is optional and can be disabled with
`--no-update-check`.

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
make frontend-dev   # Vite+ dev server (run alongside make dev)
make build          # build binary with embedded frontend
make install        # install to ~/.local/bin
```

```bash
make test           # Go tests (CGO_ENABLED=1 -tags "fts5")
make bench-backends # compare SQLite, DuckDB, and PostgreSQL store reads
make lint           # golangci-lint + NilAway
make nilaway        # NilAway through custom golangci-lint
make e2e            # Playwright E2E tests
```

`make bench-backends` requires Docker. It starts a PostgreSQL container with
testcontainers, mirrors the same SQLite fixture into DuckDB and PostgreSQL, and
benchmarks the shared `db.Store` read queries for relative comparison. The
default fixture is 1,000 sessions and 64,000 messages; use
`BENCH_BACKENDS_SESSIONS` and `BENCH_BACKENDS_MESSAGES_PER_SESSION` to scale it.
When the Docker CLI uses a non-default socket, export `DOCKER_HOST` for that
socket before running the benchmark.

Pre-commit and pre-push hooks via [prek](https://github.com/j178/prek): run
`make lint-tools` and `make install-hooks` after cloning (requires `prek` and
`uv`).

### Project Layout

```
cmd/agentsview/     CLI entrypoint
internal/           Go packages (config, db, parser, server, sync, postgres)
frontend/           Svelte 5 SPA (Vite+, TypeScript)
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
