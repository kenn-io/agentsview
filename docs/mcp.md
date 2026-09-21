---
last_edited: 2026-09-21
title: MCP Server
description: Connect assistant clients to your AgentsView session history with MCP
---

The `agentsview mcp` command runs a read-only
[Model Context Protocol](https://modelcontextprotocol.io) server. MCP-capable
assistant clients can use it to search prior sessions, inspect a session before
opening it, fetch message slices, search raw content, and summarize token usage
without leaving the assistant.

## When To Use It

Use the MCP server when you want a coding assistant to answer questions such as:

- "Have I solved this error before?"
- "Find prior sessions in this repository about the deploy pipeline."
- "Open the relevant messages around this search hit."
- "Summarize recent token usage for this project."

The tools are read-only. They expose session history and usage data, but they do
not mutate the archive or resync files directly.

### Discover running HTTP listeners

Run `agentsview mcp status --json` to list HTTP MCP listeners started by this
version. The command reads local runtime records without starting a server. Each
entry includes `transport`, `url`, `pid`, `backend_url` when known, and
`token_path` when the listener requires a bearer token. Read the token from that
private file; the status output does not print it.

For wildcard binds, the URL uses loopback (`127.0.0.1` for IPv4 or `::1` for
IPv6) so local clients can connect. Other bound addresses remain unchanged.

The listener publishes its actual bound port after startup, including when
started with port zero, and removes its record on orderly shutdown. Status omits
records whose process has exited. Stdio sessions are not listening endpoints and
do not appear. An empty JSON list means no HTTP listeners were found in this
application's configured data directory.

## Quick Start

For local desktop-style MCP clients, use stdio:

```json
{
  "mcpServers": {
    "agentsview": {
      "command": "agentsview",
      "args": ["mcp"]
    }
  }
}
```

Restart or reload your MCP client after adding the server. Once connected, the
client will see these tools:

| Tool                   | Purpose                                                                  |
| ---------------------- | ------------------------------------------------------------------------ |
| `search_sessions`      | Full-text search across recorded sessions                                |
| `list_sessions`        | List recent or filtered sessions                                         |
| `get_session_overview` | Fetch metadata and a compact message preview                             |
| `get_messages`         | Read paginated message bodies from one session                           |
| `get_memory_status`    | Report archive, lexical, semantic, and source readiness                  |
| `search_content`       | Substring, regex, terms, semantic, or hybrid search over session text    |
| `get_usage_summary`    | Aggregate token and cost usage                                           |
| `query_recall`         | Search extracted Recall entries when the backend supports Recall queries |

### Focused memory profile

Clients that use AgentsView only for conversation memory can select the focused
profile:

```json
{
  "mcpServers": {
    "agentsview-memory": {
      "command": "agentsview",
      "args": ["mcp", "--profile", "memory"]
    }
  }
}
```

This profile advertises only `get_memory_status`, `search_content`, and
`get_messages`. The status tool reports the authenticated archive backend,
read-only mode, server version, lexical availability, semantic generation
coverage, and whether per-source freshness telemetry is available. Its
`ready`, `partial`, `unavailable`, or `unknown` states come from the same
provider as the compact `coverage` object on every successful
`search_content` response. Older remote servers report `unknown` with an
`unsupported` reason. The tools use the same schemas and backend selection as
the full profile, over either stdio or StreamableHTTP. Omitting `--profile` or
choosing `--profile full` preserves the complete tool list above.

The repository's `plugins/agentsview-memory` package registers this profile for
Claude Code and Codex and bundles the generated recall skill. Its MCP process
reads `AGENTSVIEW_MEMORY_SERVER`, `AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE`, or
`AGENTSVIEW_MEMORY_PG`; explicit command flags still win. A token-file setting
without a server fails instead of falling back to the local archive. PostgreSQL
reads use the configured `default_pg` target.

Run `agentsview doctor memory` to inspect this server status together with the
local client integration. Pass the native package root with `--plugin-root` to
check its skill, MCP profile, and SessionStart hook. The diagnostic uses
read-only metadata and does not start a daemon, sync transcripts, or rebuild
vectors.

`search_sessions` accepts optional `date_from` and `date_to` bounds in
`YYYY-MM-DD` format, just like `list_sessions` and `search_content`. Dates
include sessions whose activity overlaps the requested days in UTC. Either bound
can be omitted; omitting both preserves unrestricted date matching. Malformed
dates and ranges where `date_from` is after `date_to` return an error.

Set `session_id` to a raw UUID or full stored session ID when you need one
session. The lookup returns one metadata row, includes active sessions, ignores
the other search arguments, and returns an error when the ID is missing or its
raw suffix matches more than one session. A raw UUID matches an exact stored ID,
an agent ID ending in `:<uuid>`, or a host ID ending in `~<uuid>`; the delimiter
is part of the match, so a fork entry separated by `-` is not selected. The row
has an empty `snippet`, `match_ordinal` set to `0`, and no `next_cursor`. Call
`get_messages` with that ordinal to read the first message. For a known full ID,
`get_session_overview` remains the way to get a compact message preview.
Remote bare UUID lookup requires an updated server; an exact full stored ID still
uses the existing `Get` path.

`search_sessions` and `search_content` exclude sessions active in the last ten
minutes by default, including the current conversation. Set
`include_active: true` when you need that recent work. For recall from a known
conversation, pass its full ID as `current_session_id`; `search_content` then
excludes only that session before applying the result limit and does not hide
other recent work.

`search_content` also excludes one-shot and automated sessions by default. Set
`include_one_shot: true` or `include_automated: true` to include those classes.
An empty result can therefore omit a matching one-shot or automated session.

When a vector search index is configured, prefer `search_content` with
`mode: "hybrid"` or `mode: "semantic"` for questions about prior work,
especially when the exact wording is unknown. Hybrid combines semantic
similarity with keyword matching. Use `context` to include surrounding messages.
If the index is unavailable, use `search_sessions` for keyword search, or
`search_content` with substring/regex for exact errors, identifiers, and code
fragments. The default search mode remains substring.

`search_content` accepts a `mode` of `substring` (default), `regex`, `terms`,
`semantic`, or `hybrid`. `terms` splits `pattern` on whitespace and requires
every literal term to occur within one exchange: a user message and its ensuing
assistant run on the same main or sidechain branch. Terms can appear on opposite
sides of that exchange. `%`, `_`, and backslashes stay literal; tool and system
content is outside this mode.
The `terms` mode currently requires a SQLite or PostgreSQL backend.

`scope` can be `top`, `all` (default), or `subordinate` for terms, semantic, and
hybrid searches. The semantic and hybrid modes need the opt-in
[semantic search](/docs/semantic-search/) index on the local SQLite archive;
without it they return a "not available" error. Exact `session_id`,
`git_branch`, `project`, `agent`, `date_from`, and `date_to` filters apply before
the final limit. Limits default to 10 and must be between 1 and 50.

Every match carries a conversation-unit citation: an `ordinal_range` of
`[start, end]` ordinals around the match, plus `subordinate`, `relationship`,
`parent_session_id`, and `is_sidechain` fields that flag hits from sidechain
runs and subagent or fork sessions. The response also reports requested and
effective modes, applied filters, default and exact exclusions, and whether the
candidate page was truncated.

SQLite and PostgreSQL search matches also carry `transcript_revision`, captured
by the same storage query as the evidence. A `revision_bound` response flag says
whether every returned match has that guarantee. Pass a match's revision as
`expected_revision` when calling `get_messages`. If the transcript changed in
between, the read returns `source_changed`; repeat the search and use the new
citation.

`get_messages` returns the revision observed for its page. A message longer than
`max_chars_per_message` has a `body_cursor`; keep calling `get_messages` with
that cursor before following `next_from`. The opaque cursor is signed by the
MCP server and stays bound to the archive instance, session, revision, message
ordinal, next content offset, and the role filter of the request that issued
it, so it cannot silently continue against replaced transcript content, be
modified to select another message, or widen what the first read could see. A
continuation ignores its navigation and role arguments and re-applies the
stored role filter. Cursors stop working when the transcript changes or the
MCP server restarts. When the backing daemon predates revision-bound reads
and omits revision or evidence-source metadata, `get_messages` skips cursors
and keeps ordinary `next_from` pagination. Role and
system filtering still happens after each scanned page, so an empty or short
page can have a `next_from` and should be continued.

## Daemon-Backed Reads

Local MCP mode talks to the AgentsView daemon. Each tool call resolves the local
daemon and starts it when needed, so a long-lived MCP server keeps working even
after the daemon exits due to idleness.

The MCP server does not open the local SQLite archive directly. This keeps MCP
reads on the same daemon policy as the desktop app and avoids a long-running MCP
process holding its own archive handle.

Native conversation-memory packages can call
`agentsview memory session-start` on startup, resume, and clear events. The
command ensures the writable local daemon is available, queues a debounced
background reconciliation, and returns within two seconds without waiting for
the archive pass. Parallel starts coalesce in the daemon, while the file watcher
continues to ingest changed transcripts normally.

Packages configured as hosted contributors call the same command with
`--mode hosted-contributor` and an optional named PostgreSQL target. This wakes
the existing push watcher; it does not start another writer or copy that
owner's credentials. Hosted read-only packages use `--mode hosted-reader` with
either `--server` or `--pg`. That mode only checks the selected read endpoint
and never starts a local archive or reports that the remote corpus was
refreshed. Contributor wake delivery is currently available on macOS and Linux.

Set `AGENTSVIEW_DISABLE_AUTO_SYNC=1` to skip this automatic lifecycle request.
Explicit `agentsview sync` commands and existing-history searches remain
available. A disabled or failed lifecycle request does not change archive data;
the package hook is responsible for reporting the failure without blocking the
agent session.

If you need to disable daemon auto-start for general CLI work with
`AGENTSVIEW_NO_DAEMON=1`, do not use local MCP mode for that archive. Start the
daemon yourself and connect with `--server`, or stop the MCP server.

## Explicit Daemon URLs

Use `--server` when the daemon is already running or when you want to target a
specific host:

```json
{
  "mcpServers": {
    "agentsview": {
      "command": "agentsview",
      "args": ["mcp", "--server", "http://127.0.0.1:8080"]
    }
  }
}
```

If that daemon requires bearer auth, set `AGENTSVIEW_SERVER_TOKEN` in the MCP
client environment or pass `--server-token-file <path>`:

```json
{
  "mcpServers": {
    "agentsview": {
      "command": "agentsview",
      "args": [
        "mcp",
        "--server",
        "https://agents.example.com",
        "--server-token-file",
        "/Users/me/.agentsview/token"
      ]
    }
  }
}
```

The local config `auth_token` is not sent to explicit `--server` URLs. This
prevents accidentally leaking a local daemon token to another host.

## PostgreSQL-Backed MCP

If `[pg]` or `AGENTSVIEW_PG_URL` is configured, pass `--pg` to read from
PostgreSQL directly:

```json
{
  "mcpServers": {
    "agentsview": {
      "command": "agentsview",
      "args": ["mcp", "--pg"]
    }
  }
}
```

This is useful when the MCP server should read the shared PostgreSQL archive
without relying on a local SQLite daemon.

You can also expose PostgreSQL-backed session history through a read-only
PostgreSQL daemon and point MCP at it:

```bash
agentsview pg serve --port 8085
```

```json
{
  "mcpServers": {
    "agentsview": {
      "command": "agentsview",
      "args": ["mcp", "--server", "http://127.0.0.1:8085"]
    }
  }
}
```

See [PostgreSQL Sync](/docs/pg-sync/) for configuring `pg push` and `pg serve`.

## StreamableHTTP Mode

stdio is the default and safest choice for local MCP clients. Use StreamableHTTP
only when your client needs an HTTP MCP endpoint:

```bash
agentsview mcp --http 127.0.0.1:8085
```

Bare ports and `:PORT` values bind to loopback:

```bash
agentsview mcp --http 8085   # same as 127.0.0.1:8085
agentsview mcp --http :8085  # same as 127.0.0.1:8085
```

Non-loopback binds require an explicit opt-in:

```bash
agentsview mcp --http 0.0.0.0:8085 --http-allow-insecure
```

When the HTTP listener is reachable beyond loopback, AgentsView requires a
configured bearer token and enforces `Authorization: Bearer <token>` on every
request. If `require_auth` is enabled, loopback HTTP binds also require bearer
auth so forwarded ports are not accidentally unauthenticated.

## Security Notes

The MCP server can reveal prompts, assistant responses, tool output, file paths,
project names, and usage totals. Treat it like access to your session archive.

- Prefer stdio for local assistant clients.
- Prefer loopback HTTP binds unless the endpoint is behind a trusted network or
  authenticating proxy.
- Use bearer tokens for any non-loopback or forwarded HTTP endpoint.
- Remember that MCP tools are read-only, but the data they expose may still be
  sensitive.

For every flag, see [`agentsview mcp`](/docs/commands/#agentsview-mcp) in the
CLI reference.
