---
title: Session API
description: Programmatic access to session data through the agentsview session CLI and REST endpoints
---

The `agentsview session` command group gives you a stable, programmatic way to
read and write session data from shell scripts, automation agents, and CI jobs
when you need structured output instead of the web UI.

Local session reads talk to the AgentsView daemon over HTTP and start it when
needed. That keeps reads and writes under the same archive owner. Pass
`--server` or `--pg` to target another backend; raw `session export` always
runs locally.

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

- **Additive-only.** New fields may appear at any time, and existing fields are
  never renamed or removed.
- **Stable types.** A field that is a string stays a string.
- **Unknown fields are safe to ignore.** Ignore any unrecognized fields so your
  consumer tolerates forward-compatible additions.

The HTTP API and CLI share the same response schemas. When the CLI knows the
server address, it adds browser links to the output. The CLI `watch` command
emits NDJSON lines that mirror the underlying SSE events.

## Transport

To find a running daemon, the CLI checks kit daemon runtime records in
`AGENTSVIEW_DATA_DIR`, or its `run` subdirectory on Windows, along with the
daemon ping endpoint. Those runtime records carry service and API metadata so
the CLI does not mistake an incompatible or read-only daemon for a writable
local archive owner.

- If you pass `--server <url>`, supported commands proxy to that daemon over
  HTTP. You cannot combine `--server` and `--pg`. The CLI does not send your
  local config `auth_token` to explicit URLs; when a remote daemon requires
  auth, set `AGENTSVIEW_SERVER_TOKEN` or pass `--server-token-file <path>`.
- If you omit `--server` and a local daemon is running, read and write commands
  proxy to it over HTTP.
