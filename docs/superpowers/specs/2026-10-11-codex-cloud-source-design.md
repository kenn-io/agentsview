# Codex cloud threads as an AgentsView source

## Outcome

AgentsView imports every thread that lives on OpenAI's hosted Codex app-server:
work done by ChatGPT dots, Codex web tasks, and Codex cloud tasks. Each thread
becomes a browsable, searchable session with its full trace (messages,
reasoning, shell commands, MCP and other tool calls with output, exit status,
and timing) and its token usage and estimated cost. Sessions sync in the
background like every other agent and ship to the usual replicas (PostgreSQL,
DuckDB, ClickHouse) without extra work.

Today none of this is visible. Dot work runs on OpenAI's cloud computer or
drives a connected personal computer, but the thread that records it stays on
OpenAI's servers. On a computer the dot used, only working folders such as
`~/Documents/Codex/<date>/task-N` remain, holding what the work produced, not
what the agent did.

## Background: where the traces live

The ChatGPT desktop app (bundle ID `com.openai.codex`, version 26.1007.30912
when this was written) shows dot work by talking to a remote Codex app-server it
calls the `durable` host, at `wss://codex-cloud-backend.chatgpt.com/`. That
server speaks the same JSON-RPC app-server protocol as a local
`codex app-server`. A read-only probe on 2026-10-11 confirmed it returns the
signed-in account's threads and their complete item history.

Handshake, as the desktop app performs it and as the probe reproduced it:

- websocket subprotocols, in order: `codex-app-server`, `codex-client.desktop`,
  `openai-bearer.<ChatGPT access token>`;
- headers `X-OpenAI-Product-Sku: codex`,
  `X-OpenAI-Allow-Environmentless-Fallback: true`, `originator`, and
  `ChatGPT-Account-Id` when the login has an account ID;
- then `initialize` with `clientInfo`, followed by the `initialized`
  notification.

A plain `Authorization: Bearer` header is rejected with HTTP 403.

What the probe saw:

- `thread/list` returned threads whose `source` is
  `{"custom": "codex-cloud-agent"}`. `originator` distinguishes dot tasks
  (`codex_work_cca`, `threadSource: aeon_child`; "aeon" is the internal
  codename for dots), Codex web (`codex_work_web`), and Codex cloud
  (`codex_cloud`).
- Each thread has `environments[].cwd`, which for dot tasks that used a personal
  computer is that computer's `~/Documents/Codex/<date>/task-N` folder.
- `thread/items/list` returns records shaped
  `{turnId, startedAtMs, completedAtMs, item}`, where `item` is an app-server
  `ThreadItem`. One 8-turn thread had 187 items: `reasoning`,
  `commandExecution` (command, cwd, aggregated output, exit code, duration,
  status), `mcpToolCall` (server, tool, arguments, result, error, duration),
  `agentMessage`, `userMessage`, `contextCompaction`, `functionCallOutput`.
  The protocol also defines `fileChange`, `webSearch`, `dynamicToolCall`,
  `collabAgentToolCall`, `subAgentActivity`, `plan`, `imageView`,
  `imageGeneration`, `hookPrompt`, `sleep`, and review-mode markers.
- Stored history has no per-turn token usage. Per-thread usage comes from
  `account/usage/read {threadId}`: estimated credits and US dollars, and
  input, cached input, net new input, output, and total tokens grouped by
  model, reasoning effort, and speed. Per-turn usage exists only as live
  `thread/tokenUsage/updated` notifications.

## Goals

- Import every thread the account can list on the durable host, including
  archived threads, and keep them current.
- Preserve the full trace with per-item timing.
- Show per-thread token usage and cost, grouped by model.
- Label each session's origin: dot, Codex web, or Codex cloud.
- Link a session to its working folder through `environments[].cwd`.
- Keep a local copy of the fetched records so sessions can be reparsed offline,
  survive deletion on OpenAI's side (resetting a dot deletes its
  conversations), and travel through raw capture like any other source.

## Non-goals

- Starting, steering, or changing anything on OpenAI's side.
- Per-turn token usage or cost. The server does not keep it.
- Live streaming of in-progress turns. Background sync is enough.
- The dot's own chat in ChatGPT, Slack, or Teams, unless it turns out to be
  listable on the durable host (see Open questions).
- Codex OpenTelemetry export. It covers only local processes and leaves out
  content by default.

## Anti-goals

These would make the feature a failure even if it worked:

- AgentsView sending any request that can change state on OpenAI's side.
- The ChatGPT token appearing in logs, errors, files, the archive, or test
  fixtures.
- Sync cost growing with the size of the archive or with the square of a
  thread's length.
- AgentsView refreshing the ChatGPT token itself, which would rotate the refresh
  token the Codex CLI depends on.

## Design

### Components and data flow

```
durable host --(fetcher, poller job)--> mirror files --(provider)--> SQLite --> UI, replicas
  wss                                    {dataDir}/sources/codex-cloud/
```

