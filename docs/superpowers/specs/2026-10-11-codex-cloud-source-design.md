# Codex cloud threads as an AgentsView source

## Outcome

AgentsView imports every thread that lives on OpenAI's hosted Codex app-server:
work done by ChatGPT dots, Codex web tasks, and Codex cloud tasks. A dot is a
ChatGPT agent that stays signed in to the user's account and runs Codex work on
OpenAI's cloud computer or on a personal computer the user connected. Each
thread becomes a browsable, searchable session with its full trace (messages,
reasoning, shell commands, MCP and other tool calls with output, exit status,
and timing) and the plan usage OpenAI reports for it: the share of the weekly
and five-hour limits it used and the credits it spent, split by model. Sessions
sync in the background like every other agent and ship to the usual replicas
(PostgreSQL, DuckDB, ClickHouse). Sessions and messages need no replica changes;
plan usage needs the replica work described under Plan usage.

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

Second probe, 2026-10-12, with AgentsView's own `User-Agent` and `originator`:

- The server accepted AgentsView's identity. AgentsView never presents itself as
  the desktop app.
- 33 active and 3 archived threads. Listing with every source kind returned no
  extra threads, so the dot's own conversation is not on this server.
- One dot thread had 7 turns and 845 items: 384 reasoning, 357 shell commands,
  82 agent messages, 11 function call outputs, 7 MCP calls, 3 compactions, and
  1 user message.

Usage checks, 2026-10-12, on a personal Pro account whose owner ran them:

- The durable host rejects `account/usage/read` ("unsupported app-server
  method").
- `POST https://chatgpt.com/backend-api/wham/usage/thread_usage/query`, the
  endpoint a local Codex app-server calls for that method, is the only one
  found whose response has per-thread token counts and US dollar estimates. It
  returns HTTP 403 for this account with both AgentsView's identity and the
  desktop app's. The per-turn query (`thread-estimates/query`) also returns
  403\. The desktop app's own code treats 401, 403, 404, and 503 from the
  per-thread query as "no data" and shows nothing, so the 403 is an expected
  answer for some accounts, not a client fault. Unverified: it may be open
  only to accounts billed per token.
- OpenAI's own Codex CLI (0.162.1) answers `account/usage/read` for these
  threads with `threadUsage: null`.
- `POST https://chatgpt.com/backend-api/wham/usage/thread_usage/query_v2`, which
  the desktop app's Usage & billing page uses for its Top chats list, returns
  200 with AgentsView's identity:
    - Request body
      `{"threads": [{thread_id, created_at, descendant_thread_ids}]}`. The app
      sends at most 100 groups and 1,000 thread IDs per request, and puts each
      thread's sub-agent threads in `descendant_thread_ids` so their usage
      counts toward the parent.
    - Response `{data_as_of, comparison_plan, threads}`. Each row has `thread_id`,
      `data_status` (`available`, `partial`, or `unavailable`), `usage_source`
      (for example `included_plan`), `weekly_limit_percent`,
      `five_hour_limit_percent`, `balance_usage_credits` (a decimal string), and
      `groups` of
      `{model, reasoning_effort, speed, product_experience, weekly_limit_percent, five_hour_limit_percent, balance_usage_credits}`.
    - 25 of 34 threads were `available`, 1 `partial`, and 8 `unavailable`,
      including the newest dot thread. Usage appears after a delay.
    - It has no token counts and no dollar amounts.
- `GET /wham/usage/daily-token-usage-breakdown` also reports percentages only
  (`units: percent`), per day, split by thread source, trigger, model, and
  surface.
- `thread/resume` on the durable host sends `thread/tokenUsage/updated` with
  running token totals, but it reattaches the thread's cloud environment. On
  an archived thread it failed with `environment_terminated`. Resume can
  change state on OpenAI's side, so the fetcher never calls it.

## Goals

- Import every thread the account can list on the durable host, including
  archived threads, and keep them current.
- Preserve the full trace with per-item timing.
- Show each thread's plan usage as OpenAI reports it: share of the weekly and
  five-hour limits, and credits, split by model, reasoning effort, and speed.
- Label each session's origin: dot, Codex web, or Codex cloud.
- Link a session to its working folder through `environments[].cwd`.
- Keep a local copy of the fetched records so sessions can be reparsed offline,
  survive deletion on OpenAI's side (resetting a dot deletes its
  conversations), and travel through raw capture like any other source.

## Non-goals

