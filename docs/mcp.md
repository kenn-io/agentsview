---
last_edited: 2026-11-16
title: MCP Server
description: Connect assistant clients to your AgentsView session history with MCP
---

Use `agentsview mcp` to run a read-only
[Model Context Protocol](https://modelcontextprotocol.io) server for your
session archive. From any MCP-capable assistant, you can search past sessions,
preview a session before opening it, read message slices, search raw content,
inspect tool calls and edited files, view activity, tool use, and quality
signals, compare token usage, break down one session's cost, and search the
AgentsView guides without leaving your chat.

MCP tool results warn you when the archive is stale during a full parser
rebuild.

## When to use it

Machine filters accept exact IDs, aliases, comma-separated ID or alias lists, or
a unique full display label. A valid 32-hex installation ID takes precedence
even when it has no stored sessions. If you pass an unknown or ambiguous name,
the server returns an error with valid choices.

Connect the MCP server when you want your coding assistant to answer questions
like these:

- "Have I solved this error before?"
- "Find prior sessions in this repository about the deploy pipeline."
- "Open the relevant messages around this search hit."
- "Which sessions changed `internal/server/routes.go`, and what did they run?"
- "Summarize recent token usage for this project."
- "Why did yesterday's most expensive session cost so much?"
- "How much agent time and cost did this week take, and where did agents
  struggle?"

Every MCP tool is read-only. You can inspect session history and usage data, but
the tools never modify the archive or resync files directly.

When a call includes an unknown argument, its error lists the accepted arguments.

Each tool declares read-only, non-destructive, and idempotent hints to help
clients classify calls, though those hints do not authorize access.
`search_content` and `query_recall` declare open-world hints because their
semantic search modes can send queries to your configured embedding provider,
and `get_session_file_references` does the same because it queries your
configured Docbank service. All other tools advertise a closed environment of
archived data and bundled guides.

### Discover running HTTP listeners

To list HTTP MCP listeners started by this version of AgentsView, run
`agentsview mcp status --json`. It reads local runtime records without starting
a server. Each entry includes `transport`, `url`, `pid`, `backend_url` when
known, and `token_path` when the listener requires a bearer token. The status
output never prints the token itself, so read it from that private file instead.

For wildcard binds, the reported URL uses loopback (`127.0.0.1` for IPv4 or
`::1` for IPv6) so local clients can connect. Other bound addresses appear as
configured.

After startup, a listener publishes its actual bound port (including when you
start it on port zero) and removes its record during orderly shutdown. Status
omits records for listeners it can confirm have exited, while records from
another PID namespace remain listed when AgentsView cannot verify their process
identity. Stdio sessions are not listening endpoints and never appear in this
list. An empty JSON list means AgentsView found no HTTP listeners in its
configured data directory.

## Quick Start

For local desktop MCP clients, configure the server over stdio:

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

Restart or reload your MCP client after adding the entry. Once connected, your
client sees these tools:

| Tool                          | Purpose                                                                                                   |
| ----------------------------- | --------------------------------------------------------------------------------------------------------- |
| `search_sessions`             | Full-text search across recorded sessions                                                                 |
| `list_sessions`               | List recent or filtered sessions                                                                          |
| `get_session_overview`        | Fetch metadata and a compact message preview                                                              |
| `get_compaction_summaries`    | Read compaction summaries for one or more sessions                                                        |
| `get_messages`                | Read paginated message bodies from one session                                                            |
| `get_memory_status`           | Report archive, lexical, semantic, and source readiness                                                   |
| `search_content`              | Substring, regex, terms, semantic, or hybrid search over session text                                     |
| `get_usage_summary`           | Aggregate token and cost usage                                                                            |
| `get_session_usage`           | Show one session's tokens and cost by model and by request                                                |
| `list_recall`                 | List Recall entries, optionally changed or created since a time, when the backend supports Recall queries |
| `query_recall`                | Search extracted Recall entries when the backend supports Recall queries                                  |
| `get_session_file_references` | Find retained image bytes in Docbank, including prior versions and trash                                  |
| `get_tool_calls`              | List tool calls, inputs, result sizes, and optional result text                                            |
| `get_recent_edits`            | Find which sessions edited a file, newest edit first                                                      |
| `get_child_sessions`          | List the sub-agent and fork sessions started from one session                                             |
| `compare_usage`               | Compare token usage and cost between two models or two projects                                           |
| `get_activity_report`         | Report agent time, sessions, and cost for a day, week, month, or range                                    |
| `get_tool_usage`              | Count tool calls by category, agent, and tool name                                                        |
| `get_quality_signals`         | Show health grades, outcomes, failures, and example sessions                                              |
| `get_term_trends`             | Legacy term-frequency counts over time, kept for agent harnesses                                          |
| `search_docs`                 | Search the AgentsView guides bundled with this version                                                    |
| `get_friction_digest`         | Read a dated Friction Log digest as Markdown or the summary object                                        |
| `list_friction_patterns`      | List recurring friction patterns ranked by occurrences                                                    |

Every session tool accepts a plain UUID. Ambiguous IDs list up to five matches.
A trashed exact ID is reported missing. Remote raw ID lookup requires server
API version 29 or later.

`get_compaction_summaries` accepts either a session ID or an `active_since`
timestamp. Its limit counts sessions, and passing 0 uses the default of 20.
When you paginate with `next_cursor`, keep the same `active_since` value even
if a page returns no summaries. Ambiguous older Evener labels appear separately
with their session ID and ordinal.

When your backend stores the Friction Log, the server also exposes
`get_friction_digest` and `list_friction_patterns`. `get_friction_digest`
returns the latest digest by default, or you can pass `date` (YYYY-MM-DD) and
`format` (`markdown` or `summary`). `list_friction_patterns` accepts `kind`,
`since`, and `limit` (default 20, max 100). Each returned pattern includes its
linked [Kata](/docs/kata/) issue as `issue`, or explains why automatic filing
has not linked it in `filing`. Both tools report
[Friction Log](/docs/configuration/#friction-log) heuristics rather than ground
truth.

With `list_sessions`, you can filter by `labels` (every listed label must
match) and `pr` (`owner/repo`, `owner/repo#123`, or
`github.com::owner/repo#123`). Each returned row includes the session's labels,
pull request links, and parent session. For details, see
[labels](/docs/session-api/#agentsview-session-label) and
[pull request links](/docs/session-api/#pull-request-links).
Connection instructions use the `pr` filter before `search_content` finds references in tool results.

With Docbank and every optional tool enabled, the full tool list is about 96 KB.
`TestNewServer_CatalogFitsBudget` fails if it grows past 97 KB.

### Focused memory profile

If a client only needs AgentsView for conversation memory, you can use the
focused profile instead:

```json
{
  "mcpServers": {
    "agentsview": {
      "command": "agentsview",
      "args": ["mcp", "--profile", "memory"]
    }
  }
}
```

This profile exposes only `get_memory_status`, `search_content`,
`get_messages`, and `get_compaction_summaries`. `get_memory_status` reports the
authenticated archive backend, read-only mode, server version, lexical
availability, semantic generation coverage, and whether per-source freshness
telemetry is available. Its `ready`, `partial`, `unavailable`, or `unknown`
states come from the same provider that populates the compact `coverage` object
on every successful `search_content` response, while older remote servers
report `unknown` with an `unsupported` reason. These tools use the same schemas
and backend selection as the full profile over both stdio and StreamableHTTP.
If you omit `--profile` or pass `--profile full`, you get the complete tool
list above.

The memory profile's tool list is about 23 KB, and the same test holds it under
24 KB.

### Choose an installation route

| Route                       | Clients               | What you get                                                                                             | When to use it                                                                                                                                                |
| --------------------------- | --------------------- | -------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Plugin                      | Claude Code           | Recall skill, search agent, memory-profile server, and SessionStart sync hook; updates through `/plugin` | Use the complete memory package in Claude Code.                                                                                                               |
| `agentsview agent setup`    | Claude Code and Codex | Recall skill, Claude search agent, memory-profile server, and SessionStart hook                          | Set up memory for installed clients. Use `--agent claude` or `--agent codex` to select one, and `--check` to inspect setup.                                   |
| Plain MCP entry             | MCP clients           | Full or memory-profile tools only                                                                        | Use the [Quick Start](#quick-start) entry for tools without a recall skill guiding proactive search.                                                          |
| `agentsview skills install` | Claude Code and Codex | Skill and Claude search agent only                                                                       | Install a skill with `--project` or `--server` and register a server named `agentsview` yourself. See the [CLI reference](/docs/commands/#agentsview-skills). |

To install the Claude Code plugin, make sure `agentsview` is on your `PATH`,
then run:

```text
/plugin marketplace add kenn-io/agentsview
/plugin install agentsview-memory@agentsview
```

Restart Claude Code and open `/mcp` to confirm the server is connected. Pick
one installation route per client. The startup diagnostic flags a standalone
skill installed alongside the plugin, and running both the plugin's server and
a standalone `agentsview` entry opens two separate connections. Without
`--force`, `agentsview agent setup` refuses to replace an existing `agentsview`
MCP entry that differs from its own and leaves other entries alone; passing
`--force` also overwrites any skills you edited. To install or check skills
without touching your MCP configuration, run `agent setup` with `--mcp=false`.
`--print` requires MCP setup.

### Memory everywhere, full tools in some projects

Install the plugin at user scope first. Then, in each project where you want
the full tool list, run:

```bash
claude mcp add --scope project agentsview -- agentsview mcp
```

Approve the project's `.mcp.json` (or use `--scope local` instead of
`--scope project` to keep the entry out of the repository). Next, open `/mcp`
in that project and turn off `plugin:agentsview-memory:agentsview`. Because
Claude Code stores this project setting in `~/.claude.json`, repeat that toggle
on each machine you use. The plugin's skill, agent, and hook continue working
while the agent talks to your project server, and all other projects keep using
the plugin's memory server. If you read from a remote archive, add
`--server <url>` and, when needed, `--server-token-file <path>` to the full
server command, or pass `--pg` for PostgreSQL. The `AGENTSVIEW_MEMORY_*`
environment variables only apply to the memory profile and do not affect the
full server.

For a remote archive with the focused profile, set `AGENTSVIEW_MEMORY_SERVER`,
`AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE`, or `AGENTSVIEW_MEMORY_PG`. Explicit
command flags always take precedence. Setting a token file without a server
returns an error instead of falling back to the local archive, and PostgreSQL
reads use your configured `default_pg` target.

To check both this server's status and your installed client integrations, run
`agentsview doctor memory`. Pass `--plugin-root` with the native package root
to inspect its skill, MCP profile, and SessionStart hook. The client check may
start the configured client and MCP process, and Codex may create its own
runtime state, though the check calls no data tools and never starts the local
archive daemon, syncs transcripts, or rebuilds vectors.

Like `list_sessions` and `search_content`, `search_sessions` accepts optional
`date_from` and `date_to` bounds in `YYYY-MM-DD` format. These bounds include
any session whose activity overlaps the requested UTC days. Either bound can be
omitted, and omitting both keeps date matching unrestricted. Malformed dates or
a `date_from` after `date_to` return an error.

When a browser address is available, HTTP-backed session and search results
include a `web_url` field that preserves the server's base path. Use that URL
when citing a session. Direct PostgreSQL reads omit `web_url`.

When you need a specific session, pass its raw UUID or full stored session ID
as `session_id`. This lookup returns one metadata row (including active
sessions), ignores all other search arguments, and errors if the ID is missing
or if a raw suffix matches more than one session. A raw UUID matches an exact
stored ID, an agent ID ending in `:<uuid>`, or a host ID ending in `~<uuid>`.
The delimiter is part of the match, so a fork entry separated by `-` is not
selected. The returned row has an empty `snippet`, `match_ordinal` set to `0`,
and no `next_cursor`; pass that ordinal to `get_messages` to read the first
message. If you already know the full ID and want a compact message preview,
use `get_session_overview` instead. Remote bare UUID lookup requires AgentsView
0.44.0 or later on the server, so use an exact full stored ID with older
servers.

By default, `search_sessions` and `search_content` exclude sessions active in
the last ten minutes, including the current conversation. `search_content`
applies that exclusion before applying the result limit so older matches fill
the page, and reports it in `exclusions.recent_active`. `search_sessions` and
older `search_content` servers apply the exclusion after pagination and report
dropped matches in `excluded_active`, which means their pages can come back
short or empty even with a `next_cursor`. Pass `include_active: true` when you
want recent work included. When `search_sessions` returns an empty page with
excluded active matches, the response includes `hints` with
`retry_with: {"include_active": true}`. If you are recalling from a known
conversation, pass a full stored ID or a raw UUID as `current_session_id` so
`search_content` excludes only that session before the result limit without
hiding other recent work. If the ID is not indexed yet, the search uses it as
given. Ambiguous raw UUIDs return an error.

Set `session_id` to a raw UUID or full stored session ID when you need one
session. The lookup returns one metadata row, includes active sessions, ignores
the other search arguments, and returns an error when the ID is missing or its
raw suffix matches more than one session. A raw UUID matches an exact stored ID,
an agent ID ending in `:<uuid>`, or a host ID ending in `~<uuid>`; the delimiter
is part of the match, so a fork entry separated by `-` is not selected. The row
has an empty `snippet`, `match_ordinal` set to `0`, and no `next_cursor`. Call
`get_messages` with that ordinal to read the first message.
`get_session_overview` provides a compact message preview. Use an exact full
stored ID with an older server.

`list_sessions` and `search_content` also exclude one-shot and automated
sessions by default. Set `include_one_shot: true` or `include_automated: true`
to include them, as an empty result can otherwise hide a matching one-shot or
automated session.

For questions about past work, prefer `search_content` with `mode: "hybrid"`,
especially when you do not know the exact wording. Hybrid combines semantic
similarity with keyword matching, and you can pass `context` to include
surrounding messages. If search by meaning is unavailable, hybrid falls back to
its keyword matches and returns `effective_mode: "hybrid_keyword"` alongside a
`note` next to `coverage` explaining why. On SQLite, PostgreSQL, and
ClickHouse, keyword matches rank messages containing every query word first,
followed by messages with fewer matches. Preserve `scope`, inclusion, and
current-session filters. Disclose keyword-only results once. For exact errors,
identifiers, or code fragments, use fully quoted substring text or a regex
instead. The default mode remains substring.

On SQLite, PostgreSQL, and ClickHouse, a multi-word substring search lists
exact-text matches from every selected source first, then messages containing
every query word, then messages containing fewer words. Each match reports
`matched_words`, which counts the query words it contains excluding unquoted
stop words (other searches report `0`, and hybrid keyword matches report their
word count too). A single word, a fully quoted pattern, or a search that omits
the `messages` source matches exact text only, and a quoted phrase inside a
longer query requires those words together in order.

`search_content` supports five values for `mode`: `substring` (the default),
`regex`, `terms`, `semantic`, and `hybrid`. `terms` splits `pattern` on
whitespace and requires every literal term to appear within one exchange (a
user message and its following assistant run on the same main or sidechain
branch), even if terms appear on opposite sides of the exchange. Assistant
messages before a session's first user message belong to no exchange and never
match. `%`, `_`, and backslashes stay literal, tool and system content are
outside this mode, and `terms` currently requires a SQLite or PostgreSQL
backend.

On a local archive, substring, regex, terms, and FTS searches stop after two
minutes. The keyword part of a hybrid search has the same limit. If a search
times out, retry with `date_from`, `project`, or `session_id`.

You can set `scope` to `top`, `all` (the default), or `subordinate` for
substring, regex, terms, semantic, and hybrid searches. `all` includes child
sessions and sidechain messages, `top` excludes them, and `subordinate` returns
only them. Child sessions bypass the one-shot exclusion, though top-level
one-shot sessions still require `include_one_shot: true`. Semantic mode
requires the opt-in [semantic search](/docs/semantic-search/) index and returns
a "not available" error without it. Exact `session_id`, `git_branch`,
`project`, `agent`, `date_from`, and `date_to` filters apply before the final
limit. Limits default to 10 and go up to 50, with out-of-range values falling
back to the default.

To find one past session that covered several ideas, pass two to five distinct
natural-language strings in `concepts` instead of `pattern`. No matter which
mode you request, AgentsView always uses `semantic` mode. It searches each
concept separately and keeps only sessions with evidence for every concept,
even across different exchanges, ranking results by the mean of each concept's
strongest score. Each match includes `concept_evidence` in request order with
the concept, snippet, score, ordinal, `ordinal_range`, and
`transcript_revision`. Concept searches do not paginate, so passing `cursor` is
rejected. `candidate_truncated` is true when a matching session may be missing,
either because one concept's candidate search filled up or hit its fetch cap,
or because more sessions matched than the limit allowed.

To list a session's messages in order, pass `session_id` without `pattern` or
`concepts`. This call includes active, one-shot, and automated sessions and
ignores `mode`, while any other filters you pass still apply. Results come back
as search snippets, with `...` when shortened. For the full text, use
`get_messages`.

In `terms` mode, each snippet shows about 60 characters of context around the
first occurrence of each term. When terms sit far apart in a long exchange,
separate windows are joined by `...`, so snippet length scales with the number
of terms rather than the length of the exchange.

Every match carries a conversation-unit citation: an `ordinal_range` of
`[start, end]` ordinals around the match, plus `subordinate`, `relationship`,
`parent_session_id`, and `is_sidechain` to flag hits from sidechain runs and
subagent or fork sessions. The response also includes `effective_mode`,
`effective_scope` (for modes that support scope), and the `exclusions` applied
by default. When a first page comes back empty and a diagnostic search finds
hidden matches, the response includes `hints`. Each hint has `code`, `message`,
and `retry_with`, listing the arguments to change while keeping the rest of
your request. Counts report `more than N` when a bounded probe finds more
results, or `at least N` for semantic and hybrid searches or when activity
filtering or candidate limits prevent an exact count. Failed probes add no
hint, and the default scope already includes child sessions.

Pattern searches, `get_messages`, and `get_tool_calls` cap responses at 12,000
bytes, and when a page is cut, `omitted` tells you how many items were held
back. Pageable searches report `total` up to 500, along with `total_capped` when
more matches exist, while message pages report `message_count`. Ranked searches
have no cursor; if results are omitted, reduce `limit`.

When another page is available, the response includes `next_cursor`. To
continue a `search_content` listing, pass `next_cursor` as the numeric `cursor`
and echo the returned RFC3339 `activity_cutoff` unchanged. Echoing the cutoff
keeps the recent-activity exclusion fixed across pages of an unchanged archive,
including on older servers. If that exclusion applies and you send a positive
cursor without the cutoff, the server returns a restart error; restart without
a cursor to get both fields. When you set `include_active: true` or pass an
explicit `current_session_id`, you can continue with the numeric cursor alone.
Archive edits keep the existing offset-pagination behavior.

`exclusions.children` tells you whether child sessions were excluded, even on
empty results. It is true for `scope: top` and for FTS searches without an
exact `session_id`, and stays false for `scope: subordinate` because that scope
keeps child sessions.

On SQLite and PostgreSQL, search matches also include `transcript_revision`,
captured in the same storage query as the evidence, and a `revision_bound`
response flag tells you whether every returned match carries that guarantee.
Pass a match's revision as `expected_revision` to `get_messages`. If the
transcript changed in the meantime, `get_messages` returns `source_changed`, so
repeat the search and use the updated citation.

`get_messages` also returns the revision observed for its page. If a message is
longer than `max_chars_per_message`, the response includes a `body_cursor`;
keep calling `get_messages` with that cursor until the message finishes before
following `next_from`. That opaque cursor is bound to the archive instance,
session, revision, message ordinal, and next content offset, so it cannot
silently continue against replaced transcript content, and it stops working if
the archive server restarts. If a continuation fails with `source_changed` or
`invalid body_cursor`, start a fresh `get_messages` call. Because role and
system filtering still happens after each scanned page, an empty or short page
can still include a `next_from` cursor that you should follow.

### Session details, reports, and guides

`get_tool_calls` lists one session's tool calls in order, returning up to 50
per page (maximum 200). You can narrow the list by setting `category` to
`Edit`, `Write`, `Bash`, or another tool category. The server masks
secret-shaped values in each input and truncates it to `max_input_chars`
(default 500, maximum 10000). To read the surrounding conversation for a call,
pass its `ordinal` to `get_messages`.

If you want saved result text, set `include_results`. The server masks
secret-shaped values in results too, and each call carries two kinds of result
text:

- `result`: the call's current summary, along with `result_length` for its
  stored size in bytes.
- `result_events`: each saved event in order, with its `event_index`,
  `agent_id`, `subagent_session_id`, `status`, `content`, and `content_length`.
  The server never joins events into `result`, and a call omits `result_events`
  when its only event equals `result` and has no agent, subagent, or status.

With `include_results`, each page defaults to one call instead of 50. You can
follow `next_cursor` for the next call, or raise `limit` (maximum 200).
`max_result_chars` (default 1000, maximum 10000) caps `result` and each event's
`content` separately. If a summary or event is truncated, it includes its own
`result_cursor`. To read the next chunk of that one summary or event, pass the
cursor back to `get_tool_calls` with the same session ID. New messages in the
session do not invalidate a cursor, but if a result changes, the cursor becomes
invalid and the call fails with `source_changed`. When that happens, restart
from `get_tool_calls` with `include_results`.

`get_recent_edits` matches `path` as a case-insensitive substring of edited
file paths, or lists every recently edited file if you omit `path`. Files come
back newest edit first at 10 per page (maximum 50), and each file includes up
to 20 of its newest Edit and Write calls along with the session that made each
one.

`get_child_sessions` returns the sub-agent and fork sessions started from
`session_id`, along with each child's `relationship`.

`compare_usage` compares two values across a `dimension` of `model` or
`project`. Pass a single name or comma-separated names for `left` and `right`;
both sides share any optional date range, agent, and machine filters you
provide. The response returns each side's cost, tokens, and session count along
with the differences, and like `get_usage_summary`, it includes one-shot
sessions.

`get_session_usage` reports tokens and cost for one session, matching the
figures from
[`agentsview session usage`](/docs/session-api/#agentsview-session-usage). It
pairs well with `get_activity_report` when `sessions_sort: "cost"` turns up an
expensive session. Pass a full stored ID or raw UUID as `session_id` to get:

- The CLI's JSON fields: `total_output_tokens`, `peak_context_tokens`, `cost`,
  `has_cost`, `cost_source`, `models`, `unpriced_models`, `subagent_count`,
  `plan_usage`, and `capture_coverage`.
- `by_model`: input, output, cache write, and cache read tokens and cost for
  each model, most expensive first.
- `breakdown`: one row per request, 20 at a time (maximum 200). Set
  `breakdown_sort: "cost"` to see the most expensive requests first, and pass
  `breakdown_next_cursor` back as `breakdown_cursor` for the next page. Pass a
  row's `message_ordinal` to `get_messages` as `around` to read that turn.

Totals include any subagent sessions that the session started; pass
`own_only: true` to report only its own transcript, matching `--own-only`. If
any model lacks a known price, `has_cost` is false and `unpriced_models` lists
those models, while `by_model` still shows the cost of every priced model.
Unlike the CLI, this tool does not refresh source files first and reads the
archive as stored, like `agentsview session usage --no-sync`. It omits the
CLI's deprecated `cost_usd`, `server_running`, and `refresh_incomplete` fields.

`get_activity_report` returns the same totals as `agentsview report activity`:
agent minutes, sessions, tokens, and cost, peak concurrent agents, and
breakdowns by project, model, and agent, without the timeline buckets. Set
`preset` to `day` (default), `week`, `month`, or `custom` with RFC3339 `from`
and `to`. `date` picks the day, week, or month and defaults to today in
`timezone` (default UTC). The session list returns 20 rows at a time (maximum
100), ordered longest first or most expensive first when you set
`sessions_sort: "cost"`; pass `sessions_next_cursor` back as `sessions_cursor`
for the next page.

`get_tool_usage` and `get_quality_signals` share the analytics filters: `from`
and `to` default to the 30 days ending today in UTC, and one-shot and automated
sessions are excluded unless you set `include_one_shot` or `include_automated`.
`get_tool_usage` counts tool calls by category, agent, and tool name with a
weekly trend. `get_quality_signals` reports health grades, outcomes, tool
failures and retries, edit churn, context pressure, and prompt quality by agent
and project. Pass a `signal` such as `tool_failure_signals` or `edit_churn` to
also include up to `examples` sessions (default 5, maximum 20) that show it.

`get_term_trends` exposes the data behind the Trends page, which shipped in
0.26.0 (April 2026) and is no longer in the web UI. It remains available for
agent harnesses that still read term counts; to find past work, use the search
tools instead. It accepts the same filters as `get_tool_usage`, plus `terms`,
`granularity`, and `timezone`. You can pass up to 12 `terms` (one series each)
and group variants into one series with `|`, such as
`"load bearing | load-bearing"`, up to 8 variants per term. Matching is
case-insensitive. Single words match on word boundaries (so `cat` does not
match `catalog`) and also count plurals formed by adding `s`, while phrases
match as substrings. The tool counts user and assistant messages and skips
system messages. Buckets are `day`, `week` (default), or `month` in `timezone`
(default UTC), and each bucket includes its message count so you can compute a
rate per 1,000 messages. You can read the same data from
`GET /api/v1/trends/terms`.

`search_docs` searches the guides published at
[agentsview.io/docs](https://agentsview.io/docs/) that are bundled with your
binary, so it works offline and matches the version you run. Every query word
must appear in a section. To search a single guide, set `topic` to its name,
such as `mcp` or `recall`.

### Protocol versions

Over stdio, the server speaks MCP 2026-07-28 and still accepts clients using an
older protocol version. Because StreamableHTTP maintains per-client sessions
that MCP 2026-07-28 does not support, HTTP clients negotiate 2025-11-25 or
older, and current clients fall back to it automatically.

## Daemon-Backed Reads

In local MCP mode, the server talks to the AgentsView daemon. On the first tool
call, it finds the daemon and starts it if needed, then reuses that connection
as long as the daemon's runtime record stays unchanged and the same process is
running. If the daemon exits or another daemon replaces it, the next call finds
the daemon again.

When a tool call fails, the server returns the error to the client without
retrying. The next call finds the daemon again unless the daemon returned a 4xx
error or the client canceled the call, since both show the daemon is still
serving.

If the daemon is read-only, could not write its runtime record, or runs as a
process whose identity cannot be checked, the MCP server looks up the daemon on
every call so a writable daemon can take over as soon as one is available.

If no daemon is visible while another process holds the write lock, the MCP
call returns the lock error instead of starting a daemon that could not open
the archive. The next call checks again.

The MCP server never opens the local SQLite archive directly. That keeps MCP
reads under the same daemon policy as the desktop app and prevents a
long-running MCP process from holding its own archive handle.

Native conversation-memory packages can run `agentsview memory session-start`
on startup, resume, and clear events. It makes sure the writable local daemon
is available, queues a debounced background reconciliation, and returns within
two seconds without waiting for the archive pass. Parallel starts coalesce in
the daemon, while the file watcher keeps ingesting changed transcripts
normally.

Packages configured as hosted contributors run the same command with
`--mode hosted-contributor` and an optional named PostgreSQL target. That wakes
the existing push watcher without starting another writer or copying that
owner's credentials. Hosted read-only packages run `--mode hosted-reader` with
either `--server` or `--pg`. That mode only checks the selected read endpoint
and never starts a local archive or reports that the remote corpus was
refreshed. Contributor wake delivery currently works on macOS and Linux.

Set `AGENTSVIEW_DISABLE_AUTO_SYNC=1` to skip the automatic lifecycle request.
Launcher links, explicit `agentsview source sync` commands, and searches still
work. A failed hook reports the error without blocking your agent session.

If you set `AGENTSVIEW_NO_DAEMON=1` to stop general CLI commands from
auto-starting the daemon, avoid local MCP mode for that archive. Instead, start
the daemon yourself and connect with `--server`, or stop the MCP server.

## Explicit daemon URLs

When the daemon is already running or you want to reach a specific host, pass
`--server`:

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

If that daemon requires bearer auth, set `AGENTSVIEW_SERVER_TOKEN` in your MCP
client's environment or pass `--server-token-file <path>`:

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

AgentsView never sends your local config `auth_token` to an explicit `--server`
URL, preventing an accidental leak of your local daemon token to another host.

## PostgreSQL-backed MCP

If you have `[pg]` or `AGENTSVIEW_PG_URL` configured, pass `--pg` to read
directly from PostgreSQL:

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

Use this when you want the MCP server to read a shared PostgreSQL archive
without depending on a local SQLite daemon.

You can also serve PostgreSQL-backed session history through a read-only
PostgreSQL daemon and point MCP at it:

```bash
agentsview replica pg serve --port 8085
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

For `pg push` and `pg serve` setup, see [PostgreSQL Sync](/docs/pg-sync/).

## StreamableHTTP mode

For local MCP clients, stdio is the default and safest option. Switch to
StreamableHTTP only when a client needs an HTTP MCP endpoint:

```bash
agentsview mcp --http 127.0.0.1:8085
```

Bare ports and `:PORT` values bind to loopback:

```bash
agentsview mcp --http 8085   # same as 127.0.0.1:8085
agentsview mcp --http :8085  # same as 127.0.0.1:8085
```

Binding beyond loopback requires an explicit opt-in:

```bash
agentsview mcp --http 0.0.0.0:8085 --http-allow-insecure
```

Any HTTP listener reachable beyond loopback requires a configured bearer token
and enforces `Authorization: Bearer <token>` on every request. When
`require_auth` is enabled, loopback HTTP binds also require bearer auth so
forwarded ports do not stay unauthenticated by mistake.

## Server identity

The MCP server advertises the AgentsView website and icon, embedding the icon
as a PNG data URI so both stdio and HTTP clients can display it without an
external request. Whether the icon appears depends on your client.

## Security notes

Because the MCP server can expose prompts, assistant responses, tool output,
file paths, project names, and usage totals, treat it like direct access to
your session archive:

- Prefer stdio for local assistant clients.
- Keep HTTP binds on loopback unless the endpoint sits behind a trusted
  network or authenticating proxy.
- Use bearer tokens for any non-loopback or forwarded HTTP endpoint.
- Keep in mind that read-only MCP tools can still expose sensitive data.

For every flag, see [`agentsview mcp`](/docs/commands/#agentsview-mcp) in the
CLI reference.

## Find images in Docbank

`get_session_file_references` takes `session_id` and returns the same result as
`GET /api/v1/sessions/{id}/file-references`. It is available in the full
profile. Each occurrence includes its message ordinal, optional tool and
result-event coordinates, SHA-256 digest, and one of five states: `found`,
`not_found`, `unavailable`, `limit_exceeded`, or `bytes_not_kept`. Images
dropped by the image policy report `bytes_not_kept` with an empty digest.
Matches identify current files, prior versions, or trash, and include an exact
version link when configured. If the integration is disabled, the tool returns
an empty list. For coverage and limits, see
[Docbank configuration](/docs/configuration/#find-transcript-images-in-docbank).
Database reads and Docbank requests each have a separate five-second budget.
Claude pasted images aren't covered yet.

## Review call outcomes

To summarize calls recorded by local MCP servers, run
`agentsview mcp calls --since 7d`. You can pass `--until YYYY-MM-DD` to set the
inclusive end date and `--format json` for JSON output; dates use UTC.

Call records live under `mcp/calls/` in the data directory and do not expire.
Storing them as files lets MCP servers record calls when the archive database
belongs to a separate daemon. Each record includes the tool name, outcome,
argument names, rejected argument names (arguments the tool does not take),
result size, and duration. AgentsView does not record argument values, error
text, or results.

If a call fails, its outcome is `error`. Otherwise, it is `truncated` when a
boolean field whose name contains `truncated` is true, then `empty` when a
search returns no results, and `ok` for everything else. Empty search
percentages include `list_sessions` and exclude continuation pages. If a result
is both truncated and empty, it counts toward both percentages.