1. **Fetcher.** A new package containing a minimal app-server websocket client:
   handshake, JSON-RPC requests, paging. An allowlist enforced in the client
   permits only `initialize`, the `initialized` notification, `thread/list`,
   `thread/read`, `thread/items/list`, `thread/turns/list`, and
   `account/usage/read`. Any other outgoing method fails before it is sent.
   Server-initiated requests, such as a token refresh, are answered with a
   JSON-RPC error. Notifications are ignored.
1. **Scheduler.** A `poller.Job`, which `docs/agents/background-work.md`
   requires for interval jobs against external APIs. It inherits jitter,
   cooldown, failure backoff, `Retry-After`, and `TriggerNow` for manual sync.
   Default interval: 5 minutes.
1. **Mirror.** The fetcher writes what it fetched to per-thread files under
   `{dataDir}/sources/codex-cloud/`. AgentsView owns this directory.
1. **Provider.** A new agent, `codex-cloud`, with ID prefix `codex-cloud:` and a
   session ID of `codex-cloud:<threadId>`. It discovers and parses the mirror
   files through the normal sync engine, so fingerprints, resume positions,
   full resync, archive content policy, raw capture, and replica pushes work
   as they do for other file-backed agents.

### Authentication

- Sync every ChatGPT account signed in to a Codex home that AgentsView already
  resolves for the `codex` agent: the default home plus any
  `[agents.codex] homes`, resolved the same way the Codex provider resolves
  them. Do not rely on `CODEX_HOME` from the user's shell, which a background
  daemon usually doesn't inherit.
- Deduplicate homes by `tokens.account_id`, so two homes signed in to the same
  account sync once. Sync state and mirror files are kept per account.
- At the start of each fetch, read `tokens.access_token` and `tokens.account_id`
  from that home's `auth.json`. Hold them in memory for that fetch only. Sync
  errors name the Codex home they affect, never the token.
- Never refresh. On HTTP 401 or 403, or a missing token, record a sync error
  that tells the user to run `codex login`, and back off through the poller.
- Redact the token from every error string before it leaves the fetcher,
  including echoed handshake headers. The subprotocol carries the token, so
  any handshake error formatting must be treated as sensitive.
- Identify the client honestly: send a `User-Agent` naming AgentsView and its
  version, and an `originator` naming AgentsView. Before implementation
  starts, re-run the probe with those values. If the server refuses them, stop
  and ask the user whether to send the desktop app's values instead. Do not
  silently copy the desktop app's identity.

### Incremental sync

Every step is bounded by what changed since the last fetch, never by the total
size of the archive or of a thread.

1. **Changed threads.** Call `thread/list` with `sortKey: updated_at`,
   descending, 50 per page. Stop paging at the first thread whose `updatedAt`
   matches the stored value. Do the same with `archived: true`. An unchanged
   account costs one call per list.
1. **New items.** For each changed thread, call `thread/items/list` with an item
   anchor `{type: "item", itemId}` plus its `turnId`, pointing at the last
   stored item that was finished. An item counts as finished when
   `completedAtMs` is set and, for items with a `status`, the status is
   terminal. Items after the anchor are the open tail plus anything new;
   re-fetching the open tail is how in-progress commands pick up their final
   output.
1. **Usage.** For each changed thread, call `account/usage/read {threadId}` and
   replace that thread's stored totals.
1. **First run.** With no stored state, backfill every thread once from the
   start.

Known limit: the protocol has no "items updated since" query. The open-source
thread store supports `after_updated_at_ordinal`, but the app-server's
`thread/items/list` handler always passes `None`. An edit to an item that
already looked finished would be missed. No evidence of such edits exists. If a
later protocol exposes update-ordinal paging, switch to it.

### Mirror format

- One file per thread:
  `{dataDir}/sources/codex-cloud/<accountId>/<threadId>.jsonl.zstd`.
- Each fetch appends one complete zstd frame containing JSONL records: thread
  metadata from `thread/list` or `thread/read`, item records exactly as
  returned, and the latest usage response. Each record carries a small
  envelope naming its kind and fetch time.
- Compression is zstd only, using `github.com/klauspost/compress/zstd` (already
  a direct dependency; the Go standard library has no zstd encoder). No other
  codec, and no option to choose one.
- Readers process complete frames only, so an interrupted append cannot produce
  a corrupt read. `internal/parser/deepseek_harness_format.go` already reads
  append-only multi-frame `*.jsonl.zstd` files this way; reuse or share that
  logic rather than writing a second reader.
- Re-fetched items are appended again. The parser keeps the last record per
  `(turnId, item.id)`.
- The mirror holds conversation content and is never written outside the data
  directory. It never contains the token.

### Mapping to AgentsView sessions

