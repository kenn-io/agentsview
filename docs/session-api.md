---
title: Session API
description: Programmatic access to session data via the agentsview session CLI and REST endpoints
---

The `agentsview session` command group is a stable, programmatic
surface for reading and writing session data. It is designed for
shell scripts, automation agents, and CI jobs that need structured
output rather than the web UI.

When an AgentsView daemon is running, the CLI proxies supported
operations to it over HTTP. On a cold archive, read-only commands
open local SQLite directly in read-only mode, while commands that
need fresh data or need to write start or reuse the detached local
daemon. This keeps one-off reads fast and keeps SQLite writes owned
by one process.

## Quick examples

```bash
# Search parsed message/tool content without scanning raw JSONL files.
agentsview session search "database timeout" --json --limit 10

# Recover recent context for one project, then fetch the first page
# of messages from a selected session.
agentsview session list --project myapp --limit 5 --json
agentsview session messages <session-id> --from 0 --limit 20 --json

# Query an already-running daemon explicitly.
agentsview session list --server http://127.0.0.1:8080 --json

# Read from configured PostgreSQL instead of local SQLite.
agentsview session search "regression" --pg --json
```

## Stability

- **Additive-only.** New fields may appear at any time. Existing
  fields are never renamed or removed.
- **Types are stable.** A field that is a string stays a string.
- **Unknown fields are safe to ignore.** Well-behaved consumers
  tolerate forward-compatible additions.

HTTP and CLI share DTOs for bounded responses (same JSON object).
The CLI `watch` command emits NDJSON whose lines mirror the
underlying SSE events.

## Transport

Detection uses kit daemon runtime records in `AGENTSVIEW_DATA_DIR`
and the daemon ping endpoint. Runtime records include service and
API metadata so incompatible or read-only daemons are not mistaken
for a writable local archive owner.

- If `--server <url>` is set, supported commands proxy to that
  daemon over HTTP. `--server` and `--pg` are mutually exclusive.
  The local config `auth_token` is not sent to explicit URLs; set
  `AGENTSVIEW_SERVER_TOKEN` or pass `--server-token-file <path>`
  when that remote daemon requires auth.
- If no explicit server is set and a local daemon is running, read
  and write commands proxy to it over HTTP.
