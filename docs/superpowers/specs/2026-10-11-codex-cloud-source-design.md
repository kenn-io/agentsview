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
- Stored history has no per-turn token usage. Per-turn usage exists only as live
  `thread/tokenUsage/updated` notifications.
- The durable host rejects `account/usage/read` ("unsupported app-server
  method"). A local Codex app-server answers that method by calling a ChatGPT
  HTTPS endpoint (`codex-rs/backend-client/src/client/thread_usage.rs`):
  `POST https://chatgpt.com/backend-api/wham/usage/thread_usage/query` with
  body `{"thread_ids": [...]}`, 1 to 100 distinct IDs per call, the same
  ChatGPT token as an `Authorization: Bearer` header, and
  `ChatGPT-Account-Id`. Each returned row has `thread_id`,
  `estimated_usage_credits_micros`, `estimated_usage_usd_micros`,
  `native_usage_usd_micros`, and `groups` of
  `{model, reasoning_effort, speed, estimated_usage_credits_micros, native_usage_usd_micros, input_tokens, cached_input_tokens, net_new_input_tokens, output_tokens, total_tokens}`.
  Omitted threads are "unavailable, not zero".

Second probe, 2026-10-12, with AgentsView's own `User-Agent` and `originator`:

- The server accepted AgentsView's identity. AgentsView never presents itself as
  the desktop app.
- 33 active and 3 archived threads. Listing with every source kind returned no
  extra threads, so the dot's own conversation is not on this server.
- One dot thread had 7 turns and 845 items: 384 reasoning, 357 shell commands,
  82 agent messages, 11 function call outputs, 7 MCP calls, 3 compactions, and
  1 user message.

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
- The dot's own chat in ChatGPT, Slack, or Teams. It is not listable on the
  durable host.
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
   handshake, JSON-RPC requests, paging, plus one HTTPS call for usage. An
   allowlist enforced in the client permits only `initialize`, the
   `initialized` notification, `thread/list`, `thread/read`,
   `thread/items/list`, and `thread/turns/list` on the websocket, and only the
   thread-usage query URL over HTTPS. Anything else fails before it is sent.
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
- Never refresh. On HTTP 401 or 403, or a missing token, record a fetch error
  that tells the user to run `codex login`, and back off through the poller.
- Make fetch results visible. Poller status lives only in memory, and AgentsView
  has no general sync-error display today. Report each Codex home's last fetch
  result (last success time, or the error and the action to take) through the
  daemon status API, and show it in the UI's sync status area. The next
  successful fetch clears the error.
- Redact the token from every error string before it leaves the fetcher,
  including echoed handshake headers. The subprotocol carries the token, so
  any handshake error formatting must be treated as sensitive.
- Identify the client honestly: send a `User-Agent` naming AgentsView and its
  version, and an `originator` naming AgentsView. The second probe confirmed
  the server accepts this. If a later server release refuses it, stop and ask
  the user; never fall back to the desktop app's identity on your own.

### Incremental sync

Every step is bounded by what changed since the last fetch, never by the total
size of the archive or of a thread.

Per account, the fetcher stores:

- an **account watermark**: the newest `updatedAt` of a pass in which every
  changed thread was fetched and mirrored successfully;
- a **retry set**: threads whose last fetch failed;
- for each thread, its last mirrored `updatedAt` and its **anchor** (below).

Each pass:

1. **Changed threads.** Call `thread/list` with `sortKey: updated_at`,
   descending, 50 per page. Collect threads whose `updatedAt` differs from the
   stored value. Stop paging at the first thread whose `updatedAt` is older
   than or equal to the account watermark. Do the same with `archived: true`.
   With no watermark (first run, or a backfill that never finished), page
   through everything. An unchanged account costs one call per list.
1. **Add the retry set** to the changed threads.
1. **New items.** For each changed thread, call `thread/items/list` from its
   anchor, using an item anchor `{type: "item", itemId}` plus its `turnId`.
   The anchor is the last item of the longest run of finished items from the
   start of the thread, not simply the last finished item. Codex runs tool
   calls in parallel, so a later command can finish before an earlier one;
   anchoring past an unfinished item would skip its final output. An item
   counts as finished when `completedAtMs` is set and, for items with a
   `status`, the status is terminal. Everything after the anchor is fetched
   again each pass until it finishes.
1. **Usage.** Query the thread-usage endpoint for the changed threads, up to 100
   IDs per call.
1. **Advance state.** A thread's stored `updatedAt` and anchor advance only
   after its records are mirrored. A thread that fails joins the retry set and
   the pass goes on. The account watermark advances to the pass's newest
   `updatedAt` only when every changed thread succeeded, so an interrupted
   backfill or a failed thread is never skipped by a later pass.

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
- Every frame carries a zstd content checksum. There is no header frame.
- Before appending, the writer finds the end of the last complete, checksummed
  frame and truncates anything after it. A torn frame from an interrupted
  append is discarded, never built upon, so later frames stay readable.
- Readers decode only complete frames and never keep records from a torn frame.
- `internal/parser/deepseek_harness_format.go` cannot be reused as is: it
  requires a header frame and, as written, decodes the whole file and keeps
  lines from a torn last frame. Share only its frame inspection and decoder
  memory limits, and apply the rules above.
- Re-fetched items are appended again. The newest record for a
  `(turnId, item.id)` wins.
- The mirror holds conversation content and is never written outside the data
  directory. It never contains the token.

### Incremental parsing

- The provider resumes from the end of the last frame it parsed, never from a
  byte offset inside a frame.
- Each item's `(turnId, item.id)` is its stable identity in the archive. A new
  record for an item already stored updates that item's message or tool call
  in place: final output, exit status, duration, status. It never adds a
  duplicate and never forces a reparse of the whole file.
- A full parse of a mirror file happens only on full resync or a parser version
  change, as for other providers.
- So the local work for a pass is proportional to the records appended in that
  pass, matching the fetch cost.

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

- The usage endpoint returns running totals per thread and model group. Each
  pass compares them with the last totals mirrored for that thread and records
  only the increase, as a usage entry dated at the newest item timestamp seen
  in that pass. Cost therefore lands on the day the work was observed, and
  past days never change as a thread grows.
- The first fetch of a thread has no earlier totals, so its whole total is dated
  at the thread's last activity. The user-facing docs state this limit.
- A decrease in a running total (a server-side correction) is recorded as a
  negative entry dated the same way, so the sum of entries always equals the
  server's latest total.
- Entries show up in AgentsView's existing usage and cost views, split by model.
- Cost: use the endpoint's US dollar estimate when present and label it as
  OpenAI's estimate. Otherwise price the token counts with the pricing
  catalog. Models missing from the catalog show tokens without cost, as other
  agents already do.
- A thread the endpoint omits has unknown usage, not zero. Keep its last entries
  and retry on the next pass.
- Never invent a per-turn split.

### Configuration

- Settings live in the existing per-agent table, `[agents.codex-cloud]`:
  `enabled` (default `false`), `endpoint` (default
  `wss://codex-cloud-backend.chatgpt.com/`), and `usage_endpoint` (default
  `https://chatgpt.com/backend-api/wham/usage/thread_usage/query`). The
  endpoint overrides exist for tests and future moves.
- The shared `[agents.<id>]` reader (`applyAgentDirectories`) parses only `dirs`
  and `homes` and silently ignores other keys. Add parsing and validation for
  these three keys, and reject unknown keys in `[agents.codex-cloud]` with an
  error naming the key.
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
- Watermark: an interrupted backfill resumes and reaches every thread; a thread
  that failed while newer threads succeeded is fetched on the next pass.
- Anchor: parallel tool calls that finish out of order all end with their final
  output.
- Mirror: an interrupted append followed by a restart and a successful append
  leaves every complete frame readable; torn frames contribute no records.
- Parsing: an item that finishes after a resume point updates its stored message
  in place without a full reparse or a duplicate.
- Mapping: every item type in the protocol schema, plus an unknown type.
- Usage: a thread that grows across several days adds only increases, dated per
  pass; past days stay unchanged; the entries sum to the server's latest
  total.
- Config: unknown keys in `[agents.codex-cloud]` are rejected.
- Status: a login failure appears in the daemon status API with the Codex home
  and the `codex login` action, and clears after a successful fetch.

## Documentation

- Add an evidence entry for this format and handshake to
  `docs/internal/session-format-sources.md`, as `AGENTS.md` requires for new
  providers.
- Document the setting, what it does with the user's ChatGPT login, and the
  private-endpoint caveat in the user-facing docs.

## Open questions

The second probe settled archived threads, the dot's own conversation, and the
client identity (see Background). One question remains:

1. **Usage endpoint for cloud threads:** confirm the thread-usage query returns
   rows for durable-host thread IDs with the fields listed under Background.
   If it returns none, cloud sessions ship without usage, and the Usage and
   cost section is dropped from the first version.

Only the user runs the probe, because it uses their ChatGPT login; whoever
executes the plan never reads the token. Test fixtures are synthetic: modeled on
the shapes the probe returned, with every value replaced.

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