- Starting, steering, or changing anything on OpenAI's side.
- Token counts or dollar costs for these threads. OpenAI does not return them
  for plan-billed accounts (see Background).
- Token estimates computed from the trace text. The trace leaves out system
  prompts, tool definitions, and the history each turn rereads, and these
  models have no public tokenizer, so an estimate would be far off.
- Per-turn usage. The server does not keep it.
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
- A cloud session with no token data looking free in cost views.

## Design

### Components and data flow

```
durable host --(fetcher, poller job)--> mirror files --(provider)--> SQLite --> UI, replicas
  wss                                    {dataDir}/sources/codex-cloud/
query_v2 --(fetcher)--> plan usage records in the same mirror files
```

1. **Fetcher.** A new package containing a minimal app-server websocket client:
   handshake, JSON-RPC requests, paging, plus one HTTPS call for plan usage.
   An allowlist enforced in the client permits only `initialize`, the
   `initialized` notification, `thread/list`, `thread/read`,
   `thread/items/list`, and `thread/turns/list` on the websocket, and only the
   `query_v2` URL over HTTPS. Anything else fails before it is sent.
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
- Never refresh. A missing token or HTTP 401 is a signed-out error that tells
  the user to run `codex login`. HTTP 403 is an access-denied error: the login
  is valid but OpenAI refused this request, so `codex login` is not offered as
  the fix. Both back off through the poller.
- Make fetch results visible, as described under Status and errors.
- Redact the token from every error string before it leaves the fetcher,
  including echoed handshake headers. The subprotocol carries the token, so
  any handshake error formatting must be treated as sensitive.
- Identify the client honestly: send a `User-Agent` naming AgentsView and its
  version, and an `originator` naming AgentsView. The second probe confirmed
  the durable host and `query_v2` both accept this. If a later server release
  refuses it, stop and ask the user; never fall back to the desktop app's
  identity on your own.

### Incremental sync

The server cannot return only the items that changed since a given time. So for
each thread the fetcher keeps a **resume point**: the last item of the longest
run of finished items from the start of the thread. Everything after it, the
**unfinished items** plus anything new, is fetched again on each pass until it
finishes. Every step is bounded by what changed since the last pass, never by
the total size of the archive or of a thread.

An item counts as finished when any of these holds:

- its turn has ended: turn status `completed`, `interrupted`, or `failed`, from
  `thread/turns/list`;
- a later turn in the same thread has started, so a turn that never reports a
  final status cannot pin the resume point forever;
- the thread is archived;
- `completedAtMs` is set and, for items with a `status`, that status is
  terminal.

The resume point stops at the first unfinished item, because Codex runs tool
calls in parallel and a later command can finish before an earlier one; skipping
past an unfinished item would lose its final output.

Per account, the fetcher stores:

- a **listing watermark** for each of the two listings, active and archived: the
  newest `updatedAt` that listing returned in its last complete pass. The two
  are separate, so an update that lands between the two listings is never
  hidden by the other listing's newer timestamps;
- a **retry set**: threads whose last fetch failed, each with an attempt count,
  a next-attempt time, and the time of the first failure;
- for each thread: its last mirrored `updatedAt`, its resume point, the verified
  end offset of its mirror file (see Mirror format), and a **check-again
  flag** (below).

Each pass, for each listing separately:

1. **Changed threads.** Call `thread/list` with `sortKey: updated_at`,
   descending, 50 per page. Collect threads whose `updatedAt` differs from the
   stored value. `updatedAt` has whole-second resolution, so several threads
   can share a timestamp: stop only after the first page that contains a
   thread strictly older than the listing watermark, and finish that whole
   page. With no watermark (first run, or a backfill that never finished),
   page through everything. An unchanged account costs one call per listing.
1. **Unfinished threads.** Also include every thread that the listing shows as
   not idle (`status.type` other than `idle`), or that has unfinished items
   after its resume point. A command can run silently for longer than the poll
   interval, and it is unverified whether finishing an item changes the
   thread's `updatedAt`, so these threads are fetched every pass until they
   finish.
1. **Check-again threads.** Also include every thread whose check-again flag is
   set. A fetch sets the flag. A later fetch clears it only when that fetch
   starts at least one second after the thread's stored `updatedAt`, finds no
   change, and the thread is no longer selected as unfinished. "No change"
   means the listing returned the same `updatedAt` and status, and the item
   fetch returned no record that differs from the newest stored record for
   that item. A second update within the same second as the first is therefore
   always fetched, however long the poll interval.
