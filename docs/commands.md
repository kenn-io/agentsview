---
title: CLI Reference
description: All AgentsView commands, flags, and environment variables
---

## Commands

### `agentsview serve`

Start the HTTP server with embedded web UI.

```bash
agentsview serve [flags]
```

As of 0.23.0, starting the server requires the explicit
`serve` subcommand. Running plain `agentsview` shows help
instead of starting the web UI.

| Flag | Default | Description |
|------|---------|-------------|
| `--host` | `127.0.0.1` | Host to bind to |
| `--port` | `8080` | Port to listen on |
| `--no-browser` | `false` | Don't open browser on startup |
| `--no-update-check` | `false` | Disable automatic update checks |
| `--background` | `false` | Start `agentsview serve` as a managed background process |
| `--public-url` | | Public URL for hostname or proxy access |
| `--public-origin` | | Trusted browser origin (repeatable/comma-separated) |
| `--proxy` | | Managed proxy mode (`caddy`) |
| `--caddy-bin` | `caddy` | Caddy binary path |
| `--proxy-bind-host` | `127.0.0.1` | Interface for managed proxy |
| `--public-port` | `8443` | External port for managed proxy |
| `--tls-cert` | | TLS certificate path |
| `--tls-key` | | TLS key path |
| `--allowed-subnet` | | Client CIDR allowlist (repeatable/comma-separated) |

The server auto-discovers an available port if `8080` is busy.
See [Remote Access](/remote-access/) for details on the remote
access and proxy flags.

**Examples:**

```bash
agentsview serve                                # defaults
agentsview serve --port 9090                    # custom port
agentsview serve --no-browser                   # disable browser auto-open
agentsview serve --background                   # start managed background server
agentsview serve --public-url https://agents.example.com
```

On startup, the server:

1. Loads or creates `~/.agentsview/sessions.db`
2. Runs initial sync across all discovered session directories
3. Starts the file watcher (500ms debounce)
4. Starts periodic sync (every 15 minutes)
5. Serves the Svelte SPA and REST API

The server shuts down cleanly on `Ctrl+C`, flushing the
database and stopping file watchers.

#### Background Mode

Use `--background` when you want the web UI to keep running after
the launching shell exits:

```bash
agentsview serve --background
agentsview serve status
agentsview serve stop
```

The parent command starts a detached `agentsview serve` process,
waits briefly for it to publish its runtime record, and prints the
URL, PID, and log path. Background server output is written to
`~/.agentsview/serve.log`. `serve status` reports the managed
process, URL, version, uptime, and read-only mode when available.
`serve stop` gracefully terminates the managed process and cleans
up its runtime record.

Background servers also act as the shared local daemon for the
desktop app and CLI. The daemon owns local SQLite writes for its
data directory, so common write and freshness-sensitive commands
proxy to it instead of opening the archive as a second writer. A
background daemon self-exits after the idle timeout when no
external request or daemon-owned job is active. Periodic sync and
file-watcher work prevent exit while they run, but do not keep the
daemon alive forever by themselves.

#### CLI daemon behavior

Most read-only CLI commands do not auto-start the daemon on a cold
archive. They attach to a compatible local daemon when one is
already running; otherwise they open SQLite directly in read-only
mode and return the latest indexed data. This keeps commands such
as `session list`, `session get`, `session messages`, and offline
usage reports fast in scripts.

Commands that need fresh data or need to write auto-start the
detached daemon when no compatible daemon is running. That includes
local `sync`, `session sync`, `token-use`, normal `usage` refresh
paths, `pg push`/`pg push --watch`, and `duckdb push`. If a writable
daemon is known to own the archive but is not reachable, these
commands refuse instead of writing directly.

Set `AGENTSVIEW_NO_DAEMON=1` to disable daemon auto-start. With
that escape hatch, read commands use direct read-only SQLite and
write commands acquire the local write-owner lock before opening
SQLite. If another process owns that lock, the command refuses and
asks you to stop the daemon, wait for idle shutdown, or retry after
the offline operation finishes.

---

### `agentsview sync`