- If a read-only [`pg serve`](/docs/pg-sync/#agentsview-pg-serve) daemon is
  running, read commands proxy to it, while `session sync` refuses with a clear
  error.
- If both a writable local daemon and a `pg serve` daemon advertise the same
  data directory, the writable daemon wins so sync and write operations do not
  silently land on a read-only target.
- If no daemon is running, ordinary local session reads and writes start
  `agentsview serve --background`, wait for it to become ready, and proxy the
  operation to that daemon.
- If you set `AGENTSVIEW_NO_DAEMON=1`, the CLI never auto-starts a daemon, and
  read commands require an existing compatible daemon. The
  [`session usage`](#agentsview-session-usage) refresh path can still run
  directly after acquiring the write-owner lock, though its `--no-sync` path
  requires a daemon.
- If a writable daemon owns the local archive but is unreachable, session
  commands report the connection or compatibility error instead of opening a
  second archive handle.
- `session export` always runs locally regardless of daemon state, and it
  rejects `--server`, `--pg`, and `--format`/`--json` because it streams raw
  source bytes.

When a discovered local daemon requires auth (`require_auth: true`), the CLI
attaches `Authorization: Bearer <token>` using `auth_token` from the shared
config. Explicit `--server` URLs work differently: they only receive a bearer
token you supply through `AGENTSVIEW_SERVER_TOKEN` or
`--server-token-file <path>`, so your local daemon token does not leak to
arbitrary remote URLs. On shared hosts or for long-running commands, prefer
`--server-token-file` so the token does not appear in process arguments.

Passing `--pg` opens the configured PostgreSQL read store directly. It works
well for automation running away from the UI server, though it is read-only:
`session sync` and `session export` reject it. Even when `AGENTSVIEW_PG_URL` or
`[pg].url` is configured, read commands still use local SQLite unless you pass
`--pg`.

When the CLI reads over HTTP, JSON responses for session list, detail, sync,
and search include `web_url` links to the selected server's browser view. These
links preserve the server's base path and separate the provider and session ID
into URL segments, for example `/sessions/codex/<uuid>`. Use the returned link
rather than building your own. Direct `--pg` reads omit `web_url` because no
browser address is known.

## Common flags

| Flag                         | Description                                                                                                                                                                                    |
| ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--format human\|json`       | Output format. Default `human`.                                                                                                                                                                |
| `--json`                     | Alias for `--format json`.                                                                                                                                                                     |
| `--server <url>`             | Explicit daemon URL for HTTP-backed operations.                                                                                                                                                |
| `--server-token-file <path>` | Bearer token file for an explicit `--server` URL.                                                                                                                                              |
| `--machine <value>`          | Filter bare Codebuff/Freebuff timestamp resolution to one machine (`local`, ID, alias, name, or `*`). Default `local`. `session get` only; canonical IDs and other agents are unaffected.      |
| `--pg`                       | Read from configured PostgreSQL instead of SQLite.                                                                                                                                             |

## Shared metadata endpoints

After subscribing to changes, the HTTP `/api/v1/events` stream sends
`events_ready` on every connection. If you want updates for specific sessions on
the same stream, you can repeat the `session` query parameter. For each one, the
stream sends `session_ready` and `session.timing`, or `session_missing` if the
initial lookup finds that the session does not exist or cannot be resolved. The
nonempty `session_ready` and `session_updated` SSE IDs use `v1:` followed by an
opaque hash that changes when the session state or a direct child changes. An
empty ID means the state could not be read. A session that becomes unresolved
also gets `session_missing` and is no longer watched until the client
reconnects. When you reconnect, the stream sends the readiness events again. To
receive the changed session and message records themselves with a cursor for
resuming, use the
[change stream](/docs/conversation-export/#follow-changes-live).

The web UI and generated API clients fetch filter options from these shared
metadata endpoints:

```http
GET /api/v1/projects
GET /api/v1/machines
GET /api/v1/branches
GET /api/v1/agents
```

`GET /api/v1/machines` returns the picker catalog and applies visibility flags
to its `machines` array. When you pass a selector such as
`?machine=Workstation`, the response adds a separate `resolved_machines` array
with the canonical selector IDs, even if those IDs have no visible or stored
sessions. Without a selector, the response omits `resolved_machines`. Visibility
flags never limit resolution. Unknown or ambiguous selectors return HTTP 400
along with known choices.

`GET /api/v1/branches` returns distinct `(project, branch)` pairs and an opaque
`token` field:

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
branch-aware endpoints. Treat it as opaque and URL-encode it in manual HTTP
calls. Because the token is scoped to both project and branch, `app-a/main` and
`app-b/main` stay distinct, and an empty branch stays distinct from a literal
`unknown` branch.

## Commands

### `agentsview session get`

Fetch metadata and computed signal fields for a single session. The response
shape matches `GET /api/v1/sessions/{id}`.

```bash
agentsview session get <id> [--format json]
```

**Bare timestamps on remote stores.** Codebuff and Freebuff sessions use
canonical IDs shaped like `<agent>:<project>:<timestamp>`, so a bare timestamp
such as `2026-07-16T00-09-00.236Z` is not a canonical ID. Against the local
archive, the CLI resolves a bare timestamp to its canonical ID by walking your
configured Codebuff and Freebuff roots, filtered by `--machine` (default
`local`). Against a remote store (`--server` or `--pg`), the CLI rejects a bare
Codebuff or Freebuff timestamp with an explicit error pointing you to
`session list`, because a bare timestamp is ambiguous across machines and
projects. This rejection applies only to the Codebuff/Freebuff timestamp shape;
bare IDs that are not timestamps (such as Codex, Copilot, or Gemini UUIDs)
continue to resolve through the registered agent prefix retry on remote reads,
the same way they do locally. When you know the full canonical ID, pass it on
remote reads:

```bash
# Local reads: bare timestamp OK (--machine=local by default).
agentsview session get 2026-07-16T00-09-00.236Z
agentsview session get 2026-07-16T00-09-00.236Z --machine=*

# Remote reads: pass the canonical ID from `session list`.
agentsview session get codebuff:myproject:1704067200 --pg
agentsview session get freebuff:myproject:1704067200 --pg
agentsview session get host~codebuff:myproject:1704067200 --server http://remote
agentsview session get host~freebuff:myproject:1704067200 --server http://remote
```

```json
{
  "id": "abc-123",
  "project": "myapp",
  "machine": "workstation",
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
  "secret_leak_count": 0,
  "labels": [],
  "pr_links": []
}
```

Both HTTP and CLI responses populate `health_score_basis` and
`health_penalties` whenever `health_score` is non-null.

`git_branch` holds the branch captured at sync time when the parser or source
metadata exposes it; sessions without a recorded branch omit the field.
`parser_malformed_lines` counts any malformed source lines a parser skipped
while recovering the session, and the response omits it when the count is zero.
Antigravity detail responses may also include `decode_confidence`, where `low`
means the session came from an unrecognized Antigravity schema fingerprint and
was decoded heuristically.

`labels` lists the session's [labels](#agentsview-session-label) and
`pr_links` lists its [pull request links](#pull-request-links). Both fields are
always present and appear as `[]` when empty. Human output prints them along
with the parent session if one exists.

`secret_leak_count` (added in 0.30.0) counts definite-tier findings from
[secret scanning](#secret-scanning) and is stamped inline during sync.
Candidate-tier findings appear only after you run
[`agentsview secrets scan --backfill`](/docs/commands/#agentsview-secrets) and
do not contribute to this count.

______________________________________________________________________

### `agentsview session list`

List sessions with optional filters. The session fields match
`GET /api/v1/sessions`, and CLI JSON also includes a `machine_labels` catalog.

```bash
agentsview session list [flags]
```

```json
{
  "sessions": [ ... ],
  "next_cursor": "...",
  "total": 42,
  "machine_labels": {
    "machine-key": "Build Host"
  }
}
```

By default, `session list` excludes one-shot, automated, and child sessions.
When the first CLI page hides any of them, `session list` writes an advisory to
stderr with the hidden count for each category and the flags that reveal them:
`--include-one-shot` for one-shot sessions and `--include-automated` for
automated sessions. For hidden child sessions, the notice names
`--include-children` together with whichever of `--include-one-shot` and
`--include-automated` are not already set, for example
`--include-one-shot --include-automated --include-children`. Human stdout stays
unchanged and the JSON catalog is additive, so redirecting or piping structured
output remains safe. The JSON `total` still describes the filtered result rather
than the excluded sessions. Pass the `--include-*` flags to opt back in.

On HTTP, pass `exclusion_counts=false` to get hidden one-shot and automated
counts, or `exclusion_counts=true` to also include the bounded child count. The
optional `exclusion_counts` object contains `one_shot`, `automated`,
`children`, and `children_lower_bound`. Cursor pages and fully included lists
omit it. Other API callers leave this parameter unset to avoid the extra work.
If a server omits the summary, it still returns a usable page and the CLI skips
the exclusion notice.

Machine filters accept exact IDs, aliases, comma-separated ID or alias lists,
or a unique whole display label. Exact ID and alias lists take precedence over
a label with the same spelling; otherwise, the CLI matches the complete input
against labels, including labels that contain commas. A valid 32-character
hexadecimal installation ID takes precedence over a same-text label even
without stored sessions. Unknown or ambiguous selectors return HTTP 400 with
code `invalid_machine_filter`.

In JSON output, `sessions[].machine` keeps the machine key, so look it up in
the top-level `machine_labels` map when you need a display name. Keys without a
stored label do not appear in the map, and labels for machines outside the
current page are omitted. If the CLI cannot read the catalog, it still returns
the session list, emits `machine_labels: {}`, and prints a warning to stderr.
Human output does not read the catalog.

Pass `session list --json --include-source` to include each session's recorded
`file_path`, `file_size`, and `local_modified_at` when the backend stores them.
These source fields are omitted by default, and the flag adds no column to
human output. Over HTTP, pass `include_source=true`. PostgreSQL stores the
source path but does not store file size or archive-row update time.

To compare copies across hosts, fetch their metadata in one request:

```http
GET /api/v1/sessions?ids=codex:abc,codex:def&include_source=true
```

The HTTP-only `ids` parameter accepts 1 to 100 comma-separated IDs. If an ID
contains a comma or line break, quote it with RFC 4180 CSV quoting and double
any internal quote characters; for example, the decoded query value
`ids="openclaw:main:part,part"` selects one ID. When none of the IDs contain a
comma or line break, quote characters are treated literally. The server trims
whitespace around members and returns duplicate IDs once, while empty members
or more than 100 members return HTTP 400. A raw ID matches itself and copies
whose IDs end in `~<raw-id>`, whereas an ID containing `~` matches exactly.
Unknown IDs return no rows, and you cannot select IDs containing a CRLF
sequence through this parameter. Direct selection includes child, automated,
one-shot, and empty sessions, while deleted sessions stay excluded. Any other
explicit filters intersect the selected rows, and normal sorting, limits, and
cursor pagination still apply. Hosted PostgreSQL resolves public IDs first and
rejects ambiguous copies with its existing identity-conflict response.

When choosing between copies, compare `cwd`, `is_truncated`, `message_count`,
`file_size`, `local_modified_at`, and the session timestamps. Source metadata
and timestamps help you compare copies, though they do not guarantee a complete
transcript. `local_modified_at` tracks changes to the archived session row,
including custom names and later metadata edits, rather than the source file's
modification time. Labels and launcher parent links advance only the mirror
sync marker.

Date filters match any session whose activity window overlaps the selected date
or range. A session that starts before midnight and stays active after midnight
therefore appears on both dates.

| Flag                  | HTTP param          | Notes                                                                                                                                                                             |
| --------------------- | ------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--project`           | `project`           | string                                                                                                                                                                            |
| `--exclude-project`   | `exclude_project`   | string                                                                                                                                                                            |
| `--machine`           | `machine`           | string                                                                                                                                                                            |
| —                     | `git_branch`        | opaque token from `GET /api/v1/branches`                                                                                                                                          |
| `--agent`             | `agent`             | string                                                                                                                                                                            |
| `--date`              | `date`              | `YYYY-MM-DD`                                                                                                                                                                      |
| `--date-from`         | `date_from`         | `YYYY-MM-DD`                                                                                                                                                                      |
| `--date-to`           | `date_to`           | `YYYY-MM-DD`                                                                                                                                                                      |
| `--active-since`      | `active_since`      | RFC3339 timestamp                                                                                                                                                                 |
| `--since`             | `active_since`      | Relative — `Nh` hours, `Nd` days, `Nw` weeks, `Nm` calendar months (not minutes), `Ny` years — or `YYYY-MM-DD`; resolved against now and mutually exclusive with `--active-since` |
| `--resume`            | `active_since`      | CLI shortcut for sessions active in the last 15 minutes                                                                                                                           |
| `--active`            | `active_since`      | Alias for `--resume`                                                                                                                                                              |
| `--min-messages`      | `min_messages`      | int                                                                                                                                                                               |
| `--max-messages`      | `max_messages`      | int                                                                                                                                                                               |
| `--min-user-messages` | `min_user_messages` | int                                                                                                                                                                               |
| `--include-one-shot`  | `include_one_shot`  | bool                                                                                                                                                                              |
| `--include-automated` | `include_automated` | bool                                                                                                                                                                              |
| `--include-children`  | `include_children`  | bool                                                                                                                                                                              |
| `--include-source`    | `include_source`    | bool; include available source path, size, and archive-row update time, hidden by default                                                                                                                      |
| `--outcome`           | `outcome`           | comma-separated                                                                                                                                                                   |
| `--health-grade`      | `health_grade`      | comma-separated                                                                                                                                                                   |
| `--min-tool-failures` | `min_tool_failures` | int; `0` is a meaningful filter                                                                                                                                                   |
| `--has-secret`        | `has_secret`        | bool — only sessions with at least one definite [secret finding](#secret-scanning)                                                                                                |
| `--label`             | `label`             | repeatable; exact label match, every listed label required                                                                                                                        |
| `--pr`                | `pr`                | `owner/repo`, `owner/repo#123`, or `github.com::owner/repo#123`; see [Pull request links](#pull-request-links)                                                                              |
| `--sort`              | `order_by`          | comma-separated keys; optional `:asc` / `:desc` suffix per key                                                                                                                    |
| `--reverse`, `-r`     | `descending`        | flips the default direction for unsuffixed sort keys                                                                                                                              |
| `--cursor`            | `cursor`            | opaque string from prior response                                                                                                                                                 |
| `--limit`             | `limit`             | int; default 200, max 500                                                                                                                                                         |

Available sort keys are `recent`, `started`, `messages`, `user-messages`,
`output-tokens`, `peak-context`, `failures`, `retries`, `edit-churn`,
`compactions`, `context-pressure`, `health`, `secrets`, and `id`. `recent`
sorts descending by default, while the other keys sort ascending unless you
override them with `--reverse`, `descending=true`, or an explicit suffix.

Human CLI output is laid out for resuming work: it displays the full session
ID, age, agent, project, branch, message count, title, and working directory,
and marks sessions active in the last 15 minutes. Passing `--resume` or
`--active` sets `active_since` to that 15-minute window unless you also pass
`--active-since`. Over HTTP, pass `active_since` directly.

Examples:

```bash
agentsview session list --resume
agentsview session list --sort messages:desc,started:asc
agentsview session list --sort health --reverse
```

______________________________________________________________________

### `agentsview session compactions`

Read full compaction summaries for one session or for sessions active since an
RFC3339 timestamp. RooCode and legacy Kilo sessions are excluded. Older Evener
labels that may be literal summary text appear separately as ambiguous, along
with their session ID and ordinal. The session limit does not cap response
bytes, and `--limit 0` uses the default of 20 sessions. Use `--limit` and
`--cursor` to page through sessions, and pass the same `--active-since` value
when resuming a cursor, even if a page has no summaries.

```bash
agentsview session compactions <id> --json
agentsview session compactions --active-since 2024-01-01T00:00:00Z --limit 20 --json
```

In linear reads, the messages API accepts `compact_boundary=true` to return
only compaction boundaries. You cannot combine it with `around`.

### `agentsview session messages`

Fetch a window of messages from a session. The response shape matches
`GET /api/v1/sessions/{id}/messages`.

```bash
agentsview session messages <id> [--from N] [--limit N] [--direction asc|desc]
agentsview session messages <id> --around N [--before N] [--after N] [--role user,assistant]
```

At the service layer, `--from` is pointer-valued: omitting it starts at the
beginning for ascending reads and at the newest page for descending reads,
while passing `--from 0` explicitly starts at ordinal 0 in either direction.
The CLI validates `--direction` to `asc` or `desc`.

Window and role flags support the cursor-follow workflow described in
[Semantic Search](/docs/semantic-search/#cursor-follow-from-a-hit-to-its-surrounding-conversation):

| Flag       | HTTP param | Notes                                                                           |
| ---------- | ---------- | ------------------------------------------------------------------------------- |
| `--around` | `around`   | Center a window on this ordinal; mutually exclusive with `--from`/`--direction` |
| `--before` | `before`   | Messages before the anchor (default 5); requires `--around`                     |
| `--after`  | `after`    | Messages after the anchor (default 5); requires `--around`                      |
| `--role`   | `roles`    | Comma-separated roles to include, e.g. `user,assistant`                         |

When you pass a `--role` filter, `--before` and `--after` count only the
filtered messages, and the anchor message is always included. Each response
reports `first_ordinal` and `last_ordinal` for the returned window so you can
keep paging with `--from <last_ordinal + 1>`.

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

`thinking_text` holds the concatenated text of any `thinking` blocks the agent
emitted. It is separate from the flattened `content`, which keeps inline
`[Thinking]...[/Thinking]` markers for UI rendering.

On `is_system: true` messages, promoted `source_subtype` values include
`continuation`, `resume`, `interrupted`, `task_notification`, `stop_hook`,
`peer_message`, `compact_boundary`, `hook_success`, `hook_additional_context`,
`hook_cancelled`, and `stop_hook_summary`. The three `hook_*` subtypes carry
Claude hook output with `source_type: "attachment"`, while `stop_hook_summary`
carries stop hook results with `source_type: "system"`. A `peer_message` comes
from another Claude Code session; its `content` keeps the
`<cross-session-message>` wrapper with the sender's name, and may include a
framing line before the wrapper and a notice after it.

______________________________________________________________________

### `agentsview session tool-calls`

List every tool invocation in a session in chronological order.

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

`input_json` is always a string. It usually holds a serialized JSON object, but
it may be a plain string, for example `"echo hello world"` from Codex.

______________________________________________________________________

### `POST /api/v1/sessions/{id}/resume`

Resume a local session in its native agent, or return the command that
AgentsView would run. This HTTP-only endpoint backs the web UI's resume menu.

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

Setting `command_only: true` returns the command without launching a terminal.
If a launch attempt fails, the response still includes the command along with
`launched: false` and an `error` code of `no_terminal_found` or
`launch_failed`. Read-only local mode can still return commands, while remote
sessions and read-only remote serving cannot launch local programs.

For Claude Code sessions, passing `fork_session: true` without `from_ordinal`
appends Claude's native `--fork-session` flag to the normal resume command.
Passing both `fork_session: true` and `from_ordinal` creates a message-point
fork: AgentsView renders the transcript through that message ordinal into a
temporary prompt and runs `claude < prompt` from the resolved session working
directory. Message-point forks work only for Claude, require `fork_session`,
reject `opener_id`, and return `404` if the ordinal is not present.

______________________________________________________________________

### The `agentsview://` URL scheme

The installed desktop app registers the `agentsview://` URL scheme so external
tools can open a session in the app without going through HTTP:

```bash
open "agentsview://sessions/<session-id>"
```

Contract:

- The app accepts only `agentsview://sessions/<session-id>` URLs. The session
  ID must be a single path segment with reserved characters percent-encoded.
  Anything else is logged to the desktop log and ignored.
- Pass an optional `?msg=<ordinal>` or `?msg=last` query to scroll to that
  message. The app drops other query parameters.
- If the app is already running, it focuses its window and navigates to the
  session. Otherwise, it launches and opens the session once its backend is
  ready. On Windows and Linux, a second launch forwards its URL to the running
  instance and exits.
- You can detect a missing handler because `open` on macOS and equivalent
  launchers exit non-zero when no app claims the scheme. Fall back to the web
  UI at `http://<host>:<port>/sessions/<session-id>` when that happens.

Only the installed desktop app registers this scheme; it is unavailable when
you install only the CLI or server.

______________________________________________________________________

### `agentsview session export`

Stream a session's raw source file to stdout. This **local-only** filesystem
helper resolves the source file path from your local SQLite archive and never
contacts a daemon. Because it streams raw bytes, structured-output and
remote-store flags do not apply: the command rejects `--server`, `--pg`, and
`--format`/`--json`.

By default, `session export` masks detected secrets such as API keys, tokens,
and private keys, using the same detector and masked form as search results.
It reads the whole source before writing. For a JSON Lines source, it masks
inside each record's decoded strings so JSON escapes like `\n` cannot hide a
key and every record stays valid JSON. For any other source, it masks the file
as a whole so a key block spanning multiple lines is still caught. Pass
`--reveal` to stream the source unchanged.

```bash
agentsview session export <id>
agentsview session export <id> --reveal
```

Exit states:

| State                                  | Behavior                                         |
| -------------------------------------- | ------------------------------------------------ |
| Session in local archive, file on disk | Streams bytes, secrets masked unless `--reveal`  |
| Session in local archive, file missing | Exit 1: error prefixed `source file not found`   |
| Session in local archive, path empty   | Exit 1: `source file not found for session <id>` |
| Session not in local archive           | Exit 1: `session not in local archive: <id>`     |

For a DB-derived HTML or Markdown export, use the HTTP endpoints
`/api/v1/sessions/{id}/export` or `/api/v1/sessions/{id}/md`. They mask
secrets as well; see [secrets in exports](/docs/usage/#secrets-in-exports).

Markdown export accepts an optional `depth` query parameter:

- omitted: root session only
- `depth=1`: include direct child and subagent sessions
- `depth=all`: recurse through the child-session tree

The endpoint rejects any other `depth` value.

______________________________________________________________________

### `agentsview session sync`

Parse and insert a single session, blocking until indexing and signal
computation finish. JSON output returns the synced session's `SessionDetail`,
while human output prints one line: `synced: <id>`.

```bash
agentsview session sync <path-or-id>
```

If the argument points to an existing filesystem path, `session sync` parses it
as a raw JSONL file. Otherwise, it treats the argument as a session ID to
re-parse.

When you pass `--server <url>`, path-shaped arguments such as
`/var/log/agent/session.jsonl` or `./session.jsonl` are sent to the remote
daemon as paths and resolved on that daemon host. Bare values without path
separators are treated as session IDs.

If a single JSONL file maps to more than one session (for example, a Claude
transcript with forked or resumed branches), `session sync <path>` refuses with
an error listing every candidate ID. Because the contract is one session in and
one `SessionDetail` back, the CLI never picks one arbitrarily.

Because the ambiguity check runs after the sync engine finishes, the file has
already been parsed and every candidate session is written to the local
archive. Re-run `session sync <id>` with the specific session you want. The
`<id>` form resolves the file path from the archive, so it only works for
sessions already stored there.

- `session sync` uses a writable local daemon when one is running, or starts a
  detached daemon when no compatible daemon is running. It then proxies to
  `POST /api/v1/sessions/sync` so parsing and signal computation stay owned by
  the daemon.
- If the local archive needs a rebuild, `POST /api/v1/sessions/sync` returns
  HTTP 409 with `X-Agentsview-Resync-Required: true`. Wait for the rebuild or
  run `agentsview source sync --full`, then retry the session sync.
- If a read-only [`pg serve`](/docs/pg-sync/#agentsview-pg-serve) daemon is
  running, sync refuses with a clear error.
- If `AGENTSVIEW_NO_DAEMON=1` is set, the CLI runs the sync in-process only
  after acquiring the local write-owner lock.

______________________________________________________________________

### Pull request links

Each session's `pr_links` field lists pull requests found in retained user
prompts, supported `gh pr` commands, child-agent prompts, or structured
transcript records. Supported actions are `view`, `checkout`, `edit`, `diff`,
`checks`, `comment`, `review`, `ready`, `merge`, `close`, and `reopen`.
Commands can name a pull request by URL or number. A number can use `-R` or
`--repo` with `owner/repo` or `host/owner/repo`.

```json
"pr_links": [
  {
    "url": "https://github.com/owner/repo/pull/123",
    "host": "github.com",
    "repository": "owner/repo",
    "number": 123
  }
]
```

AgentsView keeps one link per pull request. A bare number uses the session's
recorded repository when the command runs in the session's working directory.
An explicit `owner/repo` uses the recorded remote's host or a literal `GH_HOST`
in the command. Conflicting or missing host evidence leaves the number unlinked.
A host-qualified repository or a full PR URL supplies its own host. After a
Claude Bash call changes directory, later commands need a captured working
directory that matches the session's directory to use the recorded remote.
Child-agent prompts link full PR URLs as references. Prompt mentions indicate
a reference, not work on the pull request. Archives that dropped tool inputs
cannot recover command or child-prompt links. Push-to-head linking is planned
for slice 2.

Claude Code also includes `pr-link` entries in its transcripts. Every session
split from that transcript carries them like its name and git branch.

To filter by pull request, use `session list --pr` or the `pr` query parameter.
Pass `owner/repo` for any pull request in that repository, `owner/repo#123` for
one pull request, or `github.com::owner/repo#123` to restrict matches to a
host. Matching is case-insensitive. Omitting the host matches any host,
including nested repository paths such as `group/subgroup/repo`. Use `::` to
separate a host from the repository so dotted group names such as
`my.org/sub/repo` stay hostless.

#### Filtering by label or pull request

Label and pull request filters select the sessions you name, the same way `ids`
does. Hosted raw sync carries no archive labels or launcher parent links, and
its label filter matches nothing.

- In a flat list (`session list`, `GET /api/v1/sessions`, or MCP
  `list_sessions`), these filters match each session directly. Child,
  automated, and one-shot sessions are included, so a headless worker started
  by an orchestrator appears even though it is a child of the launching
  session. Passing `include_one_shot=false` explicitly still excludes one-shot
  sessions, and passing `include_automated=false` explicitly excludes automated
  sessions.
- In tree views (the web UI sidebar and `include_children=true`), they keep a
  top-level session whenever it or any descendant matches, and show its whole
  tree. Filtering by a worker's pull request shows the orchestrator session
  with that worker inside it.

______________________________________________________________________

### `agentsview session label`

Add or remove free-form labels on a session so you can find every session for
one ticket, role, or kind of run. Annotation commands use the ID exactly as
given, with no prefix lookup, so pass the archive session ID, for example
`codex:<uuid>` for Codex. Using `key=value` labels such as `ticket=example` is
a convention, not a requirement. Both `session get` and session JSON display a
session's labels.

```bash
agentsview session label <id> ticket=example role=reviewer
agentsview session label <id> --remove role=reviewer
agentsview session label <id> --replace nightly          # replace every label
agentsview session label <id> --clear
agentsview session list --label ticket=example --label role=reviewer
```

Over HTTP, send `PATCH /api/v1/sessions/{id}/labels` with
`{"add": ["nightly"], "remove": ["x"], "clear": false}`. `clear` removes every
existing label first, so `{"clear": true, "add": [...]}` replaces the set and
`{"clear": true}` empties it. Removing a label that the session does not carry
is not an error. The response shape is
`{"session_id", "labels", "session_found"}`.

- AgentsView trims whitespace from labels. A label cannot be empty, start with
  `=`, contain control characters, or exceed 200 bytes. Each session holds at
  most 64 labels, and invalid input returns HTTP 400.
- The session does not need to exist yet. A launcher can label a session as
  soon as it knows the ID, and the labels appear when sync imports the session.
  `session_found: false` reports that case and also surfaces a mistyped ID.
- Because labels are stored separately from the transcript, a reparse or full
  resync never changes them. PostgreSQL and ClickHouse receive labels on push
  and support the `label` filter, but you can write labels only through the
  local archive. Read-only daemons return HTTP 501.
- See [Filtering by label or pull request](#filtering-by-label-or-pull-request)
  for which sessions the filters return.

______________________________________________________________________

### `agentsview session parent`

Record which session launched another session. When an orchestrator starts
worker sessions as separate processes, this lets the session tree nest them
under the session that started them. Both `session get` and session JSON show
the resulting parent.

```bash
agentsview session parent <worker-id> <manager-id>
agentsview session parent <worker-id> --clear
```

Over HTTP, send `PUT /api/v1/sessions/{id}/parent` with
`{"parent_session_id": "<id>"}`. Passing an empty or omitted
`parent_session_id` removes the link. The request body accepts only
`parent_session_id`: it rejects `relationship_type`, and links always record
`subagent`.

When a link is applied, the worker becomes a subagent of the launching session
and the response reports `relationship_type` as `subagent`. The worker nests
under that session in the sidebar, and `session usage` includes its cost in
the launcher's total. The response also includes `session_found` and `applied`.

- The link applies only while the transcript names no parent of its own.
  Subagent, fork, and continuation links derived from the transcript take
  precedence, and `applied` is `false` when they do.
- AgentsView recomputes the link when writing sessions, linking spawn edges,
  and setting or clearing launcher links. If the transcript parent or loop that
  blocked it goes away, the session returns under the linked parent.
- Either session may be missing when you record the link. It stays pending with
  `applied=false` until both rows exist, leaving the worker's parent and
  classification unchanged in the meantime.
- The link is stored separately from the transcript and survives reparses and
  full resyncs. Mirrors receive the resulting parent on push.
- A session cannot be its own parent, and any link that would make a session
  its own ancestor returns HTTP 400. Clearing an absent link succeeds without
  changing the session.

______________________________________________________________________

### `agentsview session watch`

Stream NDJSON events as a session updates. Each line is a small JSON object
wrapping an SSE event.

```bash
agentsview session watch <id>
```

```
{"event":"session_updated","data":"abc-123"}
{"event":"heartbeat","data":"2026-04-18T12:05:00Z"}
```

Today, recognized `event` values are `session_updated` and `heartbeat`. New
events may be added, so ignore any unknown event types in your consumer. The
command runs until you interrupt it (Ctrl+C) or cancel its context. Like
`session export`, it streams a fixed format and rejects `--format`/`--json`.

On every connection, the HTTP session watch sends `session_ready` after taking
its baseline, and the CLI filters it out.

`session watch` validates the session ID before opening the stream. An unknown
ID fails fast with `watch: session not found: <id>` and a non-zero exit instead
of streaming heartbeats indefinitely, so typos in automation scripts show up
right away. When proxied to a daemon, the same condition comes back as HTTP 404
at the transport layer and is translated to the CLI error.

______________________________________________________________________

### `agentsview session search`

Run a substring, RE2 regex, or FTS5 search across message bodies, thinking,
tool inputs, and tool result content. The response shape matches
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
      "machine": "workstation",
      "display_name": "Database investigation",
      "ordinal": 17,
      "ordinal_range": [12, 24],
      "location": "tool_result",
      "tool_name": "Bash",
      "snippet": "...connecting to db with token ***REDACTED***...",
      "matched_words": 3
    }
  ]
}
```

Every content match includes `machine` and `display_name` across all supported
search modes and sources. `display_name` uses the user title, falls back to the
provider session name, and is `null` when neither exists. Session search at
`GET /api/v1/search` also includes `machine` alongside its existing `name`.

The content-search response includes `active_filter_applied`, which is `true`
when `exclude_active_since` ran before pagination. All current stores support
this filter, though older servers may omit the field.

On SQLite, PostgreSQL, and ClickHouse, the `messages` source searches dialogue
from structured text blocks in Pi, OMP, OMO, Prime Agent, StepCode, iFlow,
Claude Code, Cowork, iCodeMate CLI, Qoder, Cursor JSONL, Amp, OpenClaude,
OpenClaw, QClaw, Devin, and OpenHands. Those projections keep rendered tool
calls and reasoning out of snippets while preserving literal marker text in
dialogue. These parsers store reasoning blocks in `thinking_text`, including
Cursor JSONL and OpenHands. Substring and regex searches on SQLite, PostgreSQL,
and ClickHouse include `thinking` by default; pass `--in messages` for dialogue
alone or `--in thinking` for reasoning alone. FTS searches messages only, and
terms, semantic, and hybrid modes reject `thinking` as a source. Semantic
search still embeds full message text, while hybrid's keyword leg searches
dialogue. Ordinary session search and the command palette continue to search
the full rendered transcript.

When source files are available, AgentsView reparses existing sessions from
these providers. Other providers and parser paths, sessions without source
files, imported v4 artifacts, and unpaired OpenHands observations keep
searching their stored content as `message` hits. On the first writable open,
SQLite refreshes its full-text indexes once; startup waits for this scan, and
later opens reuse the indexes. Every message indexes its full rendered text in
`rendered` and `COALESCE(dialogue_text, content)` in `content`. Ordinary
session search matches `rendered` unless your query names an FTS column, while
FTS content search and hybrid's keyword leg match `content`. Session-search
ordering may shift as a result. After upgrading, the first ClickHouse push is a
full push to populate dialogue.

PostgreSQL schema setup adds the dialogue column and builds trigram indexes on
dialogue and reasoning beside the existing `content` index. The dialogue index
is close to the size of the `content` index, the reasoning index grows with
stored reasoning, and both slow message writes. Building them on an existing
mirror blocks writes to `messages` until each index finishes. The first push
after upgrading publishes every session in its scope once to fill existing
rows. A restricted hosted runtime role needs `UPDATE(dialogue_text)` on
`messages`. For hosted raw uploads, run
`agentsview replica pg raw-reparse hosted --run-id <id>` after upgrading the
server, repeating the same run ID until it reports `complete=true`. Changing
dialogue derivation alone keeps session IDs, content revisions, and embeddings
intact. A parser upgrade that changes other fields, such as Pi reasoning text,
produces a new content revision and session ID.

Artifact transport is a separate follow-up. Transcript display, block order,
and artifact format v4 remain unchanged.

By default, search excludes one-shot, automated, and subagent sessions; pass
`--include-one-shot`, `--include-automated`, or `--include-children` to opt
back in. HTTP substring and regex searches also accept `scope`. Leaving `scope`
unset keeps these exclusions, while an explicit `scope` uses each session's own
filters and exempts child sessions from the one-shot exclusion.

| Flag                  | HTTP param             | Notes                                                                                                                                                                             |
| --------------------- | ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--regex`             | `mode=regex`           | Treat pattern as an RE2 regex                                                                                                                                                     |
| `--fts`               | `mode=fts`             | Tokenized FTS5 search; messages-only                                                                                                                                              |
| `--semantic`          | `mode=semantic`        | Vector search over user/assistant messages; messages-only — see [Semantic Search](/docs/semantic-search/)                                                                         |
| `--hybrid`            | `mode=hybrid`          | Semantic + FTS reciprocal rank fusion; messages-only — see [Semantic Search](/docs/semantic-search/)                                                                              |
| `--scope`             | `scope`                | `top`, `all`, or `subordinate`; CLI: semantic/hybrid only, default `all`. HTTP: substring/regex/terms/semantic/hybrid; explicit scope supersedes `include_children`                                                                     |
| `--context`           | `context`              | int — N messages of context before/after each match (max 10)                                                                                                                      |
| `--in`                | `in`                   | Comma-separated: `messages,thinking,tool_input,tool_result` (default all); `thinking` requires SQLite, PostgreSQL, or ClickHouse substring/regex                                                                                                                          |
| `--exclude-system`    | `exclude_system`       | Drop system messages from the scan                                                                                                                                                |
| `--reveal`            | `reveal`               | Show full secret values (localhost-only; warning to stderr)                                                                                                                       |
| `--project`           | `project`              | string                                                                                                                                                                            |
| `--exclude-project`   | `exclude_project`      | string                                                                                                                                                                            |
| `--machine`           | `machine`              | string                                                                                                                                                                            |
| —                     | `git_branch`           | opaque token from `GET /api/v1/branches`                                                                                                                                          |
| `--agent`             | `agent`                | string                                                                                                                                                                            |
| `--date`              | `date`                 | `YYYY-MM-DD`                                                                                                                                                                      |
| `--date-from`         | `date_from`            | `YYYY-MM-DD`                                                                                                                                                                      |
| `--date-to`           | `date_to`              | `YYYY-MM-DD`                                                                                                                                                                      |
| `--active-since`      | `active_since`         | RFC3339 timestamp                                                                                                                                                                 |
| `--since`             | `active_since`         | Relative — `Nh` hours, `Nd` days, `Nw` weeks, `Nm` calendar months (not minutes), `Ny` years — or `YYYY-MM-DD`; resolved against now and mutually exclusive with `--active-since` |
| `--include-children`  | `include_children`     | bool                                                                                                                                                                              |
| `--include-automated` | `include_automated`    | bool                                                                                                                                                                              |
| `--include-one-shot`  | `include_one_shot`     | bool                                                                                                                                                                              |
| `--exclude-session`   | `exclude_session`      | repeatable session ID to drop before `--limit`                                                                                                                                    |
| —                     | `exclude_active_since` | RFC3339 cutoff; exclude sessions active after it before pagination, using `ended_at`, then `started_at`, then `created_at`; keep unknown or malformed activity                    |
| `--limit`             | `limit`                | int; default 50, max 500                                                                                                                                                          |
| `--cursor`            | `cursor`               | int — pagination cursor from a previous response                                                                                                                                  |

You cannot combine `--regex`, `--fts`, `--semantic`, and `--hybrid`. `--fts`
is the fastest mode on large archives, though it searches message bodies only.
Substring (the default) and regex modes also scan `tool_calls.input_json`,
`tool_calls.result_content`, and `tool_result_events` rows. `--semantic` and
`--hybrid` require an embedding index and return a single ranked page (passing
`--cursor` is rejected); see [Semantic Search](/docs/semantic-search/) for
setup, scoring, and limitations.

On SQLite, PostgreSQL, and ClickHouse, a default multi-word search that
includes `messages` returns exact text matches first, then messages ordered by
how many query words they contain, with that count in `matched_words`. Unquoted
stop words do not count, regular word forms count, and a quoted phrase inside
the query requires its words together and in order. `--fts` and hybrid's
keyword leg use the same word matching unless the query starts with a quote and
holds only quoted phrases or FTS5 operators, which keeps FTS5 behavior. Other
searches report `matched_words` as `0`.

On PostgreSQL, an explicit `--fts` query starting with a quote honors `AND`,
`OR`, `NOT`, and parentheses as SQLite does. `NEAR(...)` and prefix `*`
are not supported there, and `*` matches literally.

Every match in every mode carries the conversation-unit citation described in
[Hit shape](/docs/semantic-search/#hit-shape-ranges-and-anchors):
`ordinal_range` gives `[start, end]` of the conversation unit containing the
match (always present, and `[ordinal, ordinal]` when the match is its own
unit), along with the lineage fields `subordinate`, `relationship`,
`parent_session_id`, and `is_sidechain`. `ordinal` stays the anchor for the
exact matched message in every mode. Only the lineage fields use `omitempty`,
so a missing key unambiguously means a top-level session with no lineage,
whereas `ordinal_range` is never omitted, even at `[0, 0]`. `score` is the only
field emitted exclusively by `--semantic` and `--hybrid`. Outside `--semantic`
and `--hybrid`, `--scope` is rejected; in those modes it supersedes
`--include-children`, and subagent, fork-typed, or parent-linked sessions are
exempt from the default one-shot exclusion.

Snippets include about 60 characters of context on each side of the match,
snapped to rune boundaries. Any substring matching the
[secret scanner](#secret-scanning) rule set is masked unless you pass
`--reveal`. The HTTP endpoint applies the same masking to remote callers, and
honors `reveal=true` only on a localhost-bound daemon.

______________________________________________________________________

### `agentsview session usage-batch`

Read usage for several sessions in a single CLI process. Local and PostgreSQL
modes select the backend and open the archive or connection once, while
`--server` still sends a request for each ID. IDs resolve the same way they do
for `session usage`, including short IDs, and repeated IDs produce repeated
results in input order.

```bash
agentsview session usage-batch <id>... [--format json] [--own-only] [--no-sync]
```

This command accepts the same `--server`, `--server-token-file`, and `--pg`
options as `session usage`. Descendant usage and source refresh follow the
single-session rules below.

JSON output always wraps entries in a `results` array, even for a single ID.
Each entry includes `requested_id`, `exit_code`, and `usage` (matching the
`session usage --json` shape) when available. Per-session failures also include
`error` text: code 2 marks a missing session, code 3 marks a session without
token data, cost, or plan usage figures, code 0 means token data, cost, or
Codex Cloud plan usage figures are available, and code 1 covers other lookup
errors. The command continues processing the remaining IDs after a per-session
failure.

```json
{
  "results": [
    {
      "requested_id": "missing-session",
      "exit_code": 2,
      "error": "session not found: missing-session"
    }
  ]
}
```

Human output prints each requested ID and its exit code before its usage or
error. The process exits 0 after printing all results, even when individual
sessions fail. Backend setup or output failures exit 1. The existing
`session usage <id>` output and exit codes stay unchanged.

### `agentsview session diagnose`

Run `agentsview session diagnose <id> --json` to inspect archived usage and
evidence when investigating an expensive task. It uses the same pricing, linked
subagent accounting, and source refresh as `session usage`. Pass `--no-sync` to
read archived usage without refreshing source transcripts. If a refresh fails,
human output warns that the refresh is incomplete and JSON sets
`usage.refresh_incomplete`. Pass `--own-only` to exclude subagent usage while
still listing child links. Human output is the default, `--pg` and `--server`
select the same read transports as other session commands, and missing sessions
or read failures exit 1.

The versioned JSON includes the usage breakdown, totals of its normalized
uncached input, cache-read, cache-write, and output rows, context observations,
compaction ordinals and inferred counts, the ten largest recorded tool-result
lengths, and runs of three or more consecutive calls with identical recorded
arguments. Tool and context evidence covers only the named session, though
child links appear when recorded. Retry candidates use the same
recorded-argument comparison as the `tool_retry_count` signal; empty or
placeholder arguments can form candidates and do not prove that a provider
retried a request.

These observations can be incomplete: usage rows are not always requests,
request counts and provider cache-miss reasons are unavailable, and result
lengths are not billed tokens. Cost is reported or estimated under the existing
usage contract, with no causal savings or invoice total inferred, and the
report omits transcript content and tool arguments. Context, compaction, and
tool evidence come from a paged scan of all archived messages. Human output
shows the latest ten context observations, while JSON includes every
observation. Usage is fetched in a separate read and can reflect a different
archive revision if ingestion runs concurrently. Because message pages do not
share a snapshot, a transcript rewritten during the scan can mix revisions; run
the command again after the rewrite for a stable result. `schema_version`
versions the output shape, not the archive snapshot.

Compactions follow ingestion's detection rules. Explicit boundary markers
appear in `compaction_ordinals`. When there are no explicit markers,
`inferred_compaction_count` counts drops greater than 30% between successive
reported assistant context measurements, including across message pages, while
skipping missing measurements. Explicit markers take precedence and leave the
inferred count at zero.

### `agentsview session usage`

Get per-session token usage and cost estimates. The output shape is stable for
the fields shown below, and new fields may be added. MCP clients read the same
figures through the
[`get_session_usage`](/docs/mcp/#session-details-reports-and-guides) tool.

Totals cover the named session **and every subagent transcript spawned beneath
it**. Claude Code writes each Task-tool subagent to its own file under
`<session>/subagents/`, which AgentsView ingests as a separate session;
reporting only the parent's own rows understated its cost by up to 77% in real
sessions. Attribution happens at read time: no rollup is persisted, no child
row is duplicated under the parent, and daily or activity aggregates (which
already count subagent sessions as first-class spend) stay unaffected. Pass
`--own-only` to report only the named transcript's rows, matching the
pre-0.40.0 output.

The REST endpoint `GET /api/v1/sessions/{id}/usage` is own-session by default.
Two query parameters widen it, and they do not compose: if you pass both,
`rollup=true` takes precedence and `subagents` is ignored for that request.

- `?subagents=true` folds descendant usage into `cost`, `total_output_tokens`,
  `models`, `breakdown`, and `breakdown_count`, and adds `subagent_count`.
  For a Codex Cloud chat, it also folds `capture_coverage`: `uncaptured_turns`
  sums across the root and its sub-agents, and `last_capture_at` takes the
  newest of theirs. `plan_usage` stays the root's report, which already counts
  its sub-agents. This is the form the CLI requests.
- `?rollup=true` keeps the main totals own-session and puts the combined cost
  in the separate `rollup_*` fields below. The session detail header uses this
  form. When you pass both `rollup=true` and `subagents=true`, the handler
  returns from the rollup branch first, so `subagents` has no effect.

```bash
agentsview session usage <id> [--format json] [--own-only] [--no-sync] [--wait]
```

```json
{
  "session_id": "abc-123",
  "agent": "claude",
  "project": "myapp",
  "total_output_tokens": 15230,
  "peak_context_tokens": 84000,
  "has_token_data": true,
  "cost": {"microdollars": 2410000},
  "has_cost": true,
  "cost_usd": 2.41,
  "cost_source": "computed",
  "models": ["claude-opus-4-7"],
  "unpriced_models": [],
  "breakdown_count": 42,
  "subagent_count": 2,
  "breakdown": [
    {
      "ordinal": 1,
      "message_ordinal": 0,
      "source": "message",
      "label": "Prompt 1",
      "timestamp": "2026-07-08T14:03:21Z",
      "model": "claude-opus-4-7",
      "input_tokens": 1200,
      "output_tokens": 640,
      "cache_creation_input_tokens": 0,
      "cache_read_input_tokens": 43000,
      "web_search_requests": 2,
      "cost": {"microdollars": 580000},
      "has_cost": true
    },
    {
      "ordinal": 2,
      "message_ordinal": 4,
      "source": "message",
      "label": "Prompt 5",
      "timestamp": "2026-07-08T14:05:02Z",
      "model": "claude-opus-4-7",
      "subagent_session_id": "agent-9f2c",
      "input_tokens": 800,
      "output_tokens": 310,
      "cache_creation_input_tokens": 0,
      "cache_read_input_tokens": 12000,
      "cost": {"microdollars": 190000},
      "has_cost": true
    }
  ],
  "server_running": false,
  "refresh_incomplete": true
}
```

| Field                   | Notes                                                                                                                                                                                                                                            |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `total_output_tokens`   | Generated output tokens across the session and its included subagents, deduplicated the same way `cost` is (see below)                                                                                                                           |
| `peak_context_tokens`   | Highest context-token count observed in any included session; peaks are high-water marks, so they are maxed rather than summed                                                                                                                   |
| `has_token_data`        | `false` when no included session has token usage                                                                                                                                                                                                 |
| `cost`                  | Model-pricing estimate as an integer microdollar object; zero when `has_cost` is `false`                                                                                                                                                         |
| `has_cost`              | `false` if any contributing row is unpriced — never reports a partial total as complete                                                                                                                                                          |
| `cost_usd`              | Deprecated compatibility alias for `cost.microdollars / 1e6`, present only when `has_cost` is `true`; will be removed in a future release, use `cost.microdollars` instead                                                                       |
| `cost_source`           | Omitted without a complete cost; `reported` for an authoritative session total, otherwise `computed`, `reported`, or `mixed` for the contributing rows                                                                                           |
| `ai_credits`            | Omitted unless the priced agent uses AI Credits; derived from `cost_usd` at 100 credits per dollar                                                                                                                                               |
| `models`                | Models with contributing usage, sorted by model name                                                                                                                                                                                             |
| `unpriced_models`       | Omitted from JSON when empty; lists models seen but missing from pricing                                                                                                                                                                         |
| `breakdown_count`       | Number of deduplicated per-step usage rows across all included sessions; always populated                                                                                                                                                        |
| `subagent_count`        | Number of subagent descendant sessions folded in; omitted when zero, so own-session results are byte-identical to before                                                                                                                         |
| `breakdown`             | Per-step usage rows ordered by timestamp across all included sessions (the queried session wins ties); when a reported session total exists, row costs are estimated allocations that sum to it; CLI JSON always includes them (added in 0.37.1) |
| `recorded_cost`         | Present with the breakdown when at least one row carries a cost the agent recorded itself. `recorded` sums those figures, `computed` is AgentsView's cost for the same rows, `difference` is `computed` minus `recorded`, and `rows`, `fallback_rows` (rows priced by the agent's figure because the catalog has no rate) and `unpriced_rows` count them. The human output shows it as an `Agent cost:` line |
| `session_total`         | Present when the agent recorded a total for the whole session (Claude Code `cost-state`). `source`, `subagent_scope` (`included`, `excluded`, or omitted when unknown), `cost`, `incomplete` when the agent could not price every model, and per-model `models` token counts and costs. It overlaps the usage rows, so it is never part of `cost` or the token totals; compare it with a result that includes subagents when `subagent_scope` is `included`. The human output shows it as an `Agent total:` line |
| `server_running`        | `true` when the report came from an already-running daemon                                                                                                                                                                                       |
| `refresh_incomplete`   | Present as `true` when a requested source refresh was refused or failed; the result still contains archived totals                                                                                                                                 |
| `wait_incomplete`      | Present as `true` when an archive-only wait failed; the result contains saved totals |
| `rollup_cost`           | REST only, with `rollup=true`; integer microdollar object present only when `has_rollup_cost` is true, carrying the complete cost across the root and explicit subagent descendants                                                              |
| `has_rollup_cost`       | REST only, with `rollup=true`; true only when at least one contributing row exists and every contributing row is priced                                                                                                                          |
| `rollup_subagent_count` | REST only, with `rollup=true`; count of reachable explicit subagent descendants, including those without usage rows                                                                                                                              |
| `plan_usage`            | Codex Cloud only; omitted for other agents and until AgentsView has stored a report or a check outcome for the chat. OpenAI's plan usage report for the chat: `data_as_of`, `data_status` (`available`, `partial`, or `unavailable`), `weekly_percent`, `five_hour_percent`, `credits` (a decimal string), per-model `groups`, `final`, and the newest `outcome_kind` and `outcome_at`. An outcome-only object, such as one with `outcome_kind` `account_unavailable`, has no figures. A sub-agent chat has `outcome_kind` `counted_under` and the root session ID in `counted_under`|
| `capture_coverage`      | Codex Cloud only; omitted when live capture never saw the chat. `last_capture_at`, when AgentsView last received a token update for the chat, and `uncaptured_turns`, the number of finished turns live capture did not fully see: no token update covers them directly or through a later turn's running total |

A Codex Cloud session that live capture never saw sets `has_token_data` and
`has_cost` to `false` and carries its figures in `plan_usage` instead. Human
output shows `n/a` for output and peak context, and when the session has plan
usage figures, it shows "billed as plan usage" for cost. A `Plan usage:` line
follows, along with a `Captured:` line if live capture saw the chat. If a later
check left the chat out or failed, the `Plan usage:` line reports what that
check found and notes that the figures may be out of date. For missed turns,
the `Captured:` line warns that some tokens may be missing from the totals.
When the newest report is complete and no later check failed, it adds that plan
usage counts turns that ran before the report's data time; AgentsView does not
know whether each missed turn ran before that time, so it does not claim more.

Claude Code echoes some parent turns into a subagent's sidechain transcript, so
the same message can appear in two sessions. Every combined figure counts it
once: `cost`, `total_output_tokens`, `breakdown`, and `breakdown_count` all
come from one deduplicated row set, so they agree with each other and with the
day aggregates. That is why the combined `total_output_tokens` can be lower
than the sum of the included sessions' individual `total_output_tokens`, since
each individual figure comes from its own transcript in isolation and still
contains the echo. A session with no per-message usage rows contributes its
session-level total instead, because it has nothing to deduplicate against.

Each `breakdown` row includes the fields shown in the example:

| Row field             | Notes                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| --------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ordinal`             | 1-based position of the row in the deduplicated usage stream                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `message_ordinal`     | Ordinal of the originating message; omitted when the row is not tied to one                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `source`              | `message` for per-message token usage; otherwise the usage-event source. Subagent rows keep their real source — they are marked by `subagent_session_id`, not by a different source                                                                                                                                                                                                                                                                                                                                  |
| `subagent_session_id` | Id of the subagent session the row came from; omitted for the queried session's own rows                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `label`               | Display label — `Prompt N` for message rows, `Step N` for other rows tied to a message, else the source name                                                                                                                                                                                                                                                                                                                                                                                                         |
| `web_search_requests` | Anthropic server-side web searches this row was billed for, at $0.01 each on top of tokens; omitted when the row performed none                                                                                                                                                                                                                                                                                                                                                                                      |
| `cost`                | Per-row integer microdollar object; includes the web search fee when `web_search_requests` is present and the row's cost is computed. A row with an authoritative reported cost is assumed to already settle its own server tool use, so the fee is not added on top even when `web_search_requests` is present. Zero with `has_cost: false` when the model is unpriced and the row performed no web search; an unpriced computed row that did still reports the fee, because the fee does not depend on token rates |
| `recorded_cost`       | The cost the agent recorded for this row, as an integer microdollar object; omitted when it recorded none. It prices the row only when the catalog has no rate for the model and it is above zero |
| `recorded_cost_priced` | Present as `true` when `recorded_cost` is the row's `cost` because the catalog has no rate for the model |

Human output is a compact summary:

```
Session:       abc-123
Agent:         claude
Output:        15230
Peak ctx:      84000
Subagents:     2 (included)
Cost:          ~$2.41 (claude-opus-4-7)
Agent total:   $2.38 for the session, including subagents
```

The `Subagents` line appears only when the figures cover subagent transcripts.
`Agent cost` and `Agent total` appear when the agent recorded its own figures;
see [Costs the Agent Records Itself](/docs/token-usage/#costs-the-agent-records-itself).

When refresh is incomplete, human output adds `Usage refresh is incomplete. Saved totals may be out of date.`.

The leading `~` on the cost line marks a computed or mixed figure, while a
reported cost omits it. The parenthesized model list appears only when
contributing models exist, so a cost-only reported session does not print empty
parentheses. When some contributing models are unpriced, the cost line reads
`n/a (unpriced: model-x)`; when the session has no token data at all, it reads
`n/a`. Priced Copilot-family sessions add an `AI Credits` line below the cost.

**HTTP endpoint.** As of 0.32.0, the same data is available over REST:

```http
GET /api/v1/sessions/{id}/usage
```

Starting in 0.47.0, this endpoint accepts raw agent session IDs in addition to
canonical archive IDs. Exact matches take precedence. If an exact lookup misses
or reports an ambiguous identity, an ID without a known agent prefix or `~`
host namespace is matched literally against stored IDs ending in `:` or `~`
followed by that ID. Trashed sessions do not count toward these matches. A
unique match returns `200` with the canonical archive ID in `session_id`,
across plain requests and all `subagents`, `rollup`, and `breakdown`
combinations. Explicit canonical IDs that are missing still return `404`.

Multiple matches return `409` with `error.code = "ambiguous_session_id"`:

```json
{
  "error": {
    "code": "ambiguous_session_id",
    "message": "multiple sessions match; retry with a canonical candidate ID",
    "candidates": ["archive-a~codex:thread-id", "archive-b~codex:thread-id"]
  }
}
```

Retry with one of the canonical IDs in `error.candidates`, which lists at most
five candidates and may omit additional matches. No match returns the `404`
shown below, and lookup failures return the same `500` as usage-query failures.
This fallback applies only to the usage endpoint; other session routes keep
their existing lookup behavior. The GET reads archived data without refreshing
or syncing source files.

The response uses the same JSON fields shown above and works against both local
SQLite-backed `agentsview serve` and read-only
[`agentsview replica pg serve`](/docs/pg-sync/#agentsview-pg-serve). HTTP
responses set `server_running: true`. As of 0.37.1, pass `?breakdown=true` to
include the per-step `breakdown` rows; without it, `breakdown` is `[]` while
`breakdown_count` still reports the row count. The CLI requests the breakdown
on every path (local, `--server`, and `--pg`), so `--format json` always
includes the rows. It also passes `subagents=true` unless you specify
`--own-only`, so all three backends return the same document. The session
detail header calls this endpoint to render its
[per-step usage breakdown](/docs/usage/#token-usage). Existing sessions return
`200 OK` even when token or cost data is absent, so inspect `has_token_data`,
`has_cost`, and `unpriced_models` to decide how to display that state. Missing
sessions return `404` with:

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

**Exit codes**:

| Code | Meaning                                                                        |
| ---- | ------------------------------------------------------------------------------ |
| `0`  | Token data, cost, or Codex Cloud plan usage figures present                    |
| `1`  | Query error or session unavailable during startup ingestion or archive rebuild |
| `2`  | Session missing after startup or rebuild, or from an archive-only/direct lookup |
| `3`  | Session exists but has no token data, cost, or plan usage figures              |

Exit codes classify the reported document, so a session whose only token data
lives in its subagents exits `0` rather than `3`, while passing `--own-only`
exits `3` as before. A Codex Cloud session with plan usage figures and no
captured tokens exits `0`. A sub-agent chat counted under its root, or a
session whose plan usage has not been reported, exits `3`.

By default, combined usage tries to refresh a known session and nearby Claude
subagent transcripts without waiting for startup ingestion, sending
`usage_refresh: true`, `id`, and `subagents: true` to
`POST /api/v1/sessions/sync`. If the refresh is refused or fails, the command
returns archived totals with a warning and sets `refresh_incomplete: true` in
JSON. While `GET /api/v1/sync/status` reports `startup_pending`,
`archive_stale`, `upgrade_pending`, or `sync_pending`, a missing daemon ID
returns a retry error; once all four are clear, it returns exit 2 without
`--wait`. A scoped parser upgrade can keep saved usage readable while the status
remains pending until a successful authoritative pass clears the marker.

Pass `--wait` to give HTTP lookup, ingestion, and refresh a shared 30-second
budget after daemon discovery. When startup is deferred, combined usage
requests it once and keeps watching status if that request fails, without
starting an archive rebuild. Once startup and rebuild are clear, a known
session may refresh while catch-up is pending, and a missing ID is looked up
repeatedly, including during catch-up. At the deadline, a confirmed absent ID
returns exit 2 if status is clear, while an incomplete lookup, pending work, or
unavailable status returns a retry error. Only a confirmed parent lock refusal
permits a refresh retry. If waiting fails or expires, the command reads
available saved totals and reports that the wait did not finish; an ID that is
still absent while work remains pending returns a retry error.

Daemon API 15 adds `sync_pending`, which stays true while ingestion catches up
after an idle period, so older local clients must upgrade to interpret it. The
field and catch-up warning clear when the pass finishes or fails, and failures
are logged. API 31 adds `upgrade_pending` for scoped parser freshness and
`writes_paused` for an active writer handoff. An incomplete upgrade keeps the
freshness warning after writes resume. `archive_stale` remains the full-rebuild
signal. Archive-data API responses include `X-Agentsview-Archive-Stale: true`
when a catch-up of changes held back by the background sync interval or an
idle pause does not finish within the read's wait. A read within two minutes
of another archive read starts no catch-up; the watcher keeps syncing at
`sync_interval` during that time, so the read can miss up to that interval
of changes. If an earlier read's catch-up is still running, the read shares
its remaining wait and is marked stale if that wait runs out. Otherwise, it
answers at once and is not marked stale. A required parser rebuild sets the
stale header independently.

With HTTP `--own-only`, the CLI reads usage from the archive and, if you pass
`--wait`, can start deferred ingestion. HTTP `--no-sync` reads the archive and
never starts source sync. If the wait fails, the command warns and sets
`wait_incomplete: true` in JSON without marking a refresh. Direct and
PostgreSQL reads do not wait for a daemon. HTTP archive-only waits require
daemon API 15, and waits that refresh require API 26; upgrade or restart an
older server, and upgrade the client for a newer server API. Direct mode still
refreshes the named transcript. Combined HTTP refresh requires daemon API 12
(upgrade or restart older servers, or pass `--no-sync`), and archived combined
reads require API 6. `POST /api/v1/sessions/sync` returns HTTP 409 for a stale
local archive for every caller, including `session sync`. A usage refresh also
returns HTTP 409 while startup ingestion is incomplete or another sync holds
the engine. Only a refusal before the parent refresh acquired its lock carries
the code `session_refresh_not_started`; other conflicts do not authorize a
refresh retry. When startup ingestion is deferred, its automatic fallback
starts after a 30-second grace period.

Pass `--no-sync` to read archived usage without refreshing source transcripts.
This keeps the full subagent rollup and the exit codes above; combine it with
`--own-only` only when you want to exclude subagents. Local and remote HTTP
queries skip the sync request, so recent usage appears after the watcher or a
separate sync indexes it, while PostgreSQL reads already use archived data. The
local `--no-sync` path requires a compatible daemon and starts one with source
synchronization disabled if needed. With `AGENTSVIEW_NO_DAEMON=1`, it requires
an existing compatible daemon.

`session usage` uses a writable local daemon when one is running, or starts a
detached one when fresh local data is needed and no compatible daemon is
running. Without `--no-sync`, `AGENTSVIEW_NO_DAEMON=1` reads local SQLite
directly after acquiring the write-owner lock for any required refresh. Configuring PostgreSQL
does not change this command's default local behavior; pass `--pg` to read
usage from the shared PostgreSQL store. With `--server`, the command calls
`GET /api/v1/sessions/{id}/usage` on the explicit daemon.

Pricing comes from the same `model_pricing` table and
[custom pricing overrides](/docs/token-usage/#custom-model-pricing) that back
`agentsview report usage daily`. This command replaces the older
[`agentsview token-use`](/docs/commands/#agentsview-token-use), which remains
as a deprecated alias.

______________________________________________________________________

## Activity Report

The Activity report endpoint powers the top-level [Activity](/docs/activity/)
page and `agentsview report activity`. It returns one resolved time range with
concurrency buckets, summary totals, breakdowns, and contributing sessions.

```http
GET /api/v1/activity/report
GET /api/v1/activity/report/{report_id}/sessions
```

Session-page responses omit the full report by default so browser sorting,
bucket filtering, and pagination stay page-bounded. Stateless clients can pass
`include_report=true`, and a `refresh_required` response always includes its
complete replacement report.

Activity includes one-shot and automated sessions by default. You can filter
automated sessions with the `automation` query parameter.

The JSON response uses the same `schema_version`, `pricing`, and `projects`
metadata contract as `agentsview report activity --json`.

The report route negotiates its response format from the `Accept` header.
Ordinary clients receive plain JSON, while clients requesting
`text/event-stream` receive throttled `progress` events followed by one
terminal `report` event. Both forms run on the long-running route and are not
subject to the ordinary 30-second operation timeout.

| Query param  | Notes                                                      |
| ------------ | ---------------------------------------------------------- |
| `preset`     | `day`, `week`, `month`, or `custom`                        |
| `date`       | Anchor date for day/week/month presets (`YYYY-MM-DD`)      |
| `from`       | Custom range start, RFC3339                                |
| `to`         | Custom range end, RFC3339                                  |
| `timezone`   | IANA timezone name; default `UTC`                          |
| `bucket`     | Optional bucket override: `5m`, `15m`, `1h`, `1d`, or `1w` |
| `project`    | Filter by project                                          |
| `git_branch` | Filter by opaque branch token from `GET /api/v1/branches`  |
| `agent`      | Filter by agent                                            |
| `machine`    | Exact ID, alias, ID/alias list, or unique whole display label; unknown or ambiguous values return HTTP 400 |
| `automation` | `all`, `interactive`, or `automated`; default `all`        |

Project, branch, agent, and machine filters are capped at 1,024 UTF-8 bytes
each and 3,072 bytes combined. The server also validates the fully encoded
signed report ID before aggregation so JSON escaping cannot overflow the
session-page URL token.

Response excerpt:

```json
{
  "schema_version": 9,
  "report_id": "v1.eyJ2IjoxLC4uLn0.signature",
  "sessions_total": 18,
  "sessions_next_cursor": "v1.eyJvZmZzZXQiOjIwMH0.signature",
  "pricing": {
    "source": "fetched",
    "table_version": "litellm-398a0b15378c",
    "latest_row_updated_at": "2026-06-20T18:40:00Z",
    "custom_override_count": 0,
    "effective_row_count": 2432,
    "digest": "sha256:8d815a1737bce68fa1a19ba977bf33c8c8efcc74deb954fcf62ce80e46e75f2c",
    "cost_source": "mixed",
    "fallback": {
      "used": false,
      "models": []
    },
    "models": {
      "gpt-5.4": {
        "cost_source": "computed",
        "resolutions": [
          {
            "priced_model": "gpt-5.4",
            "matched_pattern": "gpt-5.4",
            "input_cost_per_mtok": {"microdollars": 2500000},
            "output_cost_per_mtok": {"microdollars": 15000000},
            "cache_write_cost_per_mtok": {"microdollars": 0},
            "cache_write_1h_cost_per_mtok": {"microdollars": 0},
            "cache_read_cost_per_mtok": {"microdollars": 250000},
            "cost_source": "computed",
            "bands": [
              {
                "above_input_tokens": 272000,
                "input_cost_per_mtok": {"microdollars": 5000000},
                "output_cost_per_mtok": {"microdollars": 22500000},
                "cache_write_cost_per_mtok": {"microdollars": 0},
                "cache_write_1h_cost_per_mtok": {"microdollars": 0},
                "cache_read_cost_per_mtok": {"microdollars": 500000}
              }
            ],
            "application": {
              "base_request_count": 14,
              "aggregate_row_count": 0,
              "bands": [
                {"above_input_tokens": 272000, "request_count": 2}
              ]
            }
          }
        ]
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
    "cost": {"microdollars": 12340000},
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
      "cost": {"microdollars": 1870000},
      "automated_at_peak": 1,
      "interactive_at_peak": 3
    }
  ],
  "by_project": [{"key": "agentsview", "agent_minutes": 96.4, "cost": {"microdollars": 4200000}}],
  "by_model": [{"key": "claude-sonnet-4-6", "agent_minutes": 80.0, "cost": {"microdollars": 3100000}}],
  "by_agent": [{"key": "codex", "agent_minutes": 64.0, "cost": {"microdollars": 2850000}}],
  "by_session": [
    {
      "session_id": "codex:abc",
      "title": "Update docs",
      "project": "agentsview",
      "agent": "codex",
      "primary_model": "gpt-5.4",
      "models": ["gpt-5.4"],
      "agent_minutes": 24.5,
      "cost": {"microdollars": 1120000},
      "output_tokens": 9200,
      "first_active": "2026-06-20T15:04:00Z",
      "last_active": "2026-06-20T15:38:00Z",
      "timing_quality": "timed",
      "is_automated": false
    }
  ]
}
```

Breakdown rows include total, automated, interactive, and subagent minutes and
costs. Session rows without reliable timestamped activity report
`"timing_quality": "untimed"` and `agent_minutes: null`, though they can still
contribute cost and output tokens when usage rows exist.

Schema v6 returns at most the first 200 session rows in `by_session` and no
longer returns raw activity intervals. To fetch later pages, alternate sorts,
or a bucket drill-down, call the sessions endpoint. It accepts `limit`
(default 200, maximum 500), `cursor`, `sort`, `direction`, and the zero-based
half-open range `bucket_start` and `bucket_end`, with both range bounds
required. If the archive has changed since the signed `report_id` was created,
the response sets `refresh_required` and includes a complete replacement
`report` so clients never mix different report generations.

______________________________________________________________________

## Secret Scanning

Starting in 0.30.0, AgentsView scans session content for credentials during
sync and exposes findings through the CLI, HTTP API, and per-session metadata.
Findings fall into two confidence tiers:

| Tier          | Rules                                                                                                                                                                                                                                                                                                                                                         | When scanned                                        |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------- |
| **Definite**  | Well-anchored vendor formats: AWS access keys, Anthropic `sk-ant-…`, OpenAI `sk-proj-`/`sk-svcacct-`/`sk-admin-`, GitHub `ghp_…` and `github_pat_…`, GitLab `glpat-…`, Slack `xoxb`/`xoxa`/`xoxp`/`xoxr`/`xoxs`, Stripe `sk_live_…` and `rk_live_…`, Google `AIza…`, npm `npm_…`, PyPI `pypi-…`, Hugging Face `hf_…`, SendGrid `SG.…`, PEM private-key blocks | Inline during sync                                  |
| **Candidate** | FP-prone heuristics: basic-auth URLs, JWTs, high-entropy assignments                                                                                                                                                                                                                                                                                          | Only when `agentsview secrets scan` runs explicitly |

AgentsView stores findings in a `secret_findings` table keyed by session ID,
recording the rule name, confidence, location (message, tool_input,
tool_result, or tool_result_event), match coordinates, and a `redacted_match`
value; the raw secret is never stored. Each session also tracks
`secret_leak_count` and `secrets_rules_version` so a future ruleset update can
drive an incremental backfill.

### HTTP API

```
GET /api/v1/secrets
POST /api/v1/secrets/scan
GET /api/v1/search/content    (when called with text that matches a secret rule, snippets are masked)
```

`GET /api/v1/secrets` accepts `project`, `agent`, `date_from`, `date_to`,
`rule`, `confidence` (`definite`, `candidate`, or `all`, default `definite`),
`reveal`, `limit`, and `cursor`. `reveal=true` works only on a localhost-bound
daemon, and remote callers that request reveal receive HTTP 403.

`POST /api/v1/secrets/scan` streams progress with Server-Sent Events and
accepts `backfill`, `project`, `agent`, `date_from`, and `date_to`. It works
only against a writable local daemon.

### Listing sessions with findings

Pass `agentsview session list --has-secret` (or `has_secret=true` on
`GET /api/v1/sessions`) to return only sessions with at least one definite
finding. Because candidate-tier findings do not contribute to
`secret_leak_count`, they do not appear under this filter.

### CLI

The [`agentsview secrets`](/docs/commands/#agentsview-secrets) command group
wraps both scanning and listing:

```bash
# Re-scan the archive with the full ruleset (definite + candidate)
agentsview secrets scan

# List definite findings (redacted; localhost-only --reveal for raw values)
agentsview secrets list
agentsview secrets list --reveal
```

### PostgreSQL parity

When [PostgreSQL sync](/docs/pg-sync/) is enabled, AgentsView mirrors the
`secret_findings` table, the session-level `secret_leak_count`, and the
`--has-secret` filter to the shared database. Substring and regex content
search behave the same way against `pg serve`, with the same masking and
`--reveal` restrictions.

### Ingest-time image offload

The normalized Markdown endpoint `/api/v1/sessions/{id}/md` includes stored
tool-result content, including `agentsview_image` placeholders and `image_ref`
asset references, while HTML export keeps its existing contract. Any serving
host needs the matching `{dataDir}/assets` directory; PostgreSQL and
CockroachDB retain the reference text but cannot resolve local assets. The raw
`agentsview session export` command streams the provider source with detected
secrets masked unless you pass `--reveal`, and keeps the original inline image
payloads.

## Commit lookups

Use `GET /api/v1/sessions/{id}/commits` to fetch `{"commits": [...]}` for one
session, or `POST /api/v1/commits/sessions` with `{"shas": ["abc1234"]}` to get
`{"commits": [{"sha": "abc1234", "links": [...]}]}` in request order. Send 1 to
200 hexadecimal SHAs or prefixes of 7 to 64 characters. Queries are
case-insensitive, unknown SHAs return empty `links`, and invalid SHAs or array
lengths return HTTP 400. PostgreSQL and ClickHouse replicas answer transcript
and trailer link lookups after a push from a version that stores commit links,
while hosted PostgreSQL returns HTTP 501.

Each link includes `session_id`, the recorded `sha`, `branch`, `subject`,
`repository`, `message_ordinal`, `call_index`, `tool_use_id`, `committed_at`,
and `source`. For `source: "transcript"`, `committed_at` is the tool call's
message time. The `sha` matches what Git printed (usually a short prefix), and
full SHAs match stored prefixes. `turn` contains `heuristic: false` along with
the `ordinal` and `text` of the last nonempty, non-system user prompt before
the commit call. Tool-result messages are excluded, text is capped at 2,000
UTF-8 bytes, and `turn` is omitted if no eligible prompt exists.

Links pointing to trashed or permanently deleted sessions carry `missing: true`
and omit `turn`; restoring a trashed session restores its turn. Every candidate
link carries `ambiguous: true` when the matched SHAs cannot all refer to the
same commit. The same SHA recorded by several sessions, or a short SHA paired
with its full SHA, stays unambiguous.

Sessions permanently deleted before this release have no transcript links
because their stored tool calls are already gone.

To build a transcript link, AgentsView requires a `git commit` invocation
(allowing Git global options) and Git's commit summary containing a 7- to
64-character SHA in the call's stored output. Quiet commits and summaries with
shorter SHAs create no link. Older Codex commands stored as argument arrays
support direct Git calls as well as scripts passed to `sh`, `bash`, or `zsh`
with `-c` or `-lc`. Codex output received in a later `write_stdin` poll links
back to the original commit call when its process ID matches; polls without an
originating commit call produce no links.

Code-mode scripts with a single tool call and a fixed command literal can link
commits even when output has no process ID, though command literals containing
`${` produce no links. A single-call `write_stdin` script can also poll an
earlier commit execution without an output process ID. Scripts with multiple
tool calls cannot start new commit links because their output and working
directories cannot be assigned to a single execution. Their polls require an
output process ID matching an earlier commit call. Each session records a SHA
once, at its first matching call. Decorated log lines are excluded. As a known
limitation, a custom log format that prints one bare ref in the same shape as a
commit summary can still create a link when the call also invokes `git commit`.

Repository paths come from the tool's `workdir` or `cwd`, or fall back to the
session's working directory. The last `cd` before the first commit invocation
adjusts that base, and `git -C` overrides it. Relative paths join onto the base
using the transcript's path style, while `~` paths stay literal. Any `cd` after
that invocation is ignored, and a single shell execution committing in two
repositories records the first directory for both commits.

Archives configured with `archive_content = "transcripts"` keep trailer links
but not transcript links; `"usage"` archives keep neither.

### Git trailer links

Run `agentsview git-hook install` in a repository to add trailer lines to new
commits; see [`agentsview git-hook`](/docs/commands/#agentsview-git-hook).
Commits with `Agent-Session: <agent>:<session-id>` lines appear with
`source: "trailer"`, a full SHA, Git's author time, and a repository path.
Claude IDs omit the `claude:` prefix in the archive, while other agents use
their registered prefix. Matching requires the exact archive ID, so sessions
imported from other hosts as `host~<archive-id>` do not receive trailer links
from the original session ID. Unknown agents and invalid trailer values are
ignored. Every matching line in the commit message counts, including lines in
squash commit bodies and merge commits, while inline mentions do not create
links.

`agentsview source sync` scans repositories used by local sessions. Under
`agentsview serve`, editor commits appear at the next scheduled pass, within 15
minutes. Each pass rereads every repository's history, including fetched older
branches, though it reads only checkouts where a local session ran. If a
checkout disappears, another session must still resolve to its repository to
read new commits. Stored rows survive checkout removal and archive rebuilds
under policies that keep trailer links, and rows for commits that are no longer
reachable are kept.

Known limitation: the protected-path guard does not follow a repository's
`config.worktree` or config includes.

Trailer links set `call_index: -1` and an empty `tool_use_id`. Their `branch`
is Git's traversal source with `refs/heads/` or `refs/remotes/` stripped,
excluding remote `HEAD` aliases; other sources such as tags and stash leave it
empty. Their `committed_at` is the Git author time in UTC. Their
`message_ordinal` is the last message ordinal at or before commit time within
the preceding five minutes, or `-1` when none exists, and messages of any role
can satisfy this bound.

The suggested `turn` is the last real user prompt in transcript order with a
usable timestamp at or before commit time and an ordinal at or before
`message_ordinal`. It carries `heuristic: true`, and no recent message or
eligible prompt means no turn. A squash commit made hours after the session
went quiet therefore has no suggested turn. Rebased commits use their preserved
author time. This time rule adapts
[Pando's fallback](https://github.com/backnotprop/pando/blob/80c8f91f3c445b080d49a95d7600ca6a348e9828/worker/src/understand/lookups.ts).
A transcript link for the same session takes precedence when its SHA is a
prefix of the trailer SHA, including a full SHA match. Git prints a short SHA
that uniquely identifies the commit when it is made. Other sessions' trailer
links stay visible.