- If a [`pg serve`](/pg-sync/#agentsview-pg-serve) daemon is
  running (read-only), read commands proxy to it but
  `session sync` refuses with a clear error.
- If both a writable local daemon and a `pg serve` daemon
  advertise the same data directory, the writable one wins so
  sync/write operations don't silently land on a read-only
  target.
- If no daemon is running, read-only commands open the local
  archive directly in read-only mode.
- If a command requires fresh data or needs to write and no daemon
  is running, the CLI starts `agentsview serve --background`, waits
  for readiness, and proxies the operation to that daemon.
- If `AGENTSVIEW_NO_DAEMON=1` is set, the CLI never auto-starts a
  daemon. Read commands use direct read-only SQLite. Write commands
  run directly only after acquiring the per-data-dir write-owner
  lock.
- If a writable daemon is known to own the local archive but is not
  reachable, write commands refuse instead of opening SQLite as a
  second writer. Read commands may still fall back to direct
  read-only SQLite.
- `session export` always runs locally regardless of daemon
  state, and rejects `--server`, `--pg`, and `--format`/`--json`
  because it streams raw source bytes.

When a discovered local daemon requires auth (`require_auth: true`),
the CLI attaches `Authorization: Bearer <token>` using the
`auth_token` from the shared config. Explicit `--server` URLs are
different: they only receive a bearer token supplied with
`AGENTSVIEW_SERVER_TOKEN` or `--server-token-file <path>`, so a
local daemon token is not leaked to arbitrary remote URLs. Prefer
`--server-token-file` for long-running commands and shared hosts so
the token does not appear in process arguments.

`--pg` opens the configured PostgreSQL read store directly. It is
useful for automation running away from the UI server, but it is
read-only: `session sync` and `session export` reject it. If
`AGENTSVIEW_PG_URL` or `[pg].url` is configured, read commands still
use local SQLite unless `--pg` is supplied.

## Common flags

| Flag                         | Description                                      |
|------------------------------|--------------------------------------------------|
| `--format human\|json`       | Output format. Default `human`.                  |
| `--json`                     | Alias for `--format json`.                       |
| `--server <url>`             | Explicit daemon URL for HTTP-backed operations.  |
| `--server-token-file <path>` | Bearer token file for an explicit `--server` URL. |
| `--pg`                       | Read from configured PostgreSQL instead of SQLite. |

## Shared metadata endpoints

The web UI and generated API clients use shared metadata endpoints
for filter options:

```http
GET /api/v1/projects
GET /api/v1/machines
GET /api/v1/branches
GET /api/v1/agents
```

`GET /api/v1/branches` returns distinct `(project, branch)` pairs
plus an opaque `token` field:

```json
{
  "branches": [
    {
      "project": "myapp",
      "branch": "main",
      "token": "..."
    }
  ]
}
```

Pass the returned token back as the `git_branch` query parameter on
branch-aware endpoints. Treat it as opaque and URL-encode it in
manual HTTP calls. The token is scoped by both project and branch,
so `app-a/main` and `app-b/main` remain distinct, and an empty
branch remains distinct from a literal `unknown` branch.

## Commands

### `agentsview session get`

Return session metadata plus computed signal fields. Shape matches
`GET /api/v1/sessions/{id}`.

```bash
agentsview session get <id> [--format json]
```

```json
{
  "id": "abc-123",
  "project": "myapp",
  "machine": "local",
  "agent": "claude",
  "first_message": "...",
  "display_name": "...",
  "git_branch": "main",
  "started_at": "2026-04-18T12:00:00Z",
  "ended_at": "2026-04-18T13:00:00Z",
  "message_count": 42,
  "user_message_count": 7,
  "health_score": 85,
  "health_grade": "A",
  "outcome": "completed",
  "health_score_basis": ["..."],
  "health_penalties": {"tool_retries": 5},
  "parser_malformed_lines": 3,
  "secret_leak_count": 0
}
```

`health_score_basis` and `health_penalties` are populated when
`health_score` is non-null. Both HTTP and CLI surfaces return them.

`git_branch` is the branch captured at sync time when the parser or
source metadata exposes it; sessions with no recorded branch omit
the field. `parser_malformed_lines` counts the malformed source
lines a parser skipped while still recovering the session and is
omitted when zero. Antigravity detail responses may also include
`decode_confidence`; the value `low` means the session came from an
unrecognized Antigravity schema fingerprint and was decoded
heuristically.

`secret_leak_count` (added in 0.30.0) counts definite-tier
findings from [secret scanning](#secret-scanning) and is stamped
inline during sync. Candidate-tier findings only show up after
an explicit [`agentsview secrets scan --backfill`](/commands/#agentsview-secrets)
and do not contribute to this count.

---

### `agentsview session list`

Filtered session list. Response shape matches
`GET /api/v1/sessions`.

```bash
agentsview session list [flags]
```

```json
{
  "sessions": [ ... ],
  "next_cursor": "...",
  "total": 42
}
```

One-shot and automated sessions are excluded by default. When the first CLI
page hides any, `session list` writes an advisory to stderr with the hidden
count for each category and the `--include-one-shot` or `--include-automated`
flag that reveals it. Human and JSON stdout are unchanged, so redirecting or
piping structured output remains safe. The JSON `total` continues to describe
the filtered result, not the excluded sessions. Use the `--include-*` flags to
opt back in.

| Flag                  | HTTP param          | Notes                             |
|-----------------------|---------------------|-----------------------------------|
| `--project`           | `project`           | string                            |
| `--exclude-project`   | `exclude_project`   | string                            |
| `--machine`           | `machine`           | string                            |
| —                     | `git_branch`        | opaque token from `GET /api/v1/branches` |
| `--agent`             | `agent`             | string                            |
| `--date`              | `date`              | `YYYY-MM-DD`                      |
| `--date-from`         | `date_from`         | `YYYY-MM-DD`                      |
| `--date-to`           | `date_to`           | `YYYY-MM-DD`                      |
| `--active-since`      | `active_since`      | RFC3339 timestamp                 |
| `--since`             | `active_since`      | Relative — `Nh` hours, `Nd` days, `Nw` weeks, `Nm` calendar months (not minutes), `Ny` years — or `YYYY-MM-DD`; resolved against now and mutually exclusive with `--active-since` |
| `--resume`            | `active_since`      | CLI shortcut for sessions active in the last 15 minutes |
| `--active`            | `active_since`      | Alias for `--resume`              |
| `--min-messages`      | `min_messages`      | int                               |
| `--max-messages`      | `max_messages`      | int                               |
| `--min-user-messages` | `min_user_messages` | int                               |
| `--include-one-shot`  | `include_one_shot`  | bool                              |
| `--include-automated` | `include_automated` | bool                              |
| `--include-children`  | `include_children`  | bool                              |
| `--outcome`           | `outcome`           | comma-separated                   |
| `--health-grade`      | `health_grade`      | comma-separated                   |
| `--min-tool-failures` | `min_tool_failures` | int; `0` is a meaningful filter   |
| `--has-secret`        | `has_secret`        | bool — only sessions with at least one definite [secret finding](#secret-scanning) |
| `--sort`              | `order_by`          | comma-separated keys; optional `:asc` / `:desc` suffix per key |
| `--reverse`, `-r`     | `descending`        | flips the default direction for unsuffixed sort keys |
| `--cursor`            | `cursor`            | opaque string from prior response |
| `--limit`             | `limit`             | int; default 200, max 500         |

Sort keys are `recent`, `started`, `messages`, `user-messages`,
`output-tokens`, `peak-context`, `failures`, `retries`,
`edit-churn`, `compactions`, `context-pressure`, `health`,
`secrets`, and `id`. `recent` defaults descending; the other keys
default ascending unless `--reverse`, `descending=true`, or an
explicit suffix overrides them.

Human CLI output is formatted for resuming work: it shows the full
session ID, age, agent, project, branch, message count, title, and
working directory, with a marker on sessions active in the last 15
minutes. `--resume` and `--active` set `active_since` to that
15-minute window unless `--active-since` is supplied explicitly.
HTTP callers should pass `active_since` directly.

Examples:

```bash
agentsview session list --resume
agentsview session list --sort messages:desc,started:asc
agentsview session list --sort health --reverse
```

---

### `agentsview session messages`

Return a window of messages. Response shape matches
`GET /api/v1/sessions/{id}/messages`.

```bash
agentsview session messages <id> [--from N] [--limit N] [--direction asc|desc]
agentsview session messages <id> --around N [--before N] [--after N] [--role user,assistant]
```

`--from` is pointer-valued at the service layer: omitting it means
"start at the beginning" for ascending and "start at the newest
page" for descending; an explicit `--from 0` means "start at ordinal
0" in both directions. `--direction` is validated to `asc` or `desc`.

Window and role flags (see
[Semantic Search](/semantic-search/#cursor-follow-from-a-hit-to-its-surrounding-conversation)
for the cursor-follow workflow they support):

| Flag       | HTTP param | Notes                                                        |
|------------|------------|--------------------------------------------------------------|
| `--around` | `around`   | Center a window on this ordinal; mutually exclusive with `--from`/`--direction` |
| `--before` | `before`   | Messages before the anchor (default 5); requires `--around`  |
| `--after`  | `after`    | Messages after the anchor (default 5); requires `--around`   |
| `--role`   | `roles`    | Comma-separated roles to include, e.g. `user,assistant`      |

With a `--role` filter, `--before`/`--after` count filtered messages;
the anchor message is always included. Responses report the window's
`first_ordinal`/`last_ordinal` so callers can continue paging with
`--from <last_ordinal + 1>`.

```json
{
  "messages": [
    {
      "ordinal": 0,
      "role": "user",
      "content": "...",
      "thinking_text": "",
      "timestamp": "2026-04-18T12:00:00Z",
      "is_system": false,
      "source_type": "user",
      "source_subtype": "",
      "has_thinking": false,
      "has_tool_use": false
    }
  ],
  "count": 1
}
```

`thinking_text` holds the concatenated text of any `thinking` blocks
the agent emitted, separated from the flattened `content` which
still contains inline `[Thinking]...[/Thinking]` markers for UI
rendering.

Promoted `source_subtype` values on `is_system: true` messages:
`continuation`, `resume`, `interrupted`, `task_notification`,
`stop_hook`, `compact_boundary`.

---

### `agentsview session tool-calls`

Chronological flattened list of tool invocations.

```bash
agentsview session tool-calls <id>
```

```json
{
  "tool_calls": [
    {
      "ordinal": 3,
      "timestamp": "2026-04-18T12:05:00Z",
      "tool_use_id": "toolu_01abc...",
      "tool_name": "Bash",
      "category": "Bash",
      "input_json": "{\"command\":\"ls\"}",
      "skill_name": "",
      "subagent_session_id": "",
      "result_length": 128
    }
  ],
  "count": 1
}
```

`input_json` is a string — usually a serialized JSON object but may
be a plain string (e.g. `"echo hello world"` from Codex).

---

### `POST /api/v1/sessions/{id}/resume`

Resume a local session in its native agent, or return the command
that would be launched. This is an HTTP-only surface used by the
web UI's resume menu.

```http
POST /api/v1/sessions/{id}/resume
```

Request body:

```json
{
  "skip_permissions": false,
  "fork_session": false,
  "from_ordinal": 17,
  "command_only": false,
  "opener_id": ""
}
```

Response:

```json
{
  "launched": true,
  "terminal": "Terminal",
  "command": "cd /repo && claude --resume abc-123",
  "cwd": "/repo"
}
```

`command_only: true` returns the command without launching a
terminal. When a launch is attempted but fails, the response still
includes the command with `launched: false` and an `error` code of
`no_terminal_found` or `launch_failed`. Read-only local mode can
still return commands, but remote sessions and read-only remote
serving cannot launch local programs.

For Claude Code sessions, setting `fork_session: true` without
`from_ordinal` appends Claude's native `--fork-session` flag to the
normal resume command. Setting both `fork_session: true` and
`from_ordinal` creates a message-point fork: AgentsView renders the
transcript through that message ordinal into a temporary prompt and
runs `claude < prompt` from the resolved session working directory.
Message-point forks are Claude-only, require `fork_session`, reject
`opener_id`, and return `404` when the ordinal is not present.

---

### `agentsview session export`

Stream the raw session source file to stdout. This is a
**local-only** filesystem helper: the source file path is resolved
from the local SQLite archive, never from any daemon. Both
`--server` and `--format`/`--json` are rejected with an error — the
command also rejects `--pg`. It streams raw bytes, so
structured-output and remote-store flags don't apply.

```bash
agentsview session export <id>
```

Exit states:

| State                                  | Behavior                                                  |
|----------------------------------------|-----------------------------------------------------------|
| Session in local archive, file on disk | Streams bytes verbatim                                    |
| Session in local archive, file missing | Exit 1: error prefixed `source file not found`            |
| Session in local archive, path empty   | Exit 1: `source file not found for session <id>`          |
| Session not in local archive           | Exit 1: `session not in local archive: <id>`              |

For a DB-derived export (HTML or markdown) use the HTTP endpoints
`/api/v1/sessions/{id}/export` or `/api/v1/sessions/{id}/md`.

Markdown export accepts an optional `depth` query parameter:

- omitted: root session only
- `depth=1`: include direct child/subagent sessions
- `depth=all`: recurse through the child-session tree

Any other `depth` value is rejected.

---

### `agentsview session sync`

Parse and insert a single session. Blocks until indexing and signal
computation complete. JSON output is the `SessionDetail` of the
synced session; human output is one line: `synced: <id>`.

```bash
agentsview session sync <path-or-id>
```

Argument resolution: if the argument is an existing filesystem
path, it is treated as a raw JSONL file to parse. Otherwise it is
treated as a session ID for re-parse.

With `--server <url>`, path-shaped arguments such as
`/var/log/agent/session.jsonl` or `./session.jsonl` are sent to the
remote daemon as paths and resolved on that daemon host. Bare values
without path separators are treated as session IDs.

When a single JSONL maps to more than one session (for example a
Claude transcript with forked/resumed branches), `session sync
<path>` refuses with an error that lists every candidate id. The
contract is "one session in, one `SessionDetail` back", so the CLI
never picks arbitrarily.

At that point the file has already been parsed and every candidate
session written to the local archive — the ambiguity check runs
after the sync engine finishes. Re-run `session sync <id>` with the
specific session you want; the `<id>` form resolves the file path
from the archive, so it only works for sessions already present
there.

- `session sync` uses a writable local daemon when one is running,
  or starts a detached daemon when no compatible daemon is running.
  It then proxies to `POST /api/v1/sessions/sync` so parsing and
  signal computation remain daemon-owned.
- If a [`pg serve`](/pg-sync/#agentsview-pg-serve) daemon is
  running (read-only), sync refuses with a clear error.
- If `AGENTSVIEW_NO_DAEMON=1` is set, the CLI runs the sync
  in-process only after acquiring the local write-owner lock.

---

### `agentsview session watch`

Stream NDJSON events as the session updates. Each line is a small
object that wraps an SSE event.

```bash
agentsview session watch <id>
```

```
{"event":"session_updated","data":"abc-123"}
{"event":"heartbeat","data":"2026-04-18T12:05:00Z"}
```

Recognized `event` values today: `session_updated`, `heartbeat`.
New events may be added; consumers should ignore unknown events.
The command runs until interrupted (Ctrl+C) or the context is
cancelled. Like `session export`, it streams a fixed format and
rejects `--format`/`--json`.

Watch validates the session id before opening the stream. An
unknown id fails fast with a `watch: session not found: <id>`
error and non-zero exit instead of producing an indefinite
heartbeat stream — typos in automation scripts surface immediately.
When proxied to a daemon, the same condition surfaces as HTTP 404
at the transport layer before being translated to the CLI error.

---

### `agentsview session search`

Substring, RE2 regex, or FTS5 search across message bodies,
tool inputs, and tool result content. Response shape matches
`GET /api/v1/search/content`.

```bash
agentsview session search <pattern> [flags]
```

```json
{
  "matches": [
    {
      "session_id": "abc-123",
      "project": "myapp",
      "ordinal": 17,
      "ordinal_range": [12, 24],
      "location": "tool_result",
      "tool_name": "Bash",
      "snippet": "...connecting to db with token ***REDACTED***..."
    }
  ]
}
```

One-shot, automated, and subagent sessions are excluded by
default; opt back in with `--include-one-shot`,
`--include-automated`, or `--include-children`.

| Flag                  | HTTP param          | Notes                                                  |
|-----------------------|---------------------|--------------------------------------------------------|
| `--regex`             | `mode=regex`        | Treat pattern as an RE2 regex                          |
| `--fts`               | `mode=fts`          | Tokenized FTS5 search; messages-only                   |
| `--semantic`          | `mode=semantic`     | Vector search over user/assistant messages; messages-only — see [Semantic Search](/semantic-search/) |
| `--hybrid`            | `mode=hybrid`       | Semantic + FTS reciprocal rank fusion; messages-only — see [Semantic Search](/semantic-search/) |
| `--scope`             | `scope`             | `top`, `all` (default), or `subordinate` — semantic/hybrid only; supersedes `include_children` in those modes |
| `--context`           | `context`           | int — N messages of context before/after each match (max 10) |
| `--in`                | `in`                | Comma-separated: `messages,tool_input,tool_result` (default all) |
| `--exclude-system`    | `exclude_system`    | Drop system messages from the scan                     |
| `--reveal`            | `reveal`            | Show full secret values (localhost-only; warning to stderr) |
| `--project`           | `project`           | string                                                 |
| `--exclude-project`   | `exclude_project`   | string                                                 |
| `--machine`           | `machine`           | string                                                 |
| —                     | `git_branch`        | opaque token from `GET /api/v1/branches`               |
| `--agent`             | `agent`             | string                                                 |
| `--date`              | `date`              | `YYYY-MM-DD`                                           |
| `--date-from`         | `date_from`         | `YYYY-MM-DD`                                           |
| `--date-to`           | `date_to`           | `YYYY-MM-DD`                                           |
| `--active-since`      | `active_since`      | RFC3339 timestamp                                      |
| `--since`             | `active_since`      | Relative — `Nh` hours, `Nd` days, `Nw` weeks, `Nm` calendar months (not minutes), `Ny` years — or `YYYY-MM-DD`; resolved against now and mutually exclusive with `--active-since` |
| `--include-children`  | `include_children`  | bool                                                   |
| `--include-automated` | `include_automated` | bool                                                   |
| `--include-one-shot`  | `include_one_shot`  | bool                                                   |
| `--limit`             | `limit`             | int; default 50, max 500                               |
| `--cursor`            | `cursor`            | int — pagination cursor from a previous response       |

`--regex`, `--fts`, `--semantic`, and `--hybrid` are mutually
exclusive. `--fts` is the fastest mode on large archives but only
searches message bodies; substring (the default) and regex modes
also walk `tool_calls.input_json`, `tool_calls.result_content`,
and the `tool_result_events` rows. `--semantic` and `--hybrid`
require an embedding index and return a single ranked page
(`--cursor` is rejected) — see [Semantic Search](/semantic-search/)
for setup, scoring, and limitations.

Every match, in every mode, carries the conversation-unit
citation described in
[Hit shape](/semantic-search/#hit-shape-ranges-and-anchors):
`ordinal_range` — `[start, end]` of the conversation unit
containing the match, always present, `[ordinal, ordinal]` when
the match is its own unit — plus the lineage fields
`subordinate`, `relationship`, `parent_session_id`, and
`is_sidechain`. `ordinal` stays the anchor (the exact matched
message) in every mode. Only the lineage fields are `omitempty`:
a missing key unambiguously means top-level with no lineage,
while `ordinal_range` is never omitted, even at `[0, 0]`.
`score` is the one field only `--semantic`/`--hybrid` emit.
`--scope` is rejected outside `--semantic`/`--hybrid`; in those
modes it supersedes `--include-children`, and
subagent/fork-typed or parent-linked sessions are exempt from
the default one-shot exclusion.

Snippets carry ~60 characters of context on each side of the
match, snapped to rune boundaries. Any substring that matches
the [secret scanner](#secret-scanning) rule set is masked unless
`--reveal` is passed. The same masking applies to the HTTP
endpoint when called from a remote origin — `reveal=true` is
only honored on a localhost-bound daemon.

---

### `agentsview session usage`

Per-session token usage and cost estimate. Output shape is
stable for the fields shown below; new fields may be added.

```bash
agentsview session usage <id> [--format json]
```

```json
{
  "session_id": "abc-123",
  "agent": "claude",
  "project": "myapp",
  "total_output_tokens": 15230,
  "peak_context_tokens": 84000,
  "has_token_data": true,
  "cost_usd": 2.41,
  "has_cost": true,
  "models": ["claude-opus-4-7"],
  "unpriced_models": [],
  "breakdown_count": 42,
  "breakdown": [
    {
      "ordinal": 0,
      "message_ordinal": 0,
      "source": "message",
      "label": "Prompt 1",
      "timestamp": "2026-07-08T14:03:21Z",
      "model": "claude-opus-4-7",
      "input_tokens": 1200,
      "output_tokens": 640,
      "cache_creation_input_tokens": 0,
      "cache_read_input_tokens": 43000,
      "cost_usd": 0.58,
      "has_cost": true
    }
  ],
  "server_running": false
}
```

| Field                 | Notes                                                                |
|-----------------------|----------------------------------------------------------------------|
| `total_output_tokens` | Sum of generated output tokens across the session                    |
| `peak_context_tokens` | Highest context-token count observed during the session              |
| `has_token_data`      | `false` when the session has no per-message token usage              |
| `cost_usd`            | Model-pricing estimate in USD; `0` when `has_cost` is `false`        |
| `has_cost`            | `false` if any contributing row is unpriced — never reports a partial total as complete |
| `models`              | Models that contributed to the cost estimate, sorted by model name      |
| `unpriced_models`     | Omitted from JSON when empty; lists models seen but missing from pricing |
| `breakdown_count`     | Number of per-step usage rows in the session; always populated       |
| `breakdown`           | Per-step usage rows, in session order; CLI JSON always includes them (added in 0.37.1) |
| `server_running`      | `true` when the report came from an already-running daemon           |

Each `breakdown` row carries the fields shown in the example:

| Row field         | Notes                                                            |
|-------------------|------------------------------------------------------------------|
| `ordinal`         | Position of the row in the session's deduplicated usage stream   |
| `message_ordinal` | Ordinal of the originating message; omitted when the row is not tied to one |
| `source`          | `message` for per-message token usage; otherwise the usage-event source |
| `label`           | Display label — `Prompt N` for message rows, `Step N` for other rows tied to a message, else the source name |
| `cost_usd`        | Per-row estimate; `0` with `has_cost: false` when the model is unpriced |

Human output is a five-line summary:

```
Session:       abc-123
Agent:         claude
Output:        15230
Peak ctx:      84000
Cost:          ~$2.41 (claude-opus-4-7)
```

The leading `~` on the cost line marks the figure as a
model-pricing estimate. When some contributing models are
unpriced, the cost line reads `n/a (unpriced: model-x)`; when
the session has no token data at all, it reads `n/a`.

**HTTP endpoint** — as of 0.32.0, the same data is available
over REST:

```http
GET /api/v1/sessions/{id}/usage
```

The response uses the same JSON fields shown above and is
available from both local SQLite-backed `agentsview serve` and
read-only [`agentsview pg serve`](/pg-sync/#agentsview-pg-serve).
HTTP responses set `server_running: true`. As of 0.37.1, pass
`?breakdown=true` to include the per-step `breakdown` rows;
without it `breakdown` is `[]` while `breakdown_count` still
reports the row count. The CLI requests the breakdown on every
path (local, `--server`, and `--pg`), so its `--format json`
output always includes the rows. The session detail header uses
this endpoint to render its
[per-step usage breakdown](/usage/#token-usage). Existing sessions
return `200 OK` even when token or cost data is absent; inspect
`has_token_data`, `has_cost`, and `unpriced_models` to decide
how to present that state. Missing sessions return `404` with:

```json
{
  "error": {
    "code": "session_not_found",
    "message": "session not found"
  }
}
```

Unexpected usage-query failures return `500` with
`error.code = "usage_query_failed"`.

**Exit codes** — `usage` keeps the older `token-use` contract:

| Code | Meaning                                       |
|------|-----------------------------------------------|
| `0`  | Token data or cost present and reported       |
| `2`  | Session not found in the local archive        |
| `3`  | Session exists but has neither token data nor cost |

The command uses a writable local daemon when one is running, or
starts a detached daemon when fresh local data is needed and no
compatible daemon is running. With `AGENTSVIEW_NO_DAEMON=1`, it
falls back to direct local SQLite after acquiring the write-owner
lock for any required refresh. Configured PostgreSQL does not
change this command's default local behavior; pass `--pg` to read
usage from the shared PostgreSQL store. With `--server`, it calls
`GET /api/v1/sessions/{id}/usage` on the explicit daemon. With
`--pg`, it reads usage from the shared PostgreSQL store.

Pricing comes from the same `model_pricing` table and
[custom pricing overrides](/token-usage/#custom-model-pricing)
that back `agentsview usage daily`. Replaces the older
[`agentsview token-use`](/commands/#agentsview-token-use), which
remains as a deprecated alias.

---

## Activity Report

The Activity report endpoint powers the top-level
[Activity](/activity/) page and the `agentsview activity report`
CLI command. It returns one resolved range with concurrency buckets,
summary totals, breakdowns, and contributing sessions.

```http
GET /api/v1/activity/report
```

Activity includes one-shot sessions by default. Automated sessions
are also included by default and can be filtered with the
`automation` query parameter.

The JSON response shares the same `schema_version`, `pricing`, and `projects`
metadata contract as `agentsview activity report --json`.

| Query param | Notes |
|-------------|-------|
| `preset` | `day`, `week`, `month`, or `custom` |
| `date` | Anchor date for day/week/month presets (`YYYY-MM-DD`) |
| `from` | Custom range start, RFC3339 |
| `to` | Custom range end, RFC3339 |
| `timezone` | IANA timezone name; default `UTC` |
| `bucket` | Optional bucket override: `5m`, `15m`, `1h`, `1d`, or `1w` |
| `project` | Filter by project |
| `git_branch` | Filter by opaque branch token from `GET /api/v1/branches` |
| `agent` | Filter by agent |
| `machine` | Filter by machine |
| `automation` | `all`, `interactive`, or `automated`; default `all` |

Response excerpt:

```json
{
  "schema_version": 1,
  "pricing": {
    "source": "fetched",
    "table_version": "litellm-398a0b15378c",
    "latest_row_updated_at": "2026-06-20T18:40:00Z",
    "custom_override_count": 0,
    "effective_row_count": 2428,
    "digest": "sha256:8d815a1737bce68fa1a19ba977bf33c8c8efcc74deb954fcf62ce80e46e75f2c",
    "cost_source": "mixed",
    "fallback": {
      "used": false,
      "models": []
    },
    "models": {
      "gpt-5.4": {
        "matched_pattern": "gpt-5.4",
        "input_cost_per_mtok": 2,
        "output_cost_per_mtok": 8,
        "cache_write_cost_per_mtok": 3,
        "cache_read_cost_per_mtok": 0.5,
        "cost_source": "computed"
      }
    }
  },
  "projects": {
    "agentsview": {
      "resolution": "resolved",
      "identity": {
        "key": "sha256:97879729c8ab311e9d4b28941e3a04830b28c527f00af53f2270212eccdbbd39",
        "key_source": "git_remote",
        "normalized_remote": "github.com/acme/agentsview"
      }
    }
  },
  "timezone": "America/Chicago",
  "range_start": "2026-06-20T05:00:00Z",
  "range_end": "2026-06-21T05:00:00Z",
  "bucket_unit": "hour",
  "bucket_seconds": 3600,
  "partial": true,
  "as_of": "2026-06-20T18:40:00Z",
  "peak": {"agents": 4, "at": "2026-06-20T15:00:00Z"},
  "totals": {
    "active_minutes": 210.5,
    "agent_minutes": 346.2,
    "sessions": 18,
    "untimed_sessions": 1,
    "distinct_projects": 5,
    "distinct_models": 4,
    "output_tokens": 84231,
    "cost": 12.34,
    "automated_sessions": 3,
    "interactive_sessions": 15
  },
  "buckets": [
    {
      "start": "2026-06-20T15:00:00Z",
      "end": "2026-06-20T16:00:00Z",
      "max_agents": 4,
      "agent_minutes": 52.0,
      "output_tokens": 12000,
      "cost": 1.87,
      "automated_at_peak": 1,
      "interactive_at_peak": 3
    }
  ],
  "by_project": [{"key": "agentsview", "agent_minutes": 96.4, "cost": 4.20}],
  "by_model": [{"key": "claude-sonnet-4-6", "agent_minutes": 80.0, "cost": 3.10}],
  "by_agent": [{"key": "codex", "agent_minutes": 64.0, "cost": 2.85}],
  "by_session": [
    {
      "session_id": "codex:abc",
      "title": "Update docs",
      "project": "agentsview",
      "agent": "codex",
      "primary_model": "gpt-5.4",
      "models": ["gpt-5.4"],
      "agent_minutes": 24.5,
      "cost": 1.12,
      "output_tokens": 9200,
      "first_active": "2026-06-20T15:04:00Z",
      "last_active": "2026-06-20T15:38:00Z",
      "timing_quality": "timed",
      "is_automated": false
    }
  ],
  "intervals": [
    {
      "session_id": "codex:abc",
      "start": "2026-06-20T15:04:00Z",
      "end": "2026-06-20T15:38:00Z"
    }
  ]
}
```

Breakdown rows include total, automated, and interactive minutes and
costs. Session rows with no reliable timestamped activity use
`"timing_quality": "untimed"` and `agent_minutes: null`; they can
still contribute cost and output tokens when usage rows exist.

---

## Secret Scanning

As of 0.30.0, AgentsView scans session content for credentials
during sync and exposes findings through CLI, HTTP, and per-
session metadata. Two confidence tiers exist:

| Tier | Rules | When scanned |
|------|-------|--------------|
| **Definite** | Well-anchored vendor formats: AWS access keys, Anthropic `sk-ant-…`, OpenAI `sk-proj-`/`sk-svcacct-`/`sk-admin-`, GitHub `ghp_…` and `github_pat_…`, GitLab `glpat-…`, Slack `xoxb`/`xoxa`/`xoxp`/`xoxr`/`xoxs`, Stripe `sk_live_…` and `rk_live_…`, Google `AIza…`, npm `npm_…`, PyPI `pypi-…`, Hugging Face `hf_…`, SendGrid `SG.…`, PEM private-key blocks | Inline during sync |
| **Candidate** | FP-prone heuristics: basic-auth URLs, JWTs, high-entropy assignments | Only when `agentsview secrets scan` runs explicitly |

Findings are written to a `secret_findings` table keyed by
session ID, with the rule name, confidence, location
(message / tool_input / tool_result / tool_result_event),
match coordinates, and a `redacted_match` value (the raw
secret is never stored). Each session also carries a
`secret_leak_count` and a `secrets_rules_version` so a future
ruleset bump can drive an incremental backfill.

### HTTP API

```
GET /api/v1/secrets
POST /api/v1/secrets/scan
GET /api/v1/search/content    (when called with text that matches a secret rule, snippets are masked)
```

`GET /api/v1/secrets` accepts: `project`, `agent`, `date_from`,
`date_to`, `rule`, `confidence` (`definite` / `candidate` /
`all`, default `definite`), `reveal`, `limit`, `cursor`.
`reveal=true` is only honored on a localhost-bound daemon —
remote callers that request reveal receive HTTP 403.

`POST /api/v1/secrets/scan` streams progress with
Server-Sent Events and accepts `backfill`, `project`, `agent`,
`date_from`, and `date_to`. It is only available from a
writable local daemon.

### Listing sessions with findings

`agentsview session list --has-secret` (or `has_secret=true` on
`GET /api/v1/sessions`) returns only sessions with at least one
definite finding. The candidate tier does not contribute to
`secret_leak_count` and so does not surface through this filter.

### CLI

The [`agentsview secrets`](/commands/#agentsview-secrets) command
group wraps the scan and list operations. The fast path is:

```bash
# Re-scan the archive with the full ruleset (definite + candidate)
agentsview secrets scan

# List definite findings (redacted; localhost-only --reveal for raw values)
agentsview secrets list
agentsview secrets list --reveal
```

### PostgreSQL parity

When [PostgreSQL sync](/pg-sync/) is enabled, the
`secret_findings` table, the session-level `secret_leak_count`,
and the `--has-secret` filter all mirror to the shared
database. Substring and regex content search work the same way
against `pg serve`, with the same masking and `--reveal`
constraints.