1. **Retry set.** Add retry-set threads whose next-attempt time has passed.
1. **Apply backoff.** Remove from the selection every thread in the retry set
   whose next-attempt time has not passed, however it was selected. A failing
   thread that stays at the top of the listing still waits out its backoff.
1. **Fetch.** For each selected thread, first call `thread/turns/list` for the
   turn statuses, then `thread/items/list` from the resume point, using an
   item anchor `{type: "item", itemId}` plus its `turnId`. Reading turn
   statuses first means a turn that ends between the two calls is not yet
   treated as ended, so the items it finalizes are fetched again on the next
   pass.
1. **Make it durable, then advance.** Append the fetched records to the mirror,
   fsync the file (and its directory when the file is new), and only then save
   the thread's new `updatedAt`, resume point, and mirror offset, and remove
   it from the retry set. A crash before the save repeats the fetch; it never
   skips records.
1. **Failures.** A thread whose fetch fails joins or stays in the retry set with
   exponential backoff, capped at one hour, and the pass goes on. The latest
   failure category (see Status and errors) is stored with it.
1. **Deleted threads.** If the server reports that a thread no longer exists,
   that outcome is final: remove the thread from the retry set, clear its
   check-again flag and any usage-pending mark, and stop fetching it.
   Everything already stored stays.
1. **Advance the listing watermark** to the newest `updatedAt` that listing
   returned, once the listing finished and every changed thread from it is
   either mirrored or recorded in the persisted retry set. A thread that keeps
   failing therefore never holds the watermark back.

AgentsView does not scan for deleted threads. An unchanged thread is never
fetched again, so a deletion is noticed only when a fetch reports it, and stored
sessions are kept either way.

Known limit: the protocol has no "items updated since" query. The open-source
thread store supports `after_updated_at_ordinal`, but the app-server's
`thread/items/list` handler always passes `None`. An edit to an item after it
was already finished would be missed. No evidence of such edits exists. If a
later protocol exposes update-ordinal paging, switch to it.

### Mirror format

- One file per thread:
  `{dataDir}/sources/codex-cloud/<accountId>/<threadId>.jsonl.zstd`.
- Each fetch appends one complete zstd frame containing JSONL records: thread
  metadata from `thread/list` or `thread/read`, item records exactly as
  returned, and plan usage rows from `query_v2`. Each record carries a small
  envelope naming its kind and fetch time.
- Compression is zstd only, using `github.com/klauspost/compress/zstd` (already
  a direct dependency; the Go standard library has no zstd encoder). No other
  codec, and no option to choose one.
- Every frame carries a zstd content checksum. There is no header frame.
- The fetcher stores the verified end offset of the last complete, checksummed
  frame with the thread's sync state. Before appending, the writer checks only
  the bytes after that offset:
    - complete, checksummed frames there are kept. They come from an append that
      finished before a crash prevented the state save, and the provider may
      already have parsed them. The writer moves the stored offset past them.
      Their records repeat in the next fetch, which is harmless because the
      newest record for an item wins;
    - an incomplete frame at the very end is a torn append. The writer truncates
      it and never builds on it.
- The writer scans the whole file only when the stored offset is missing or lies
  past the end of the file.
- After appending, the writer fsyncs the file, and the directory when the file
  is new, before the new offset is saved.
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
  byte offset inside a frame. Because the writer only truncates an incomplete
  final frame, a frame the provider already parsed is never removed or
  replaced.
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

### Plan usage

OpenAI reports usage for these threads as a share of the plan's limits and as
credits, not as tokens or dollars. AgentsView stores and shows exactly that.

**Usage roots.** The desktop app counts a thread's sub-agent threads toward the
thread that started them. AgentsView does the same. A thread is a **usage root**
unless a known ancestor exists through its `parentThreadId` chain. Each root's
group contains every known descendant at any depth, with duplicate IDs removed.
A thread is sent in exactly one group.

**Fetching.**

- Plan usage is a separate step that runs after the thread step in each pass.
  Its failures never change thread sync status, its backoff, or the listing
  watermarks.
- Each root carries a persisted **usage-pending** mark. The mark is set when the
  root or any of its descendants is fetched, when the root has no usage row,
  when a usage request for it fails, when the response leaves it out, and
  while its row is `partial` or `unavailable`. Only a usable row, or settling
  (below), clears it.
