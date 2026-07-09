---
title: Configuration
description: Config file, default paths, and runtime settings
---

## Data Directory

AgentsView stores all persistent data under a single directory, defaulting to
`~/.agentsview/`. Override with the `AGENTSVIEW_DATA_DIR` environment variable.

!!! note

    `AGENT_VIEWER_DATA_DIR` is still accepted as a legacy fallback when
    `AGENTSVIEW_DATA_DIR` is unset, but new setups should use
    `AGENTSVIEW_DATA_DIR`.

```
~/.agentsview/
├── sessions.db      # SQLite database (WAL mode)
├── vectors.db       # Semantic-search vector index (when [vector] is enabled)
├── config.toml      # Configuration file
├── config.toml.lock # Serializes concurrent config writers
├── db.write.lock    # Per-data-dir SQLite write-owner lock
├── serve.log        # Detached daemon log
└── uploads/         # Uploaded session files
```

The desktop app and CLI share a detached local daemon for fresh reads and
writes. A running daemon owns local SQLite writes for this data directory and
self-exits after an idle period. Read-only CLI commands can still open
`sessions.db` directly in read-only mode when no daemon is running. Set
`AGENTSVIEW_NO_DAEMON=1` for scripts or CI jobs that must never auto-start a
daemon.

The Cursor source in code attribution stats is a live, machine-local read
from `~/.cursor/ai-tracking/ai-code-tracking.db` by default. Set
`AGENTSVIEW_CURSOR_ATTRIBUTION_DB` when Cursor stores that database
somewhere else on the host answering the stats request. The attribution
database is not synced into AgentsView's archive and is not pushed to
PostgreSQL.

## Config File

The config file at `~/.agentsview/config.toml` is auto-created on first run. It
stores persistent settings that survive restarts.

!!! note

    The config format changed from JSON to TOML. Existing `config.json` files
    are automatically migrated to `config.toml` on first run (the JSON file is
    renamed to `config.json.bak`).

```toml
cursor_secret = "base64-encoded-secret"
require_auth = true
cursor_admin_api_key = "key_xxxxx"
daemon_idle_timeout = "20m"
```

