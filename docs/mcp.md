---
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

| Tool                   | Purpose                                                            |
| ---------------------- | ------------------------------------------------------------------ |
| `search_sessions`      | Full-text search across recorded sessions                          |
| `list_sessions`        | List recent or filtered sessions                                   |
| `get_session_overview` | Fetch metadata and a compact message preview                       |
| `get_messages`         | Read paginated message bodies from one session                     |
| `search_content`       | Substring, regex, semantic, or hybrid search over raw session text |
| `get_usage_summary`    | Aggregate token and cost usage                                     |

`search_content` accepts a `mode` of `substring` (default), `regex`, `semantic`,
or `hybrid`, plus a `scope` of `top`, `all` (default), or `subordinate` that is
only valid with the semantic and hybrid modes. The `semantic` and `hybrid` modes
need the opt-in [semantic search](/semantic-search/) index on the local SQLite
archive; without it they return a "not available" error. In every mode, each
match carries a conversation-unit citation: an `ordinal_range` of `[start, end]`
ordinals around the match, plus `subordinate`, `relationship`,
`parent_session_id`, and `is_sidechain` fields that flag hits from sidechain
runs and subagent or fork sessions.

## Daemon-Backed Reads

Local MCP mode talks to the AgentsView daemon. Each tool call resolves the local
daemon and starts it when needed, so a long-lived MCP server keeps working even
after the daemon exits due to idleness.

The MCP server does not open the local SQLite archive directly. This keeps MCP
reads on the same daemon policy as the desktop app and avoids a long-running MCP
process holding its own archive handle.

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

See [PostgreSQL Sync](/pg-sync/) for configuring `pg push` and `pg serve`.

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

For every flag, see [`agentsview mcp`](/commands/#agentsview-mcp) in the CLI
reference.