- Each pending root has its own next-attempt time. A failed or unresolved
  attempt doubles the wait, from 5 minutes up to 6 hours.
- **Settling.** A root whose row stays `partial` or `unavailable`, unchanged,
  for 7 days after the thread's last change is settled. Automatic retries
  stop, the last values stay, and the row is marked final. A later change to
  the root or a descendant starts the cycle again.
- Rows older than one day are refreshed for roots that changed in the past week.
- Each pass sends at most 5 requests, oldest pending first. Requests follow the
  desktop app's limits: at most 100 groups and 1,000 thread IDs each. Roots
  left over wait for the next pass, so a large backlog never makes one pass
  unbounded.
- A group sends the root's `thread_id` and `created_at`, and its descendants in
  `descendant_thread_ids`.
- HTTP 401, 403, or 404 from `query_v2` means OpenAI does not offer this usage
  to the account. AgentsView records "Usage isn't available for this account",
  sends no usage requests for that account for 24 hours, and shows no error.
  Other failures use the per-root backoff above.
- A response row is appended to the root's mirror file, with `data_as_of`, only
  when it differs from the last stored row.

**Storage.** Usage rows are running totals for the thread, not increments, and a
percentage of a weekly limit does not add up across weeks. So AgentsView keeps
the latest row per root, replacing the previous one, with its `data_as_of`,
`data_status`, `usage_source`, the thread-level percentages and credits, the
per-group values, and whether the row is final. It does not create token usage
events. Descendants have no row of their own; when a stored thread turns out to
be a descendant, its own row is deleted and it leaves the usage retries. The
builder chooses the table and follows `docs/agents/storage.md`.

**Replicas.** The plan usage table is pushed to every replica backend
(PostgreSQL and CockroachDB, DuckDB, ClickHouse) the same way other per-session
data is, following `docs/agents/storage.md`. The rule that leaves these sessions
out of token and cost totals lives in the shared read layer
(`internal/readbase`) and the SQLite read path, so every backend returns the
same totals. A parity test checks that SQLite and each replica return the same
plan usage and the same totals.

**Display.**

- Session details show "Plan usage": the share of the weekly Codex limit, the
  share of the five-hour Codex limit when present, and credits, split by
  model, reasoning effort, and speed, with "as of T" in local time. A note
  explains that credits are OpenAI's unit for usage beyond what the plan
  includes, so 0 credits means the plan covered the work.
- Session details show one of these when there is no complete row:
    - "Plan usage: not checked yet": no request has finished.
    - "Plan usage: still updating, as of T": a `partial` row that is still
      pending.
    - "Partial plan usage, as of T": a settled `partial` row.
    - "Not reported by OpenAI": a settled `unavailable` row, or none.
    - "Counted under parent thread", with a link to the parent session: a
      descendant.
    - "Usage isn't available for this account": see Fetching.