| Field                               | Description                                                                                                  |
| ----------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| `cursor_secret`                     | Auto-generated HMAC key for pagination cursor signing                                                        |
| `cursor_admin_api_key`              | Cursor Admin API key used by `agentsview usage cursor`                                                       |
| `cursor_admin_email`                | Optional default Cursor Admin usage filter by member email                                                   |
| `cursor_admin_user_id`              | Optional default Cursor Admin usage filter by member user ID                                                 |
| `github_token`                      | Optional saved GitHub token for Gist publishing                                                              |
| `result_content_blocked_categories` | Tool categories whose result content is not stored (default: `["Read", "Glob"]`)                             |
| `host`                              | Interface the server binds to (default `127.0.0.1`); non-loopback values require `require_auth = true`       |
| `require_auth`                      | Require bearer-token authentication for API access                                                           |
| `auth_token`                        | Auto-generated 256-bit bearer token for remote access; can be overridden with `AGENTSVIEW_AUTH_TOKEN`        |
| `public_url`                        | Public URL for hostname/proxy access and origin validation                                                   |
| `public_origins`                    | Array of additional trusted CORS origins                                                                     |
| `daemon_idle_timeout`               | Idle timeout for detached `serve --background` daemons; set to `"0s"` to keep them alive                     |
| `[proxy]`                           | Managed proxy configuration table — see [Remote Access](/remote-access/)                                     |
| `disable_update_check`              | Disable the automatic update check (see [Privacy](#privacy-and-telemetry))                                   |
| `[pg]`                              | PostgreSQL sync configuration — see [PostgreSQL Sync](/pg-sync/)                                             |
| `[duckdb]`                          | DuckDB mirror configuration — see [DuckDB Mirror](/duckdb/)                                                  |
| `[vector]`                          | Opt-in semantic-search index; model settings live in `[vector.embeddings]`, named endpoints in `[vector.embeddings.servers.<name>]`, embedding schedule in `[vector.embed]` — see [Semantic Search](/semantic-search/#enabling-vector) for every key |
| `[[remote_hosts]]`                  | Remote machines synced by a bare `agentsview sync` — see [CLI Reference](/commands/#agentsview-sync)         |
| `[automated]`                       | Custom automated-session patterns — see [Automated Session Detection](#automated-session-detection)          |
| `[custom_model_pricing]`            | Per-model price overrides for usage reports — see [Custom Model Pricing](/token-usage/#custom-model-pricing) |

The `cursor_secret` is generated automatically on first run. For Gist
publishing, AgentsView first uses a saved `github_token`. For local browser
requests, if no token is saved, it then tries `AGENTSVIEW_GITHUB_TOKEN` and then
`gh auth token` from the GitHub CLI. Local users usually only need to run
`gh auth login`. For remote or proxied access, save a `github_token` via the web
UI Settings page or the API endpoint `POST /api/v1/config/github` when you want
AgentsView to publish gists. Remote access fields can be configured via the
Settings page or CLI flags — see [Remote Access](/remote-access/) for details.

When `require_auth` is enabled, the browser login prompt accepts the configured
`auth_token`. The value can come from `~/.agentsview/config.toml` or from the
`AGENTSVIEW_AUTH_TOKEN` environment variable; the environment variable wins when
both are set.

!!! note

    Older configs may still contain `remote_access = true`. AgentsView still
    reads that legacy key for backward compatibility, but new setups should use
    `require_auth = true`.

## Remote Hosts

Add `[[remote_hosts]]` entries when a bare `agentsview sync` should pull raw
session files from other machines after the local sync finishes. SSH remains the
default transport:

```toml
[[remote_hosts]]
host = "buildbox"
transport = "ssh" # optional; default
user = "wes"
port = 2222
```

For daemon-backed HTTP sync, run an AgentsView daemon on the remote host and
secure reachability with a private network such as Tailscale:

```toml
[[remote_hosts]]
host = "devbox1"
transport = "http"
url = "http://devbox1.tailnet.ts.net:8080"
token = "remote-token"
interval = "5m" # optional; zero or omitted means manual sync only
```

HTTP remote sync calls the remote daemon's archive endpoints and always uses a
bearer token, even when the rest of that daemon has `require_auth = false`. The
per-host `token` is required and must match the remote daemon's `auth_token`.
Do not reuse the collector daemon's own `auth_token` for untrusted remote
endpoints. HTTP transfers use a persistent per-host mirror and request file
deltas when fewer than half of the manifest files need fetching; see
[Remote Access — Incremental Sync](/remote-access/#incremental-sync).

Each `remote_hosts.host` value must be unique and stable. It namespaces imported
session IDs, the database skip cache, and the persistent mirror; changing it for
the same machine can duplicate sessions, while reusing it for another machine
can reuse stale state. A configured HTTP host can be selected later with
`agentsview sync --host <name>`, but ad hoc HTTP remotes are not supported;
without a matching configured host, `--host` remains an SSH remote sync. HTTP
failures are summarized with actionable messages for common cases such as token
rejection, missing remote archive endpoints, connection refusal, DNS failures,
and timeouts.

Set `interval` to a positive duration such as `"5m"` to have a running
collector daemon sync that host periodically. Zero or omitted disables the
per-host schedule; manual `agentsview sync` still includes the host.

The remote daemon must also listen on an interface the collector can reach.
The server binds `127.0.0.1` by default, so set `host` in the remote
machine's config.toml (requires `require_auth = true`) or start it with
`serve --host`:

```toml
host = "0.0.0.0"
require_auth = true
```

Detached daemons started with `agentsview serve --background` exit after
`daemon_idle_timeout` when idle. Set it to zero on machines that should stay
available for HTTP remote sync:

```toml
daemon_idle_timeout = "0s"
```

Supervised daemons run under systemd, launchd, Docker, or a foreground shell do
not create the detached-daemon idle tracker, so they do not idle-exit regardless
of this setting.

## Cursor Admin Usage API

`agentsview usage cursor` imports Cursor Admin API usage events into the local
archive so Cursor's billed usage can appear in the Usage dashboard and
`agentsview usage daily` reports. Set the API key in
`~/.agentsview/config.toml`:

```toml
cursor_admin_api_key = "key_xxxxx"
cursor_admin_email = "you@example.com" # optional
cursor_admin_user_id = "152683922"     # optional
```

Environment variables take precedence over the config file:

```bash
export AGENTSVIEW_CURSOR_ADMIN_API_KEY=key_xxxxx
export AGENTSVIEW_CURSOR_ADMIN_EMAIL=you@example.com
export AGENTSVIEW_CURSOR_ADMIN_USER_ID=152683922
```

The legacy unprefixed names `CURSOR_ADMIN_API_KEY`, `CURSOR_ADMIN_EMAIL`, and
`CURSOR_ADMIN_USER_ID` are also accepted when the matching `AGENTSVIEW_`
variable is unset. The email and user ID values are default filters; pass
`--email` or `--user-id` to `agentsview usage cursor` to override them for one
import.

## Session Discovery

AgentsView auto-discovers session files from the following agent sources. Amp
support is deprecated because current Amp releases may keep full threads
server-side and leave only local stubs; historical local Amp thread JSON files
can still be parsed.

| Agent                 | Default Directory                                                                | File Format                                                                                                                     |
| --------------------- | -------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| Aider                 | No default; opt in with `AIDER_DIR` or `aider_dirs`                              | `.aider.chat.history.md` Markdown history files                                                                                 |
| Amp (deprecated)      | `~/.local/share/amp/threads/`                                                    | Historical local JSON thread files                                                                                              |
| Antigravity (IDE)     | `~/.gemini/antigravity/`                                                         | SQLite database per session                                                                                                     |
| Antigravity CLI       | `~/.gemini/antigravity-cli/`                                                     | SQLite `conversations/<uuid>.db`, `<uuid>.trajectory.json` sidecars, or encrypted `.pb` files plus `brain/` and `history.jsonl` |
| Claude Code           | `~/.claude/projects/`                                                            | JSONL per session                                                                                                               |
| OpenClaude            | `~/.openclaude/projects/`                                                        | JSONL per session                                                                                                               |
| Claude Cowork         | (platform-specific, see below)                                                   | Claude Desktop cowork sessions                                                                                                  |
| Codex                 | `~/.codex/sessions/` and `~/.codex/archived_sessions/`                           | JSONL per session                                                                                                               |
| Command Code          | `~/.commandcode/projects/`                                                       | JSONL per session, optional `.meta.json` sidecar                                                                                |
| Copilot CLI           | `~/.copilot/`                                                                    | JSONL per session under `session-state/`                                                                                        |
| Devin CLI             | `~/.local/share/devin/` (Linux), `~/Library/Application Support/devin/` (macOS)             | Local CLI data rooted at the directory that contains `cli/`; session data is discovered under `<root>/cli/...`                 |
| Cortex Code           | `~/.snowflake/cortex/conversations/`                                             | JSON / JSONL per session                                                                                                        |
| Cursor                | `~/.cursor/projects/`                                                            | JSONL or plain-text transcripts                                                                                                 |
| DeepSeek TUI          | `~/.codewhale/sessions/` and `~/.deepseek/sessions/`                             | JSON per session                                                                                                                |
| Forge                 | `~/.forge/`                                                                      | SQLite database (`.forge.db`)                                                                                                   |
| Gemini CLI            | `~/.gemini/`                                                                     | JSONL in `tmp/` subdirectory                                                                                                    |
| gptme                 | `~/.local/share/gptme/logs/`                                                     | JSONL logs                                                                                                                      |
| Hermes Agent          | `~/.hermes/sessions/`                                                            | JSONL / JSON per session                                                                                                        |
| iFlow                 | `~/.iflow/projects/`                                                             | JSONL per session                                                                                                               |
| Kilo                  | `~/.local/share/kilo/`                                                           | SQLite DB or `storage/` JSON files                                                                                              |
| Kimi                  | `~/.kimi/sessions/` and `~/.kimi-code/sessions/`                                 | JSONL per session                                                                                                               |
| Kiro CLI              | `~/.kiro/sessions/cli/` and `~/.local/share/kiro-cli/`                           | JSONL per session and SQLite database                                                                                           |
| Kiro IDE              | (platform-specific, see below)                                                   | JSON / chat files                                                                                                               |
| MiMoCode              | `~/.local/share/mimocode/`                                                       | SQLite DB or `storage/` JSON files                                                                                              |
| Mistral Vibe          | `~/.vibe/logs/session/`                                                          | Per-session `messages.jsonl` plus `meta.json`                                                                                   |
| OhMyPi                | `~/.omp/agent/sessions/`                                                         | JSONL per session                                                                                                               |
| OpenClaw              | `~/.openclaw/assets/static/agents/` and `~/.kimi_openclaw/assets/static/agents/` | JSONL per session                                                                                                               |
| OpenCode              | `~/.local/share/opencode/`                                                       | SQLite DB or `storage/` JSON files                                                                                              |
| OpenHands CLI         | `~/.openhands/conversations/`                                                    | Per-conversation `base_state.json` + `events/*.json`                                                                            |
| Pi                    | `~/.pi/agent/sessions/`                                                          | JSONL per session                                                                                                               |
| Piebald               | `~/.local/share/piebald/`                                                        | SQLite database (`app.db`)                                                                                                      |
| Posit Assistant       | `~/.posit/assistant/workspaces/`                                                 | Per-conversation `conversation.json` tree plus `lm-messages.jsonl` transcript                                                   |
| Positron Assistant    | (platform-specific, see below)                                                   | JSON / JSONL per session                                                                                                        |
| QClaw                 | `~/.qclaw/assets/static/agents/`                                                 | JSONL per session                                                                                                               |
| Qoder                 | `~/.qoder/projects/` and `~/.qoderwork/projects/`                                | JSONL project transcripts plus sidecar metadata                                                                                  |
| Qwen Code             | `~/.qwen/projects/`                                                              | JSONL per session                                                                                                               |
| QwenPaw               | `~/.copaw/workspaces/`                                                           | JSON session files                                                                                                              |
| Reasonix              | `~/.reasonix/` and `~/AppData/Roaming/reasonix/`                                 | JSONL sessions plus `.jsonl.meta` sidecars                                                                                      |
| Shelley               | `~/.config/shelley/`                                                             | SQLite database (`shelley.db`)                                                                                                  |
| Visual Studio Copilot | (platform-specific, see below)                                                   | Trace JSONL files                                                                                                               |
| VS Code Copilot       | (platform-specific, see below)                                                   | JSON / JSONL per session                                                                                                        |
| Windsurf              | (platform-specific, see below)                                                   | SQLite `workspaceStorage/<hash>/state.vscdb` workspace chat data                                                                |
| Warp                  | (platform-specific, see below)                                                   | SQLite database                                                                                                                 |
| WorkBuddy             | `~/.workbuddy/projects/`                                                         | JSONL per session                                                                                                               |
| ZCode                 | `~/.zcode/cli/db/` or `~/.zcode/cli/`                                            | SQLite database (`db.sqlite`) with usage rows                                                                                   |
| Zed                   | (platform-specific, see below)                                                   | SQLite database (`threads/threads.db`)                                                                                          |
| Zencoder              | `~/.zencoder/sessions/`                                                          | JSONL per session                                                                                                               |

**VS Code Copilot default directories** vary by platform:

- **macOS:** `~/Library/Application Support/Code/User/`
- **Linux:** `~/.config/Code/User/`
- **Windows:** `%APPDATA%/Code/User/`

Code Insiders and VSCodium variants are also discovered automatically.

**Visual Studio Copilot default directories** vary by platform:

- **macOS:** `~/Library/Caches/VSGitHubCopilotLogs/traces/`
- **Linux:** `~/.cache/VSGitHubCopilotLogs/traces/`
- **Windows:** `%LOCALAPPDATA%/Temp/VSGitHubCopilotLogs/traces/`

This is separate from VS Code Copilot. Visual Studio Copilot stores trace files
named like `*_VSGitHubCopilot_traces.jsonl`; set `VISUALSTUDIO_COPILOT_DIR` or
`visualstudio_copilot_dirs` if your installation writes them elsewhere.

**Windsurf default directories** vary by platform:

- **macOS:** `~/Library/Application Support/Windsurf/User/` and
  `~/Library/Application Support/Windsurf - Next/User/`
- **Linux:** `~/.config/Windsurf/User/` and
  `~/.config/Windsurf - Next/User/`
- **Windows:** `%APPDATA%/Windsurf/User/` and
  `%APPDATA%/Windsurf - Next/User/`

Windsurf stores workspace chats in `workspaceStorage/<hash>/state.vscdb`.
AgentsView watches the `workspaceStorage` subtree and reads chat records from
that SQLite database. Set `WINDSURF_DIR` or `windsurf_dirs` if your user-data
directory is somewhere else.

**Positron Assistant default directory** (macOS only):

- **macOS:** `~/Library/Application Support/Positron/User/`

Positron is an IDE built on VS Code, so sessions use the same
`workspaceStorage/<hash>/chatSessions/` layout as VS Code Copilot. As of
v0.20.0, Positron Assistant has a built-in default path only on macOS — on Linux
and Windows, set `POSITRON_DIR` or `positron_dirs` to point at your Positron
user directory (for example, `~/.config/Positron/User` on Linux or
`%APPDATA%\Positron\User` on Windows).

**Posit Assistant** (posit-dev/assistant, also known as Databot) is a separate
product from the Positron IDE's built-in Assistant above. It stores one
directory per conversation under
`~/.posit/assistant/workspaces/<workspaceId>/<conversationId>/`, containing a
`conversation.json` message tree and an append-only `lm-messages.jsonl`
transcript; subagent runs nest under a `subagents/` subdirectory of their
parent conversation. All Posit Assistant hosts (Positron/VS Code extension,
standalone, desktop, TUI) share this location. Set `POSIT_ASSISTANT_DIR` or
`posit_assistant_dirs` if your installation stores its workspaces elsewhere.

**Claude Cowork default directories** follow Claude Desktop's Electron user-data
location:

- **macOS:** `~/Library/Application Support/Claude/local-agent-mode-sessions/`
- **Linux:** `~/.config/Claude/local-agent-mode-sessions/`
- **Windows:**
  `%LOCALAPPDATA%\Packages\Claude_pzs8sxrjxfjjc\LocalCache\Roaming\Claude\local-agent-mode-sessions\`
  or `%APPDATA%\Claude\local-agent-mode-sessions\`

Set `COWORK_DIR` or `cowork_dirs` when Claude Desktop stores local-agent-mode
sessions somewhere else.

**OpenHands CLI shallow watch:** OpenHands stores each conversation in its own
subdirectory, which would consume one recursive file watch per session and can
exhaust inotify limits on Linux. AgentsView watches the root
`~/.openhands/conversations/` directory non-recursively and relies on the
15-minute periodic sync to pick up changes inside existing conversations. New
conversation directories are still detected immediately. The server's startup
log reports how many directories are watched this way:

```
Watching 74 directories for changes (2 shallow) (76ms)
```

**Devin CLI root:** Point `DEVIN_DIR` or `devin_dirs` at the local root that
contains Devin's `cli/` directory, not at copied config or OAuth files. The
default roots are `~/Library/Application Support/devin` on macOS and
`~/.local/share/devin` on Linux, and AgentsView discovers session data under
`<root>/cli/...`. When sharing a path publicly, redact parent directories and
keep only the relevant tail, for example `.../Application Support/devin` or
`.../.local/share/devin`.

AgentsView intentionally ignores copied config/OAuth locations because those
paths are not the session archive source and may contain sensitive account
material. When filing bugs, share only the redacted local-share root and
directory shape, never pasted tokens, OAuth files, or other secrets.

**OpenCode storage backend:** As of 0.24.0, AgentsView reads both of OpenCode's
layouts. If a `storage/session/` directory exists under the OpenCode root,
sessions are parsed from the per-file JSON layout (`storage/session`,
`storage/message`, `storage/part`); otherwise the legacy `opencode.db` SQLite
file is used. Detection is automatic and requires no configuration. In storage
mode, the file watcher scopes itself to the `storage/` subtree rather than the
entire OpenCode directory, so unrelated OpenCode state like binaries, logs, and
caches no longer trigger sync events. In SQLite mode, it watches the
`opencode.db` parent.

Kilo and MiMoCode use the same OpenCode-format storage reader. Kilo reads from
`storage/session`, while MiMoCode reads from `storage/session_diff` when
present; both fall back to their SQLite databases when the file-backed storage
layout is absent.

**aider discovery:** aider writes one `.aider.chat.history.md` file per
repository instead of a central session directory. AgentsView does not scan for
Aider logs unless you opt in with `AIDER_DIR` or `aider_dirs`. Always-on
home-directory discovery has caused unwanted macOS privacy prompts from
background refreshes, so Aider discovery is limited to roots you explicitly
configure. On macOS, broad home roots still skip protected top-level folders
unless one of those folders is configured directly.

**Warp default directories** vary by platform:

- **macOS:**
  `~/Library/Group Containers/2BBY89MBSN.dev.warp/Library/Application Support/dev.warp.Warp-Stable/`
- **Linux:** `~/.local/state/warp-terminal/`
- **Windows:** `~/AppData/Local/warp/Warp/data/`

**Zed default directories** vary by platform:

- **macOS:** `~/Library/Application Support/Zed/`
- **Linux:** `~/.local/share/zed/`
- **Windows:** `~/AppData/Local/Zed/`

Zed stores all assistant threads in a single `threads/threads.db` SQLite
database under its data directory. AgentsView reads it directly, including model
names and per-request token usage.

**Kiro IDE default directories** vary by platform:

- **macOS:**
  `~/Library/Application Support/Kiro/User/globalStorage/kiro.kiroagent/`
- **Linux:** `~/.config/Kiro/User/globalStorage/kiro.kiroagent/`
- **Windows:** `~/AppData/Roaming/Kiro/User/globalStorage/kiro.kiroagent/`

**Antigravity CLI transcript sources:** Antigravity CLI has used both SQLite
databases and AES-encrypted `.pb` files. AgentsView reads whichever source is
richest, in this order:

1. **Decrypted trajectory sidecar.** For either format, if a
   `<uuid>.trajectory.json` file sits next to the source `.db`
   or `.pb` file (under `conversations/` or `implicit/`) and
   covers the session, AgentsView uses it as the source of
   truth for the full structured transcript — messages, tool
   calls, tool results, reasoning, and diffs. This is the
   highest-fidelity source for both formats. These sidecars are
   written out-of-process by
   [agy-reader](https://github.com/mjacobs/agy-reader), which
   performs the decryption; AgentsView reads the resulting
   plain JSON as untrusted input and needs no
   `ANTIGRAVITY_KEY` in this mode.
2. **SQLite trajectory database.** Newer Antigravity CLI
   releases write `conversations/<uuid>.db`. Without a covering
   sidecar (above), AgentsView opens the database read-only and
   decodes the trajectory steps directly. This direct decode is
   heuristic: it recovers prompts and tool-call names but not
   full structured tool results, reasoning, or diffs — a
   degraded **summary mode** transcript. If both
   `conversations/<uuid>.db` and `conversations/<uuid>.pb`
   exist, the SQLite database wins. Change detection also
   factors in `<uuid>.db-wal` and `<uuid>.db-shm` so active
   sessions resync as SQLite sidecar files move.
3. **In-process `.pb` decryption.** With no sidecar present,
   set `ANTIGRAVITY_KEY` (base64-encoded AES key, 16/24/32
   bytes after decoding) before starting AgentsView and it
   decrypts the `.pb` payloads itself, mirroring the upstream
   Python tool
   [`antigravity_decryptor`](https://github.com/arashz/antigravity_decryptor).
1. **Plaintext summary mode.** Otherwise AgentsView reads only `history.jsonl`
   and the `brain/` summaries — enough to populate session metadata and a
   high-level transcript.

Any session not backed by a covering sidecar — heuristic `.db`
decode, in-process `.pb` decryption, or plaintext summary mode —
shows a "Summary mode" badge in the detail header. Install
`agy-reader` when you want high-resolution transcripts for `.db`
and `.pb` sessions alike:

```bash
go install github.com/mjacobs/agy-reader@latest
agy-reader --sync
agy-reader --watch
```

Override any default with an environment variable (single directory). For Aider,
this opt-in is required because there is no default discovery root:

```bash
export AIDER_DIR=~/code
export AMP_DIR=~/custom/amp # historical local Amp threads only
export ANTIGRAVITY_DIR=~/custom/antigravity
export ANTIGRAVITY_CLI_DIR=~/custom/antigravity-cli
export CLAUDE_PROJECTS_DIR=~/custom/claude
export OPENCLAUDE_PROJECTS_DIR=~/custom/openclaude/projects
export OPENCLAUDE_CONFIG_DIR=~/custom/openclaude
export COWORK_DIR=~/custom/cowork
export CODEX_SESSIONS_DIR=~/custom/codex
export COMMANDCODE_PROJECTS_DIR=~/custom/commandcode
export COPILOT_DIR=~/custom/copilot
export DEVIN_DIR=~/Library/Application\ Support/devin
export CORTEX_DIR=~/custom/cortex
export CURSOR_PROJECTS_DIR=~/custom/cursor
export DEEPSEEK_TUI_SESSIONS_DIR=~/custom/deepseek/sessions
export FORGE_DIR=~/custom/forge
export GEMINI_DIR=~/custom/gemini
export GPTME_DIR=~/custom/gptme/logs
export HERMES_SESSIONS_DIR=~/custom/hermes
export IFLOW_DIR=~/custom/iflow
export KILO_DIR=~/custom/kilo
export KIMI_DIR=~/custom/kimi
export KIRO_SESSIONS_DIR=~/custom/kiro
export KIRO_IDE_DIR=~/custom/kiro-ide
export MIMOCODE_DIR=~/custom/mimocode
export VIBE_SESSIONS_DIR=~/custom/vibe/logs/session
export OMP_DIR=~/custom/omp
export OPENCLAW_DIR=~/custom/openclaw
export OPENCODE_DIR=~/custom/opencode
export OPENHANDS_CONVERSATIONS_DIR=~/custom/openhands
export PI_DIR=~/custom/pi
export PIEBALD_DIR=~/custom/piebald
export POSIT_ASSISTANT_DIR=~/custom/posit-assistant/workspaces
export POSITRON_DIR=~/custom/positron
export QCLAW_DIR=~/custom/qclaw
export QODER_PROJECTS_DIR=~/custom/qoder/projects
export QWEN_PROJECTS_DIR=~/custom/qwen
export QWENPAW_DIR=~/custom/qwenpaw
export REASONIX_DIR=~/custom/reasonix
export SHELLEY_DIR=~/custom/shelley
export VISUALSTUDIO_COPILOT_DIR=~/custom/visualstudio-copilot/traces
export VSCODE_COPILOT_DIR=~/custom/vscode
export WINDSURF_DIR=~/custom/windsurf/User
export WARP_DIR=~/custom/warp
export WORKBUDDY_PROJECTS_DIR=~/custom/workbuddy
export ZCODE_DIR=~/custom/zcode/cli
export ZED_DIR=~/custom/zed
export ZENCODER_DIR=~/custom/zencoder
```

### Multiple Directories

To scan more than one directory per agent — for example, when running Windows
and WSL side by side — add array fields to `~/.agentsview/config.toml`:

```toml
claude_project_dirs = [
  "~/.claude/projects",
  "/mnt/c/Users/you/.claude/projects",
]

codex_sessions_dirs = [
  "~/.codex/sessions",
]
```

The corresponding fields are `aider_dirs`, `amp_dirs`, `antigravity_dirs`,
`antigravity_cli_dirs`, `claude_project_dirs`, `openclaude_project_dirs`,
`cowork_dirs`, `devin_dirs`,
`codex_sessions_dirs`, `commandcode_project_dirs`, `copilot_dirs`,
`cortex_dirs`, `cursor_project_dirs`, `deepseek_tui_sessions_dirs`,
`forge_dirs`, `gemini_dirs`, `gptme_dirs`, `hermes_sessions_dirs`, `iflow_dirs`,
`kilo_dirs`, `kimi_dirs`, `kiro_dirs`, `kiro_ide_dirs`, `mimocode_dirs`,
`vibe_session_dirs`, `omp_dirs`, `openclaw_dirs`, `opencode_dirs`,
`openhands_dirs`, `pi_dirs`, `piebald_dirs`, `posit_assistant_dirs`,
`positron_dirs`, `qclaw_dirs`, `qoder_project_dirs`, `qwen_project_dirs`,
`qwenpaw_dirs`, `reasonix_dirs`, `shelley_dirs`, `visualstudio_copilot_dirs`,
`vscode_copilot_dirs`, `windsurf_dirs`, `warp_dirs`,
`workbuddy_project_dirs`, `zcode_dirs`, `zed_dirs`, and `zencoder_dirs`. Each
accepts an array of paths. When set, these take precedence over the
single-directory environment variable and the default path.

All listed directories are discovered, watched, and synced independently.

### S3-Compatible Session Sources

Claude and Codex session roots can also be `s3://` URIs. This is useful when
several machines push their raw session files to object storage and one central
AgentsView instance reads them without SSH access to those machines.

```toml
claude_project_dirs = [
  "~/.claude/projects",
  "s3://agent-archive/laptop/raw/claude",
]

codex_sessions_dirs = [
  "~/.codex/sessions",
  "s3://agent-archive/laptop/raw/codex",
]
```

S3 sources are read-only inputs to the normal local sync. AgentsView lists
matching objects, fetches each changed object to a temporary file, parses it
with the existing Claude/Codex parser, records the original `s3://` URI as
`file_path`, and removes the temporary file. No persistent local mirror is
created.

Credentials and endpoint configuration use standard AWS-style environment
variables:

```bash
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-east-1
export AWS_S3_ENDPOINT=https://s3.amazonaws.com
```

`AWS_S3_ENDPOINT` is optional for AWS S3. Set it for S3-compatible services such
as MinIO, Aliyun OSS, or Cloudflare R2. `http://` endpoints are accepted only
for loopback hosts such as `localhost` or `127.0.0.1`. For a non-loopback HTTP
endpoint, set `AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT=true`; use that override
only for trusted private networks because session transcripts travel without
TLS.

Expected object layouts:

```text
s3://bucket/.../<machine>/raw/claude/<project>/<uuid>.jsonl
s3://bucket/.../<machine>/raw/claude/<project>/subagents/.../agent-*.jsonl
s3://bucket/.../<machine>/raw/codex/2026/06/24/rollout-*.jsonl
```

The machine name is derived from the path segment immediately before `raw`. If
no such segment exists, sessions use the local AgentsView machine label. Codex
discovery only imports rollout files under the configured root plus a trailing
slash, so sibling prefixes such as `raw/codex-backup` are ignored.

S3 object `Size`, `LastModified`, and available object fingerprints (`ETag`,
version ID, and checksum headers) are stored in the session row and used for
unchanged-object skip checks. A later sync therefore lists object metadata first
and downloads only objects whose source metadata changed or whose stored parser
data is stale.

S3 roots are not watched with fsnotify. They are picked up by initial sync,
manual sync, and the periodic directory scan.

### Worktree Project Mappings

The parser infers a session's project from its `cwd`, which works for standard
layouts but not custom worktree conventions like
`~/code/{project}.worktrees/feat/<branch>/` — those sessions otherwise group
under `<branch>` rather than `{project}`. As of 0.29.0, you can register manual
**path-prefix → project** rules from the **Worktree Project Mappings** section
in Settings:

![Worktree Project Mappings settings section](/assets/generated/screenshots/worktree-mappings.png)

- Mappings are explicit; there is no auto-discovery.
- Each rule applies whenever a session's `cwd` falls under the configured
  prefix, on both new sessions as they sync and (via the **Apply** button)
  already-imported sessions.
- The default `explicit` layout maps every matching path to the project name
  stored on the rule. The `repo_dot_worktrees` layout derives the project from
  the first path segment under the prefix when it is named
  `<repo>.worktrees`, so a path like
  `/code/agentsview.worktrees/feature/frontend` resolves to project
  `agentsview`.
- Rules are stored in a `worktree_project_mappings` SQLite table scoped to the
  host machine, so a mapping created on one machine does not leak into another
  machine's view of synced sessions.
- Excluded, trashed, and skipped session files are left alone.

Mappings only mutate the session's `project` field; the rest of the session
record is preserved through the bulk-resync rebuild-and-copy path.

## Automated Session Detection

AgentsView classifies a session as "automated" when it has one or fewer real
user messages and its first user message matches the automation classifier.
Automated sessions (roborev reviews, title generation, warmup pings, changelog
generation, and similar scripted runs) are filtered out of session lists,
counts, and analytics by default — the **Include automated** toggle in the
session filter dropdown opts them back in.

A set of built-in patterns covers the roborev family and AgentsView's own
internal prompts. To teach AgentsView about first-message patterns unique to
your own automation, add them to `~/.agentsview/config.toml`:

```toml
[automated]
prefixes = [
  "You are summarizing a nightly batch run.",
  "INTERNAL-AUTOMATION:",
]
substrings = [
  "This is an automated repository maintenance run.",
]
exact_matches = [
  "Nightly automation completed.",
]
```

User-configured entries are case-sensitive and are matched against the session's
first user message:

| Key             | Match behavior                                               |
| --------------- | ------------------------------------------------------------ |
| `prefixes`      | `HasPrefix` against the first user message                   |
| `substrings`    | `Contains` anywhere in the first user message                |
| `exact_matches` | trims the first user message, then compares the whole string |

Entries are trimmed, deduplicated, and capped at 1024 characters. Entries that
duplicate a built-in pattern in the same category are silently dropped.

**Reclassification on config change.** AgentsView stores a hash of the active
classifier (built-in patterns + your configured patterns) with the database. On
startup, it rechecks stored `is_automated` values against the active classifier
and re-stamps the hash, so edits to `[automated]` patterns apply to history
immediately — no manual resync required. The same backfill also corrects rows
pulled in from PostgreSQL sync or copied from other archives.

## Database

The SQLite database uses WAL mode for concurrent reads and includes FTS5
full-text search indexes on message content.

**Schema tables:**

| Table                | Purpose                                                                      |
| -------------------- | ---------------------------------------------------------------------------- |
| `sessions`           | Session metadata (project, agent, timestamps, file info, user message count) |
| `messages`           | Message content with role, ordinal, timestamps                               |
| `tool_calls`         | Tool invocations with normalized category taxonomy                           |
| `tool_result_events` | Chronological status events for tool calls (e.g. Codex subagent updates)     |
| `insights`           | AI-generated session analysis and summaries                                  |
| `starred_sessions`   | Server-side star persistence (replaces localStorage)                         |
| `pinned_messages`    | Pinned message references with session linkage                               |
| `stats`              | Aggregate counts (session_count, message_count)                              |
| `skipped_files`      | Cache of non-interactive session files                                       |
| `messages_fts`       | FTS5 virtual table for full-text search                                      |

The database is automatically migrated on startup when the schema changes. When
the stored data version is stale, AgentsView preserves the existing database and
runs a full resync into a fresh temporary database. The resync then copies
preserved/orphaned session data from the previous database before swapping
atomically. If the full resync aborts, AgentsView falls back to an incremental
sync and leaves the data-version marker stale so a later startup can retry the
full rewrite.

## Sync Behavior

AgentsView keeps the database in sync with session files through two mechanisms:

1. **File watcher** — uses fsnotify to detect file changes in real time (500ms
   debounce). Common dependency and build folders (`node_modules`,
   `__pycache__`, `.git`, `vendor`, `dist`, etc.) are automatically skipped to
   reduce noise and overhead.
1. **Periodic sync** — full directory scan every 15 minutes as a safety net

Change detection uses file size, mtime, inode, and device tracking to validate
incremental parses more reliably. A pool of 8 workers processes files in
parallel during sync.

For `s3://` Claude and Codex roots, change detection uses object size,
`LastModified`, and available object fingerprints such as ETag, version ID, and
checksums from listing or stat calls. Object content is downloaded only after
that metadata shows a parse may be needed.

Files that fail to parse or contain no interactive content are cached in the
`skipped_files` table and skipped on subsequent syncs until their mtime changes.

Sync summaries include a `Parser anomalies (this run)` section whenever the
current run observes parser or sanitizer anomalies. The section can include
malformed-line counts, unrecognized Antigravity schema sessions,
sanitized-field counts, and Antigravity `gen_metadata without usage` counts. A
`gen_metadata without usage` entry means Antigravity supplied generation
metadata for one or more records, but AgentsView could not derive normalized
usage totals from those records during that sync.

When a data-version resync runs, startup output prints durable phase and
completion lines for the resync steps. Background daemons also publish startup
state while they hold the start lock, so `agentsview serve status` can show the
starting PID, elapsed time, current phase, progress detail, and log path before
the HTTP server is ready.

### Restricting Ingestion by Working Directory

By default every discovered session is ingested. To limit the archive to
sessions from specific workspaces — for example on a machine shared across
multiple clients where transcripts from one workspace should never appear
alongside another — set `sync_include_cwd_prefixes` in
`~/.agentsview/config.toml`:

```toml
sync_include_cwd_prefixes = [
  "/home/me/work/client-a",
  "/home/me/oss",
]
```

When the list is non-empty, a session is ingested only if its recorded
working directory equals one of the prefixes or lives underneath one.
Prefixes and session directories are lexically cleaned before matching:
trailing separators are ignored and `..` components are resolved, so
`/home/me/oss/../other` does not match a `/home/me/oss` prefix. Matching
is path-boundary aware (`/home/me/oss` matches `/home/me/oss/repo` but
not `/home/me/oss-other`), case-sensitive, and uses the local operating
system's path separator — on Linux and macOS a backslash is an ordinary
filename character, not a directory boundary. Use absolute paths; `~` is
not expanded.

Notes:

- Sessions without a recorded working directory (a few agents do not store
  one) are skipped while the filter is set.
- The filter gates ingestion only. Sessions already in the archive are
  preserved (the SQLite database is a persistent archive); remove unwanted
  existing sessions explicitly with `agentsview prune`.
- Remote-host sync is unaffected: the prefixes describe local paths, so they
  are not applied to sessions pulled from `[[remote_hosts]]` entries.

### Large Watch Trees

The recursive watcher has a hard budget of 8192 directories per process. If a
session root is larger than the remaining budget, or if registering watches hits
the operating system's inotify or file-descriptor limit (`ENOSPC` / `EMFILE`),
as of 0.27.0 AgentsView **degrades** that root to polling instead of aborting
startup. The HTTP listener is now bound before any watches are registered, so
the server still comes up cleanly.

Roots that fall back to polling are picked up by:

- the existing 15-minute periodic full sync, plus
- a new 2-minute fallback sync loop that runs whenever any roots are unwatched
  (it re-syncs all configured roots, not just the unwatched ones)

Startup logs make degradation explicit. Per-root and summary lines look like:

```
Couldn't watch 12500 directories under /home/me/.claude/projects, will poll every 2m0s
Polling 1 roots every 2m0s for changes
```

No configuration is required, but on Linux you can still raise the global cap to
keep more roots watched in real time:

```bash
sudo sysctl fs.inotify.max_user_watches=524288
```

## Manual Sync

Trigger a sync from the API:

```bash
curl -X POST http://127.0.0.1:8080/api/v1/sync
```

Trigger a full resync (re-parses all session files from scratch):

```bash
curl -X POST http://127.0.0.1:8080/api/v1/resync
```

Both endpoints stream progress via Server-Sent Events when accessed from a
browser or SSE-capable client.

Check sync status:

```bash
curl http://127.0.0.1:8080/api/v1/sync/status
```

## Privacy and Telemetry

By default, all session data stays on your local machine in SQLite. AgentsView
never sends session content, project names, prompts, file paths, or hostnames
anywhere.

Optional features that send data externally when you enable them:

- [PostgreSQL sync](/pg-sync/) (`pg push`) sends session data to a PostgreSQL
  database you configure.
- The [DuckDB mirror](/duckdb/) writes a local DuckDB file by default; data only
  leaves the machine if you expose the mirror over a remote Quack endpoint.
- [Session Insights](/insights/) sends session content to an AI provider
  (Claude, Codex, Copilot, or Gemini) to generate summaries.
- [Publish to Gist](/usage/#publish-to-gist) uploads a session to GitHub.

The automatic outbound requests are update checks and an anonymous daemon ping:

- **CLI and web UI** — on startup, the server contacts the GitHub API to check
  for new releases. No identifying information is sent beyond what a standard
  GitHub API request includes (IP address, user-agent).
- **Desktop app** — uses Tauri's native updater, which checks the GitHub release
  feed independently.
- **Anonymous daemon telemetry** — see below.

### Anonymous Daemon Telemetry

As of 0.33.0, the server sends an anonymous `daemon_active` liveness ping on
startup and every 24 hours while running. The ping contains only:

- app version and git commit
- operating system and CPU architecture
- a random install ID, generated once and stored in
  `~/.agentsview/telemetry-install-id`

It contains no session data, prompts, project names, file paths, account
information, or hostname, and the events are sent with person-profile processing
and GeoIP lookup disabled. The ping runs in the background and never blocks
startup or operation.

Disable it with an environment variable:

```bash
export AGENTSVIEW_TELEMETRY_ENABLED=0
```

### Disabling Update Checks

Disable the CLI/web UI update check with any of:

| Method               | Value                                                        |
| -------------------- | ------------------------------------------------------------ |
| Config file          | `disable_update_check = true` in `~/.agentsview/config.toml` |
| Environment variable | `AGENTSVIEW_DISABLE_UPDATE_CHECK=1`                          |
| CLI flag             | `--no-update-check`                                          |

The desktop app's auto-updater is controlled separately via
`AGENTSVIEW_DESKTOP_AUTOUPDATE=0`.