Refresh the local archive. For local sync, the CLI uses the
running daemon or starts a detached daemon so SQLite writes stay
owned by one process. Set `AGENTSVIEW_NO_DAEMON=1` to force a
direct offline sync that acquires the local write-owner lock and
exits when done.

```bash
agentsview sync [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--full` | `false` | Force a full resync regardless of data version |
| `--host` | | SSH hostname for remote sync |
| `--user` | | SSH username for remote sync |
| `--port` | `22` | SSH port for remote sync |

**Examples:**

```bash
agentsview sync           # incremental sync and exit
agentsview sync --full    # full resync and exit
agentsview sync --host buildbox.local
agentsview sync --host buildbox.local --user wes --port 2222
```

After syncing, a summary of session and message counts is printed
to stdout.

When `--host` is set, AgentsView performs a remote sync over SSH:
it resolves the supported agent session directories on the remote
machine, transfers the source session data locally, and indexes it
into your local archive.

Local sync can also read configured Claude and Codex roots from
S3-compatible object storage. Add `s3://` entries to
`claude_project_dirs` or `codex_sessions_dirs` in
`~/.agentsview/config.toml`, then run `agentsview sync` normally.
This is not SSH remote sync: object storage is treated as a read-only
session source, using object size and `LastModified` metadata to skip
unchanged sessions and downloading only objects that need parsing. See
[Configuration — S3-Compatible Session Sources](/configuration/#s3-compatible-session-sources).

#### Configured Remote Hosts

As of 0.33.0, remote hosts can also be declared in
`~/.agentsview/config.toml` so a single bare `agentsview sync`
covers a whole fleet:

```toml
[[remote_hosts]]
host = "buildbox.local"
user = "wes"      # optional
port = 2222       # optional, defaults to 22

[[remote_hosts]]
host = "laptop2"
```

With hosts configured, `agentsview sync` (no `--host`) runs the
local sync first, then syncs each configured host over SSH in
the order declared. `--full` applies to every host. A failing
host is reported on stderr and skipped so the remaining hosts
still run; the command exits non-zero if any host failed.

`agentsview sync --host X` ignores the configured list and syncs
only that host, failing fast on error as before. Remote sync is
non-interactive in both forms — it requires key-based
passwordless SSH and never prompts for a password. Hosts must be
unique within the list, since remote sessions are namespaced by
host.

---

### `agentsview prune`

Delete sessions matching one or more filters. At least one filter
is required.

```bash
agentsview prune [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--project` | | Sessions whose project contains this substring |
| `--max-messages` | `-1` | Sessions with at most N messages |
| `--before` | | Sessions that ended before this date (`YYYY-MM-DD`) |
| `--first-message` | | Sessions whose first message starts with this text |
| `--dry-run` | `false` | Show what would be pruned without deleting |
| `--yes` | `false` | Skip confirmation prompt |

**Examples:**

```bash
# Preview what would be deleted
agentsview prune --project "scratch" --dry-run

# Delete short sessions from before 2025
agentsview prune --max-messages 2 --before 2025-01-01

# Delete sessions starting with a specific message
agentsview prune --first-message "test" --yes

# Combine filters (AND logic)
agentsview prune --project "old-project" --max-messages 5 --before 2025-06-01
```

The prune command displays the number of sessions deleted and
disk space reclaimed. Use `--dry-run` first to verify the filter
matches what you expect.

---

### `agentsview version`

Print the version, git commit, and build date.

```bash
agentsview version
```

```
agentsview 0.23.0 (commit d49f1a9, built 2026-04-19)
```

---

### `agentsview usage daily`

Report token usage and estimated cost aggregated by local-time
day, scoped to the last 30 days by default. See
[Token Usage & Costs](/token-usage/) for a full write-up,
including benchmarks against `ccusage`.

```bash
agentsview usage daily [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--json` | `false` | Emit JSON instead of a terminal table |
| `--since` | `30 days ago` | Start of window, a duration like `28d` or a `YYYY-MM-DD` date, inclusive |
| `--until` | | End of window, a duration like `28d` or a `YYYY-MM-DD` date, inclusive |
| `--all` | `false` | Scan all history; overrides the default 30-day window |
| `--agent` | | Filter by agent name |
| `--breakdown` | `false` | Show indented per-model rows under each day |
| `--offline` | `false` | Skip the LiteLLM pricing fetch; use embedded fallback |
| `--no-sync` | `false` | Skip the on-demand sync pass before querying |
| `--timezone` | system | IANA timezone name for date bucketing |

**Examples:**

```bash
agentsview usage daily                           # last 30 days
agentsview usage daily --all                     # full history
agentsview usage daily --since 14d               # last 14 days
agentsview usage daily --since 2026-04-01 --breakdown
agentsview usage daily --json --agent claude
```

---

### `agentsview usage statusline`

Print today's total estimated cost as a single line, for
shell prompts and tmux status lines.

```bash
agentsview usage statusline [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--agent` | | Filter by agent name |
| `--offline` | `false` | Use embedded fallback pricing only |
| `--no-sync` | `false` | Skip on-demand sync |

**Example:**

```bash
$ agentsview usage statusline
$9.61 today
```

See [Token Usage & Costs](/token-usage/#agentsview-usage-statusline)
for integration examples (Starship, tmux).

---

### `agentsview usage cursor`

Fetch Cursor Admin API usage events and store them in the local
archive so they contribute to the Usage dashboard and daily reports.

```bash
agentsview usage cursor [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--since` | `30 days ago` | Start date (`YYYY-MM-DD`), inclusive |
| `--until` | today | End date (`YYYY-MM-DD`), inclusive |
| `--all` | `false` | Include all history |
| `--page-size` | `100` | Events requested per Cursor API page |
| `--email` | config | Filter by Cursor team member email |
| `--user-id` | config | Filter by Cursor team member user ID |

**Examples:**

```bash
agentsview usage cursor
agentsview usage cursor --since 2026-05-01 --until 2026-05-31
agentsview usage cursor --all --email you@example.com
```

See [Cursor Admin Usage Events](/token-usage/#cursor-admin-usage-events)
for setup and reporting behavior.

---

### `agentsview activity report`

Report active time, concurrency, cost, token, breakdown, and session
rows for a resolved date range. The command uses the same report model
as the web UI's [Activity](/activity/) page.

```bash
agentsview activity report [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--preset` | | Range preset: `day`, `week`, `month`, or `custom` |
| `--date` | today | Anchor date for day/week/month presets (`YYYY-MM-DD`) |
| `--from` | | Start instant for custom range (RFC3339) |
| `--to` | | End instant for custom range (RFC3339) |
| `--timezone` | system | IANA timezone for range bucketing |
| `--bucket` | automatic | Bucket size: `5m`, `15m`, `1h`, `1d`, or `1w` |
| `--project` | | Filter by project |
| `--agent` | | Filter by agent name |
| `--machine` | | Filter by machine name |
| `--json` | `false` | Emit the full report as JSON |
| `--no-sync` | `false` | Skip on-demand sync before querying |
| `--offline` | `false` | Use fallback pricing only |

**Examples:**

```bash
agentsview activity report --preset day --date 2026-06-20
agentsview activity report --preset week --date 2026-06-20 --json
agentsview activity report --preset custom \
  --from 2026-06-20T14:00:00Z \
  --to 2026-06-20T18:00:00Z \
  --bucket 15m
```

The human output prints totals, peak concurrency, top project/model/
agent breakdowns, and top sessions. JSON output includes the dense
bucket timeline and session rows used by the web UI.

---

### `agentsview token-use`

!!! note "Deprecated"
    As of 0.30.0, `agentsview token-use` is a deprecated alias for
    [`agentsview session usage`](/session-api/#agentsview-session-usage).
    Both commands accept the same `<session-id>` argument. `token-use`
    always emits the same JSON shape that `session usage --format json`
    emits (now extended with a cost estimate). New scripts should use
    `agentsview session usage`.

Print machine-readable token usage data and a cost estimate for a
single session.

```bash
agentsview token-use <session-id>
```

Session ID format depends on the agent. For example, Claude
root sessions usually use UUIDs like
`550e8400-e29b-41d4-a716-446655440000`, Claude subagents use
IDs like `agent-a86574e`, and some other agents use prefixes
such as `codex:my-session-id`. Raw session IDs emitted by the
underlying agent are also accepted when AgentsView can resolve
them back to the canonical stored session.

If the AgentsView server is already running, the command reads the
current database state. If no server is running, it performs an
on-demand sync for the requested session first.

**Example:**

```bash
agentsview token-use 550e8400-e29b-41d4-a716-446655440000
```

```json
{
  "session_id": "550e8400-e29b-41d4-a716-446655440000",
  "agent": "claude",
  "project": "my-project",
  "total_output_tokens": 15230,
  "peak_context_tokens": 84000,
  "has_token_data": true,
  "cost_usd": 2.41,
  "has_cost": true,
  "models": ["claude-opus-4-7"],
  "server_running": false
}
```

See [`agentsview session usage`](/session-api/#agentsview-session-usage)
for the full field reference and exit-code contract.

---

### `agentsview pg push`

Sync sessions from local SQLite to PostgreSQL. See
[PostgreSQL Sync](/pg-sync/) for full documentation.

```bash
agentsview pg push [target] [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--full` | `false` | Force full local resync and re-push |
| `--projects` | | Comma-separated projects to push (inclusive) |
| `--exclude-projects` | | Comma-separated projects to exclude from push |
| `--all-projects` | `false` | Ignore configured project filters for this run |
| `--all` | `false` | Push every configured PostgreSQL target sequentially |
| `--watch` | `false` | Run continuously, pushing on change plus a periodic floor |
| `--debounce` | `30s` | Coalesce window after a change before pushing (`--watch` only) |
| `--interval` | `15m` | Periodic floor push interval (`--watch` only) |

See [PostgreSQL Sync — Project Filtering](/pg-sync/#project-filtering)
for details on how filtering interacts with the push watermark.

---

### `agentsview pg status`

Show PostgreSQL sync status.

```bash
agentsview pg status [target] [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--all` | `false` | Show status for every configured PostgreSQL target |
| `--projects` | | Comma-separated projects whose push status to show |
| `--exclude-projects` | | Comma-separated excluded projects whose push status to show |
| `--all-projects` | `false` | Ignore configured project filters for this status |

---

### `agentsview pg serve`

Start a read-only web UI backed by PostgreSQL. See
[PostgreSQL Sync](/pg-sync/) for full documentation.

```bash
agentsview pg serve [flags]
```

Accepts the same serve flags (`--host`, `--port`, `--proxy`, etc.)
plus PostgreSQL configuration from `config.toml`.

---

### `agentsview pg service`

Install and manage the PostgreSQL auto-push service, which runs
`agentsview pg push --watch` in the background. Supported service
managers are launchd on macOS and `systemd --user` on Linux.
See [PostgreSQL Sync — `agentsview pg service`](/pg-sync/#agentsview-pg-service)
for setup notes.

```bash
agentsview pg service install
agentsview pg service status
agentsview pg service logs [-f]
agentsview pg service start
agentsview pg service stop
agentsview pg service uninstall
```

| Command | Description |
|---------|-------------|
| `install` | Generate the service unit, enable it, and start it |
| `status` | Show the service-manager status plus last successful push |
| `logs -f` | Follow `pg-watch.log` under the AgentsView data directory |
| `start` | Start the installed service |
| `stop` | Stop the installed service |
| `uninstall` | Stop and remove the service unit |

---

### `agentsview duckdb`

Mirror the local SQLite archive into DuckDB and serve from it,
locally or over the Quack remote protocol. See
[DuckDB Mirror](/duckdb/) for full documentation.

```bash
agentsview duckdb push          # mirror SQLite into sessions.duckdb
agentsview duckdb status        # show mirror sync status
agentsview duckdb serve         # read-only web UI from the mirror
agentsview duckdb quack serve   # expose the mirror over Quack
```

`duckdb push` accepts the same `--full` / `--projects` /
`--exclude-projects` / `--all-projects` flags as `pg push`, and
`duckdb serve` accepts the same serve flags as `pg serve`. The
DuckDB backend is unavailable on Windows ARM64 (the upstream
bindings ship no prebuilt library for that platform); all other
commands work normally there.

---

### `agentsview projects`

List all projects in the local database with their session counts.

```bash
agentsview projects [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--json` | `false` | Output as a JSON array |

**Examples:**

```bash
agentsview projects         # tabular output
agentsview projects --json   # JSON array
```

---

### `agentsview health`

Inspect session intelligence in a human-friendly CLI view. See
[Session Intelligence](/session-intelligence/) for the scoring and
signal model.

```bash
agentsview health [session-id] [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--json` | `false` | Output JSON instead of terminal text |
| `--limit` | `20` | Number of sessions to list when no session ID is given |

**Examples:**

```bash
agentsview health
agentsview health --limit 50
agentsview health 550e8400-e29b-41d4-a716-446655440000
agentsview health agent-a86574e --json
```

Without a session ID, the command lists recent sessions with grade
and outcome columns. With a session ID, it prints the detailed
signal counts for that session.

---

### `agentsview stats`

Experimental window-scoped workspace analytics across sessions and
git activity. See [Stats](/stats/) for the full write-up.

```bash
agentsview stats [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--format` | `human` | Output format: `human` or `json` |
| `--since` | `28d` | Start of window, either `YYYY-MM-DD` or a compact duration like `28d` |
| `--until` | | End of window as `YYYY-MM-DD` |
| `--agent` | `all` | Restrict to one agent or use `all` |
| `--include-project` | | Repeatable project allowlist |
| `--exclude-project` | | Repeatable project blocklist |
| `--timezone` | local | Timezone used for temporal reporting |

**Examples:**

```bash
agentsview stats
agentsview stats --format json --since 2026-04-01 --until 2026-04-15
agentsview stats --agent claude --include-project agentsview
```

The command is experimental. The exact human output may change, and
the JSON output should be treated as a moving surface even though it
currently carries `schema_version: 1`.

---

### `agentsview doctor sync`

Collect read-only diagnostics for startup sync decisions. This command
does not create or migrate config files; it loads the current config
read-only, inspects the SQLite archive, checks configured agent roots,
and prints recent sync-related `debug.log` lines.

```bash
agentsview doctor sync
```

The report includes:

- data directory and SQLite database path
- whether the database exists and is readable
- SQLite `user_version` and the binary data version
- the startup sync decision
- session counts by stored data version
- leftover resync temp files
- configured/default agent roots and whether each exists
- recent debug lines mentioning sync, data versions, warnings, or
  failures
- a likely-cause summary when startup sync behavior looks abnormal

---

### `agentsview parse-diff`

Validate parser changes against the real session archive already in
your local SQLite database. The command re-parses source files with
the current binary, normalizes them through the sync path, and reports
field-level differences from the stored rows without writing updated
session data.

```bash
agentsview parse-diff [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--agent` | | Restrict to one agent; repeatable |
| `--limit` | `0` | Maximum number of source files to inspect, newest first (`0` means all) |
| `--fail-on-change` | `false` | Exit non-zero when changes or parse errors are found |
| `--json` | `false` | Emit a machine-readable report |
| `--verbose` / `-v` | `false` | Include more detail in the human report |

**Examples:**

```bash
agentsview parse-diff --agent claude --limit 100
agentsview parse-diff --agent codex --fail-on-change
agentsview parse-diff --json > parser-report.json
```

`parse-diff` is intended for parser development and release QA. Run
it against a quiescent, freshly synced archive for the clearest
signal. Import-only sources and non-file-backed agents are skipped
because there is no source file to re-parse.

---

### `agentsview import`

Import Claude.ai or ChatGPT conversations into the local
database. See [Chat Import](/chat-import/) for full
documentation.

```bash
agentsview import --type <type> <path>
```

| Flag | Default | Description |
|------|---------|-------------|
| `--type` | | Import type: `claude-ai` or `chatgpt` (required) |

The path can be a `.zip` file, a `conversations.json` file
(Claude.ai only), or a directory containing the extracted
export.

**Examples:**

```bash
agentsview import --type claude-ai ~/Downloads/claude.zip
agentsview import --type chatgpt ~/Downloads/chatgpt.zip
agentsview import --type claude-ai ./conversations.json
```

---

### `agentsview session`

Programmatic access to session data for scripts, automation
agents, and CI jobs. See [Session API](/session-api/) for full
documentation, including stability guarantees, transport
auto-detection, and every subcommand.

```bash
agentsview session get <id>              # metadata + signals
agentsview session list [flags]          # filtered list
agentsview session list --resume         # recently-active resume table
agentsview session messages <id>         # paginated messages
agentsview session tool-calls <id>       # flat tool-call list
agentsview session export <id>           # stream raw source file
agentsview session sync <path-or-id>     # parse + insert
agentsview session watch <id>            # NDJSON event stream
agentsview session search <pattern>      # content search across sessions
agentsview session usage <id>            # token usage and cost estimate
```

Structured response commands accept `--format json`; `--json` is
a short alias for that scripting mode. Use `--server <url>` to
target an explicit running daemon, `AGENTSVIEW_SERVER_TOKEN` or
`--server-token-file <path>` when that daemon requires auth, or
`--pg` to read from configured PostgreSQL.

`AGENTSVIEW_PG_URL`, a legacy `[pg].url`, or the effective default target from `default_pg` plus `[pg.NAME]` are sync configuration only; they
do not change the default read path. Read commands use local SQLite
unless `--pg` is supplied, in which case they fail fast if no
connection URL is available. Mutating commands such as `session sync`
and local-only raw source export continue to use the local archive.

Use [`agentsview health`](#agentsview-health) for a human-first
signal view and [Session API](/session-api/) for the full
programmatic contract, including daemon-first transport behavior
and markdown export details.

`agentsview session list` renders a resume-oriented human table by
default, including a recently-active marker, session ID, age, agent,
project, branch, message count, title, and working directory. Pass
`--resume` or its `--active` alias to show sessions active in the
last 15 minutes; combine either flag with `--active-since <RFC3339>`
to choose a wider or narrower window.

---

### `agentsview mcp`

Run a read-only Model Context Protocol server for assistant clients
that can call MCP tools. The server exposes session search, listing,
overview, message retrieval, content search, and usage-summary tools
over the same service layer used by the CLI and HTTP API. See
[MCP Server](/mcp/) for setup examples and operational guidance.

```bash
agentsview mcp
agentsview mcp --http 127.0.0.1:8085
agentsview mcp --server http://127.0.0.1:8080
```

By default, `agentsview mcp` speaks stdio, which is the expected
transport for local MCP clients such as Claude Desktop, Claude Code,
and Codex. `--http <addr>` serves StreamableHTTP instead. Bare ports
and `:PORT` values bind to `127.0.0.1`; non-loopback binds require
`--http-allow-insecure` plus a configured bearer token.

Local MCP mode is daemon-backed. Each tool call resolves the local
AgentsView daemon and starts it when needed, so a long-lived MCP
server can keep working after the daemon exits due to idleness. The
MCP server does not fall back to opening the local SQLite archive
directly.

Use `--server <url>` to point at an explicit running daemon. When the
daemon requires auth, provide `AGENTSVIEW_SERVER_TOKEN` or
`--server-token-file <path>`. Use `--pg` to read from configured
PostgreSQL directly, or run
[`agentsview pg serve`](/pg-sync/#agentsview-pg-serve) and pass its
URL with `--server`.

| Flag | Default | Description |
|------|---------|-------------|
| `--http <addr>` | | Serve StreamableHTTP instead of stdio |
| `--http-allow-insecure` | `false` | Allow non-loopback HTTP binds; requires bearer auth |
| `--server <url>` | | Explicit daemon URL for MCP tool calls |
| `--server-token-file <path>` | | Bearer token file for an explicit daemon URL |
| `--pg` | `false` | Read from configured PostgreSQL |

---

### `agentsview secrets`

Scan for and list detected secret leaks across sessions, with
matches redacted by default. See
[Secret Scanning](/session-api/#secret-scanning) for the full
detector set, storage shape, and HTTP API.

```bash
agentsview secrets scan [flags]   # scan sessions for leaks
agentsview secrets list [flags]   # list stored findings
```

`secrets scan` walks the archive, applies the full ruleset
(definite + candidate tiers), and writes new findings to the
`secret_findings` table. Pass `--backfill` to scan only
sessions that haven't yet been scanned at the current ruleset
version — the inline scan that runs during sync only stamps
definite-tier findings, so `--backfill` is how you pick up the
heuristic candidates without re-scanning the whole archive.

`secrets list` returns redacted findings by default. Pass
`--reveal` to print the raw values — this only works against a
localhost-bound daemon and emits a warning to stderr.

| Flag | Used by | Description |
|------|---------|-------------|
| `--format` | both | `human` or `json` (inherited from `secrets`) |
| `--backfill` | scan | Scan only sessions not yet scanned at the current ruleset version |
| `--project` | both | Limit to a project |
| `--agent` | both | Limit to an agent |
| `--date-from` | both | Sessions on or after `YYYY-MM-DD` |
| `--date-to` | both | Sessions on or before `YYYY-MM-DD` |
| `--rule` | list | Filter by rule name (e.g. `aws-access-key`) |
| `--confidence` | list | `definite`, `candidate`, or `all` (default `definite`) |
| `--reveal` | list | Show unredacted values (localhost-only) |
| `--limit` | list | Max findings (default 50, max 500) |
| `--cursor` | list | Pagination cursor from a previous response |

---

### `agentsview help`

Print usage information.

```bash
agentsview help
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AIDER_DIR` | unset | Aider discovery root; set this to opt into scanning a code root |
| `AMP_DIR` | `~/.local/share/amp/threads` | Deprecated; historical local Amp thread JSON files only |
| `ANTIGRAVITY_DIR` | `~/.gemini/antigravity` | Google Antigravity IDE sessions directory |
| `ANTIGRAVITY_CLI_DIR` | `~/.gemini/antigravity-cli` | Google Antigravity CLI sessions directory |
| `ANTIGRAVITY_KEY` | | Optional key for decrypting Antigravity CLI `.pb` transcripts (defaults to summary mode without it) |
| `CLAUDE_PROJECTS_DIR` | `~/.claude/projects` | Claude Code projects directory |
| `COWORK_DIR` | (platform-specific) | Claude Desktop cowork sessions directory |
| `CODEX_SESSIONS_DIR` | `~/.codex/sessions` | Codex sessions directory |
| `COMMANDCODE_PROJECTS_DIR` | `~/.commandcode/projects` | Command Code projects directory |
| `COPILOT_DIR` | `~/.copilot` | Copilot CLI sessions directory |
| `CORTEX_DIR` | `~/.snowflake/cortex/conversations` | Cortex Code conversations directory |
| `CURSOR_PROJECTS_DIR` | `~/.cursor/projects` | Cursor transcripts directory |
| `DEEPSEEK_TUI_SESSIONS_DIR` | `~/.codewhale/sessions` and `~/.deepseek/sessions` | DeepSeek TUI sessions directory |
| `FORGE_DIR` | `~/.forge` | Forge directory (contains `.forge.db`) |
| `GEMINI_DIR` | `~/.gemini` | Gemini CLI directory |
| `GPTME_DIR` | `~/.local/share/gptme/logs` | gptme logs directory |
| `HERMES_SESSIONS_DIR` | `~/.hermes/sessions` | Hermes Agent sessions directory |
| `IFLOW_DIR` | `~/.iflow/projects` | iFlow projects directory |
| `KILO_DIR` | `~/.local/share/kilo` | Kilo data directory |
| `KIMI_DIR` | `~/.kimi/sessions` and `~/.kimi-code/sessions` | Kimi sessions directory |
| `KIRO_SESSIONS_DIR` | `~/.kiro/sessions/cli` and `~/.local/share/kiro-cli` | Kiro CLI sessions directory (JSONL and SQLite) |
| `KIRO_IDE_DIR` | (platform-specific) | Kiro IDE sessions directory |
| `MIMOCODE_DIR` | `~/.local/share/mimocode` | MiMoCode data directory |
| `VIBE_SESSIONS_DIR` | `~/.vibe/logs/session` | Mistral Vibe sessions directory |
| `OMP_DIR` | `~/.omp/agent/sessions` | OhMyPi sessions directory |
| `OPENCLAW_DIR` | `~/.openclaw/agents` and `~/.kimi_openclaw/agents` | OpenClaw agents directory |
| `OPENCODE_DIR` | `~/.local/share/opencode` | OpenCode data directory |
| `OPENHANDS_CONVERSATIONS_DIR` | `~/.openhands/conversations` | OpenHands CLI conversations directory |
| `PI_DIR` | `~/.pi/agent/sessions` | Pi sessions directory |
| `PIEBALD_DIR` | `~/.local/share/piebald` | Piebald directory (contains `app.db`) |
| `POSITRON_DIR` | (platform-specific) | Positron Assistant user directory |
| `QCLAW_DIR` | `~/.qclaw/agents` | QClaw agents directory |
| `QWEN_PROJECTS_DIR` | `~/.qwen/projects` | Qwen Code projects directory |
| `QWENPAW_DIR` | `~/.copaw/workspaces` | QwenPaw workspaces directory |
| `REASONIX_DIR` | `~/.reasonix` and `~/AppData/Roaming/reasonix` | Reasonix data directory |
| `SHELLEY_DIR` | `~/.config/shelley` | Shelley data directory |
| `VISUALSTUDIO_COPILOT_DIR` | (platform-specific) | Visual Studio Copilot traces directory |
| `VSCODE_COPILOT_DIR` | (platform-specific) | VS Code Copilot sessions directory |
| `WARP_DIR` | (platform-specific) | Warp database directory |
| `WORKBUDDY_PROJECTS_DIR` | `~/.workbuddy/projects` | WorkBuddy projects directory |
| `ZED_DIR` | (platform-specific) | Zed data directory (contains `threads/threads.db`) |
| `ZENCODER_DIR` | `~/.zencoder/sessions` | Zencoder sessions directory |
| `AGENTSVIEW_DATA_DIR` | `~/.agentsview` | Data directory (database, config) |
| `AGENTSVIEW_PG_URL` | | PostgreSQL connection URL |
| `AGENTSVIEW_PG_MACHINE` | | Machine name for PG push sync |
| `AGENTSVIEW_PG_SCHEMA` | `agentsview` | PostgreSQL schema name |
| `AGENTSVIEW_DUCKDB_PATH` | `~/.agentsview/sessions.duckdb` | DuckDB mirror file path |
| `AGENTSVIEW_DUCKDB_URL` | | Remote Quack endpoint URL for `duckdb serve` |
| `AGENTSVIEW_DUCKDB_TOKEN` | | Quack authentication token |
| `AGENTSVIEW_DUCKDB_MACHINE` | | Machine name for DuckDB push |
| `AGENTSVIEW_GITHUB_TOKEN` | | GitHub token used for local Gist publishing fallback and `agentsview stats` PR aggregation |
| `AGENTSVIEW_DISABLE_UPDATE_CHECK` | | Set to `1` to disable the update check |
| `AGENTSVIEW_NO_DAEMON` | | Set to `1`, `true`, `yes`, or `on` to disable CLI daemon auto-start |
| `AGENTSVIEW_DAEMON_IDLE_TIMEOUT` | `20m` | Override idle self-shutdown duration for detached background daemons |
| `AGENTSVIEW_TELEMETRY_ENABLED` | | Set to `0` to disable [anonymous daemon telemetry](/configuration/#anonymous-daemon-telemetry) |

Environment variables override the built-in defaults. Set them
in your shell profile or pass them inline:

```bash
AGENTSVIEW_DATA_DIR=/tmp/av-test agentsview serve
```