- These sessions never appear with zero tokens or zero cost on any surface:
    - Session lists show "Plan usage" in the token and cost cells and sort these
      sessions after priced sessions, as missing values.
    - Totals in the UI and in CLI usage reports leave them out and say how many
      sessions were left out and why ("N sessions billed as plan usage, not
      priced").
    - The HTTP API returns `null`, never `0`, for their token and cost fields, and
      a separate plan usage object.
    - Exports write empty cost and token values and include the plan usage fields.

### Configuration

- Settings live in the existing per-agent table, `[agents.codex-cloud]`:
  `enabled` (default `false`), `endpoint` (default
  `wss://codex-cloud-backend.chatgpt.com/`), and `usage_endpoint` (default
  `https://chatgpt.com/backend-api/wham/usage/thread_usage/query_v2`). The
  endpoint overrides exist for tests and future moves.
- The shared `[agents.<id>]` reader (`applyAgentDirectories`) parses only `dirs`
  and `homes` and silently ignores other keys. Add parsing and validation for
  these three keys. An unknown key in `[agents.codex-cloud]` is a config
  error. It appears at startup and in the status area (see Status and errors),
  this source stays off until it is fixed, and every other agent still loads.
- Unlike other agents, which are on unless listed in `disabled_agents`, this
  source is off until the user sets `enabled = true`, because it uses a
  private, undocumented endpoint as the user's ChatGPT account.
- `disabled_agents` wins: if `codex-cloud` is listed there, nothing is fetched
  or parsed, even when `enabled = true`.
- Poller jobs can only be registered at daemon start, so the job is always
  registered and reads the current setting on each run. A config change takes
  effect on the next run without a restart.
- The first version is configured in `config.toml` only. The settings screen
  does not show it yet.

### Status and errors

Poller status lives only in memory, and AgentsView has no general sync-error
display today. This source adds one for itself, shown in the daemon status API,
the UI's sync status area, and `agentsview serve status`. All times are in the
computer's local time.

The source appears only when the user has configured it: `[agents.codex-cloud]`
exists in `config.toml`, or `codex-cloud` is in `disabled_agents`. Users who
never set it up see nothing.

Each Codex home shows one state:

- **Off: listed in `disabled_agents`.** Shown even when `enabled = true`, since
  `disabled_agents` wins.
- **Off: `enabled` is false.**
- **Waiting for first sync:** enabled, no pass has finished.
- **Importing, found N threads so far:** during a first backfill, until both
  listings have been read. After that: **Importing N of M threads.**
- **Up to date as of T:** the last pass succeeded and no thread is waiting to be
  retried.
- **N threads could not be imported:** the last pass succeeded but threads are
  in the retry set. Shows the count, the oldest failure time, and the latest
  failure's message from the table below, followed by "AgentsView keeps
  retrying them. The daemon log lists each thread. Sessions already imported
  are not affected."
- **Error:** the whole pass failed. Shows the message below and when the last
  successful pass finished.

Plan usage has its own line under the home's state, so a usage problem never
hides or replaces thread status. It shows "Plan usage: not checked yet", "Plan
usage as of T", "Usage isn't available for this account", or a usage error from
the table below.

| Cause                                      | Message                                                                                                                                     |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------- |
| No token, or HTTP 401 from the thread host | Signed out of ChatGPT in this Codex home. Run `codex login`, then AgentsView picks up the new login on its next sync.                       |
| HTTP 403 from the thread host              | OpenAI refused this request. AgentsView retries automatically. If this continues, check that this ChatGPT account can use Codex.            |
| Network failure                            | Can't reach OpenAI. AgentsView retries automatically.                                                                                       |
| HTTP 429                                   | OpenAI is limiting requests from this account. AgentsView retries at T.                                                                     |
| HTTP 5xx                                   | OpenAI's service had an error. AgentsView retries automatically.                                                                            |
| Handshake refused or reply not understood  | OpenAI changed its service in a way this version of AgentsView can't read. Update AgentsView. Sessions already imported are safe.           |
| Unknown key in `[agents.codex-cloud]`      | `<config path>` has an unknown setting `<key>` under `[agents.codex-cloud]`. Remove it or correct its name. Cloud import is off until then. |

Raw error details go to the daemon log, with the token redacted, the endpoint
named (thread host or plan usage), and the thread ID for per-thread failures.
The next successful pass clears the error.

### Several machines

The same ChatGPT account may be synced from more than one computer. Session IDs
come from the thread ID alone, so every computer produces the same ID for the
same thread. Pushing the same session from two machines must converge on one
session in each replica, not duplicate it. The builder verifies how each replica
keys sessions and documents the behavior. Users can also enable the source on
only one machine.

## Testing

- A fake durable host on a local websocket server and a fake `query_v2` HTTPS
  server, built from synthetic fixtures. The fixtures contain no private
  names, paths, IDs, or tokens.
- Handshake: the token is sent only as the subprotocol, and handshake errors
  never contain it.
- Allowlist: a non-allowlisted method or URL fails without being sent.
- Incremental behavior: an unchanged account costs one `thread/list` call per
  listing. A thread that grows by k items transfers about k items plus its
  unfinished items, not the whole thread. Add the cardinality-scaling
  regression `docs/agents/background-work.md` requires (small versus large
  archive, unchanged work stays bounded).
- Watermarks: an interrupted backfill resumes and reaches every thread. An
  update to an active thread is found even when the archived listing holds
  newer timestamps, and the reverse. Several threads sharing one `updatedAt`
  second across a page boundary are all fetched.
- Same-second update: a thread changes twice within one second, the first pass
  sees only the first change, and the next pass, five minutes later, fetches
  the second.
- Silent command: a command that produces no output for longer than the poll
  interval, in a thread whose `updatedAt` does not change, still ends with its
  final output and exit status.
- Failures: a thread that keeps failing and stays at the top of the listing is
  not fetched again before its next-attempt time, backs off to the one-hour
  cap, does not hold back the listing watermark, and is fetched once it
  recovers.
- Deletion: a thread the server reports as gone leaves the retry set, is not
  fetched again, keeps its stored session, and does not keep the "could not be
  imported" state up.
- Resume point: parallel tool calls that finish out of order all end with their
  final output. An item without a terminal status in a finished turn, or in a
  turn followed by a later turn, does not pin the resume point. A turn that
  ends between the turn-status call and the item call does not mark its items
  finished.
- Durability: a crash between the mirror append and the state save repeats the
  fetch and leaves no gap and no duplicate session item.
- Mirror: an interrupted append followed by a restart and a successful append
  leaves every complete frame readable; torn frames contribute no records. A
  complete frame written after the saved offset, and already parsed by the
  provider, survives recovery, and the provider keeps parsing from where it
  was. A normal append checks only the bytes after the stored offset; a
  missing or too-large offset triggers a full scan.
- Parsing: an item that finishes after a resume point updates its stored message
  in place without a full reparse or a duplicate.
- Mapping: every item type in the protocol schema, plus an unknown type.
- Plan usage:
    - a newer row replaces the older one;
    - a usage request that fails during the first import is retried after the
      thread's own sync has settled, until a usable row arrives;
    - a `partial` or `unavailable` row backs off and settles after 7 days without
      change, keeping its last values, and a later thread change reopens it;
    - a large backlog of pending roots sends at most 5 requests per pass;
    - nested sub-agent threads (child and grandchild) are all sent with their
      root, never as their own group, and a change in a grandchild refreshes the
      root;
    - a thread that turns out to be a descendant loses its own row and shows
      "Counted under parent thread";
    - a 403 from `query_v2` shows "Usage isn't available for this account", stops
      usage requests for 24 hours, and leaves thread sync status unchanged.
- Cost surfaces: sessions with plan usage are left out of token and cost totals
  in the UI, CLI reports, API, and exports; the API returns `null` for their
  token and cost fields; totals state how many sessions were left out; no
  surface shows them as zero cost.
- Replicas: SQLite, PostgreSQL, DuckDB, and ClickHouse return the same plan
  usage and the same totals for the same data.
- Config: an unknown key in `[agents.codex-cloud]` produces the message above
  with the config path, the source stays off, and local agents still load.
  `disabled_agents` wins over `enabled = true` and shows its own Off state.
- Status: each state appears in the status API and `agentsview serve status`. A
  401 from the thread host shows the `codex login` message and a 403 does not.
  A permanently failing thread shows "N threads could not be imported" with
  the latest failure's message, never "Up to date". Backfill shows "found N so
  far" until both listings are read. A user who never configured the source
  sees no row.

## Documentation

- Add an evidence entry for this format, the handshake, and `query_v2` to
  `docs/internal/session-format-sources.md`, as `AGENTS.md` requires for new
  providers.
- Document the setting, what it does with the user's ChatGPT login, the
  private-endpoint caveat, and why these sessions show plan usage instead of
  tokens and cost, in the user-facing docs.

## Open questions

1. **Live token counts.** While a dot is working, the durable host may send
   `thread/tokenUsage/updated` notifications (input, cached input, output, and
   reasoning token totals) to a client that only listens, without calling
   `thread/resume`. A 10-minute listening test caught nothing because no
   thread was running. A test during an active dot task settles it. If they
   arrive, a later version can record real token counts for work AgentsView
   sees live; finished threads would still have plan usage only.
1. **Descendant threads.** The desktop app finds a thread's sub-agent threads
   with a local discovery step. No listed thread had a `parentThreadId`, so
   whether `parentThreadId` finds them all is unverified. The builder checks
   this against a thread that used sub-agents.

Only the user runs probes, because they use their ChatGPT login; whoever
executes the plan never reads the token. Test fixtures are synthetic: modeled on
the shapes the probes returned, with every value replaced.

## Risks

- The endpoints are private and undocumented. Any desktop app release may change
  the handshake, the protocol, or `query_v2`. Failures must surface as clear
  sync errors and must never damage stored sessions.
- OpenAI may treat third-party clients on these endpoints differently from their
  own apps.
- Re-fetching only unfinished items misses edits to items that already looked
  finished.

## Left to the builder

- Package and type names, file layout, and how the fetcher hands newly mirrored
  threads to the sync engine.
- Exact page sizes within protocol limits.
- The table for plan usage rows and how session details and cost views render
  them, within the rules under Plan usage.