| Source                                                                                                      | AgentsView                                                                    |
| ----------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| `userMessage`                                                                                               | user message                                                                  |
| `agentMessage`                                                                                              | assistant message                                                             |
| `reasoning`                                                                                                 | thinking block                                                                |
| `commandExecution`                                                                                          | shell tool call with command, cwd, output, exit code, duration                |
| `mcpToolCall`                                                                                               | tool call named by server and tool, with arguments, result or error, duration |
| `fileChange`                                                                                                | edit tool call                                                                |
| `webSearch`, `dynamicToolCall`, `collabAgentToolCall`, `functionCallOutput`, `imageView`, `imageGeneration` | tool call or tool result, by the nearest existing category                    |
| `contextCompaction`                                                                                         | compaction marker                                                             |
| `plan`, `hookPrompt`, `subAgentActivity`, review-mode markers, `sleep`                                      | the closest existing representation; never dropped silently                   |
| unknown item type                                                                                           | kept as a generic tool call or note, with its raw type name                   |

Session fields:

- **Title:** `name`, falling back to `preview`.
- **Times:** `createdAt` and `updatedAt`. Per-item times come from `startedAtMs`
  and `completedAtMs`.
- **Model and reasoning effort:** from the thread.
- **Origin label:** dot for `codex_work_cca` or `threadSource: aeon_child`,
  Codex web for `codex_work_web`, Codex cloud for `codex_cloud`, otherwise the
  raw originator.
- **Working directory and project:** from the first `environments[].cwd`,
  falling back to `cwd`.
- **Parent:** from `forkedFromId` or `parentThreadId` when present.

### Usage and cost

- Store the per-model groups from `account/usage/read` as the session's usage.
  They must show up in AgentsView's existing usage and cost views, split by
  model.
- Cost: use `estimatedUsageUsdMicros` when present and label it as OpenAI's
  estimate. Otherwise price the token counts with the pricing catalog. Models
  missing from the catalog show tokens without cost, as other agents already
  do.
- Never invent a per-turn split. Updating a growing thread must replace its
  totals, never add to them.

### Configuration

- Settings live in the existing per-agent table, `[agents.codex-cloud]`:
  `enabled` (default `false`) and `endpoint` (default
  `wss://codex-cloud-backend.chatgpt.com/`, for tests and future moves).
- Unlike other agents, which are on unless listed in `disabled_agents`, this
  source is off until the user sets `enabled = true`, because it uses a
  private, undocumented endpoint as the user's ChatGPT account.
- `disabled_agents` wins: if `codex-cloud` is listed there, nothing is fetched
  or parsed, whatever `enabled` says.
- Poller jobs can only be registered at daemon start, so the job is always
  registered and reads the current setting on each run. A config change takes
  effect on the next run without a restart.
- The first version is configured in `config.toml` only. The settings screen
  does not show it yet.

### Several machines

The same ChatGPT account may be synced from more than one computer. Session IDs
come from the thread ID alone, so every computer produces the same ID for the
same thread. Pushing the same session from two machines must converge on one
session in each replica, not duplicate it. The builder verifies how each replica
keys sessions and documents the behavior. Users can also enable the source on
only one machine.

## Testing

- A fake durable host on a local websocket server, built from synthetic
  fixtures. The fixtures contain no private names, paths, IDs, or tokens.
- Handshake: the token is sent only as the subprotocol, and handshake errors
  never contain it.
- Allowlist: a non-allowlisted method fails without being sent.
- Incremental behavior: an unchanged account costs one `thread/list` call per
  list. A thread that grows by k items transfers about k items plus its open
  tail, not the whole thread. Add the cardinality-scaling regression
  `docs/agents/background-work.md` requires (small versus large archive,
  unchanged work stays bounded).
- Mirror: interrupted appends, truncated frames, and duplicate item records
  parse correctly.
- Mapping: every item type in the protocol schema, plus an unknown type.
- Usage: totals are replaced, not summed, when a thread grows.

## Documentation

- Add an evidence entry for this format and handshake to
  `docs/internal/session-format-sources.md`, as `AGENTS.md` requires for new
  providers.
- Document the setting, what it does with the user's ChatGPT login, and the
  private-endpoint caveat in the user-facing docs.

## Open questions

Implementation starts only after the user runs the second probe (the read-only
script used for the first one, with AgentsView's own identity) and its results
are recorded here. Only the user runs it, because it uses their ChatGPT login;
whoever executes the plan never reads the token. Test fixtures are synthetic:
modeled on the shapes the probe returned, with every value replaced. The probe
answers these questions, and the design covers either answer.

1. **Archived threads:** whether `thread/list` with `archived: true` returns
   more threads. The design lists both regardless.
1. **The dot's own conversation:** whether listing with every source kind
   returns threads beyond the 31 seen. If so, import them like any other
   thread. If not, they stay a non-goal.
1. **Usage response shape:** confirm the `account/usage/read {threadId}`
   response matches the schema above.

## Risks

- The endpoint is private and undocumented. Any desktop app release may change
  the handshake or the protocol. Failures must surface as clear sync errors
  and must never damage stored sessions.
- OpenAI may treat third-party clients on this endpoint differently from their
  own apps.
- The open-tail strategy misses edits to items that already looked finished.

## Left to the builder

- Package and type names, file layout, and how the fetcher hands newly mirrored
  threads to the sync engine.
- Exact page sizes within protocol limits.
- How the usage groups map onto existing usage storage, within the rules under
  Usage and cost.
