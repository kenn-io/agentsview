# Codex cloud threads as an AgentsView source

## Outcome

AgentsView imports every thread that lives on OpenAI's hosted Codex app-server:
work done by ChatGPT dots, Codex web tasks, and Codex cloud tasks. A dot is a
ChatGPT agent that stays signed in to the user's account and runs Codex work on
OpenAI's cloud computer or on a personal computer the user connected. Each
thread becomes a browsable, searchable session with its full trace (messages,
reasoning, shell commands, MCP and other tool calls with output, exit status,
and timing), real token counts for work AgentsView sees while it runs, and the
plan usage OpenAI reports for it: the share of the weekly and five-hour limits
it used and the credits it spent, split by model. Sessions sync in the
background like every other agent and ship to the usual replicas (PostgreSQL,
ClickHouse) and the DuckDB mirror. Sessions, messages, and live token usage need
no schema changes there; plan usage and live-capture coverage need the new
per-session table described under Plan usage.

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
- `thread/read` and `thread/turns/list` (with `limit`) also answered. Turn
  records carry `id`, `status`, `startedAt`, `completedAt`, `durationMs`,
  `error`, `items`, and `itemsView`. The `itemsView` request parameter was not
  sent in the probes.
- 33 active and 3 archived threads. Listing with every source kind returned no
  extra threads, so the dot's own conversation is not on this server.
- One dot thread had 7 turns and 845 items: 384 reasoning, 357 shell commands,
  82 agent messages, 11 function call outputs, 7 MCP calls, 3 compactions, and
  1 user message.

Usage checks, 2026-10-12, on a personal Pro account whose owner ran them:

- The durable host rejects `account/usage/read` with JSON-RPC error `-32601` and
  the message "unsupported app-server method: account/usage/read".
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

Live token checks, 2026-10-12, run by the account owner:

- Listening without joining a thread receives nothing: a 3-minute listen while
  two dot threads ran received no notifications.
- `thread/resume` with `excludeTurns: true` and no other parameters on a running
  dot thread joined it. The response had `resumeKind: "warm"`. The server then
  sent `thread/tokenUsage/updated` with `total` (running totals since the
  thread started) and `last` (the latest model call), each with `inputTokens`,
  `cachedInputTokens`, `cacheWriteInputTokens`, `outputTokens`,
  `reasoningOutputTokens`, and `totalTokens`, plus `modelContextWindow` and
  `turnId`. The dot sent no requests to AgentsView and kept working.
- `thread/unsubscribe` is unsupported on the durable host (`-32601`,
  "unsupported app-server method: thread/unsubscribe"). Closing the connection
  leaves the thread.
- `thread/resume` on a stopped, unarchived dot thread (all turns completed)
  returned `resumeKind: "cold"`. It started no turn and no new environment
  (the environment ID was unchanged), and the thread's status, `updatedAt`,
  turns, and items were unchanged afterward. It did load the thread into a new
  server-side process, which started every MCP server configured for the dot
  (seven ready, seven failed to start) and sent one warning. It sent no token
  totals, with or without turns requested.
- On an archived thread, `thread/resume` failed with JSON-RPC error `-32600`
  whose `data.errorType` is `environment_terminated`, and changed nothing.
- The listing's `status` can be stale: a thread listed as `idle` had a turn in
  progress and kept adding items.
- Saved items lag the live work: during a running turn, `thread/items/list` did
  not grow for minutes while live item notifications arrived.

## Goals

- Import every thread the account can list on the durable host, including
  archived threads, and keep them current.
- Preserve the full trace with per-item timing.
- Record real token counts for every turn AgentsView sees running, as token
  usage like any other agent.
- Show each thread's plan usage as OpenAI reports it: share of the weekly and
  five-hour limits, and credits, split by model, reasoning effort, and speed.
- Label each session's origin: dot, Codex web, or Codex cloud.
- Link a session to its working folder through `environments[].cwd`.
- Keep a local copy of the fetched records so sessions can be reparsed offline,
  survive deletion on OpenAI's side (resetting a dot deletes its
  conversations), and travel through raw capture like any other source.

## Non-goals

- Starting, steering, or changing anything on OpenAI's side, apart from the join
  described under Live token capture.
- Token counts for work that started and finished before AgentsView saw it
  running. OpenAI does not return them for plan-billed accounts (see
  Background); those turns have plan usage only.
- Token estimates computed from the trace text. The trace leaves out system
  prompts, tool definitions, and the history each turn rereads, and these
  models have no public tokenizer, so an estimate would be far off.
- Per-turn usage. The server does not keep it.
- Showing in-progress turns as they stream. Background sync is enough for the
  trace; the live token monitor records token counts only.
- The dot's own chat in ChatGPT, Slack, or Teams. It is not listable on the
  durable host.
- Codex OpenTelemetry export. It covers only local processes and leaves out
  content by default.

## Anti-goals

These would make the feature a failure even if it worked:

- AgentsView sending any request that can change state on OpenAI's side, with
  one documented exception: live token capture joins running threads with
  `thread/resume` (see Live token capture).
- Restarting a dot's cloud environment, starting a turn, or answering a dot's
  approval prompt or question.
- The ChatGPT token appearing in logs, errors, files, the archive, or test
  fixtures.
- Network cost growing with the size of the archive or with the square of a
  thread's length, or local work per pass growing with the size of the
  archive.
- AgentsView refreshing the ChatGPT token itself, which would rotate the refresh
  token the Codex CLI depends on.
- A cloud session with no token data looking free in cost views.

## Design

### Components and data flow

```
durable host --(fetcher, poller job)--> mirror files --(provider)--> SQLite --> UI, replicas
  wss                                    {dataDir}/sources/codex-cloud/
query_v2 --(fetcher)--> plan usage records in the same mirror files
running threads --(live token monitor)--> token usage records in the same mirror files
```

1. **Fetcher.** A new package containing a minimal app-server websocket client
   (`github.com/coder/websocket`, already in the module graph; the standard
   library has no websocket client): handshake, JSON-RPC requests, paging,
   plus one HTTPS call for plan usage. An allowlist enforced in the client
   permits only `initialize`, the `initialized` notification, `thread/list`,
   `thread/read`, `thread/items/list`, and `thread/turns/list` on the
   websocket, and only the configured `usage_endpoint` URL over HTTPS (or HTTP
   for a loopback override; see Configuration). Anything else fails before it
   is sent. The fetcher joins no thread, so the only server-initiated request
   it expects is a token refresh; it answers that one method with a JSON-RPC
   error and leaves every other server-initiated request unanswered, so it can
   never decline an approval prompt. Notifications are ignored.
1. **Live token monitor.** A second client mode used only for live token
   capture. Its allowlist adds `thread/resume`, accepted only with `threadId`
   and `excludeTurns: true` and no other parameters. It answers only the
   token-refresh request, with an error, and never answers any other
   server-initiated request (see Live token capture).
1. **Scheduler.** `poller.Job` values, which `docs/agents/background-work.md`
   requires for interval jobs against external APIs: one for sync and one for
   live token discovery. Both use the scheduler's jitter and cooldown. Sync
   runs every 5 minutes; live token discovery every 30 seconds. When a joined
   turn completes, the monitor calls the sync job's `TriggerNow` from its own
   goroutine, because the call blocks until the run ends, so re-fetching the
   trace starts without waiting for the next interval. `TriggerNow` reports a
   queued run only as a formatted error today; the builder adds an exported
   `ErrTriggerPending` to `internal/poller`, and the monitor treats it as
   success. Per-account failures are handled inside each job (see Failure
   timing), because the scheduler's own backoff is per job.
1. **Mirror.** The fetcher writes what it fetched to per-thread files under
   `{dataDir}/sources/codex-cloud/`. AgentsView owns this directory. The
   fetcher's own state lives beside the mirror files (see Fetcher state).
1. **Provider.** A new agent, `codex-cloud`, with ID prefix `codex-cloud:` and a
   session ID of `codex-cloud:<threadId>`. It discovers and parses the mirror
   files through the normal sync engine, so fingerprints, full resync, archive
   content policy, and replica pushes work as they do for other file-backed
   agents. See Incremental parsing for how it parses a changed file. Plan
   usage and capture coverage travel in the parse result, so every place that
   stores parsed sessions writes them: the local archive and every
   `rawderive.ProjectionSink`, including the hosted hub's PostgreSQL
   projection (`internal/postgres/raw_projection_rows.go`), which writes
   parsed rows directly rather than through a push. Raw capture is opt-in per
   provider: the provider declares the `RawCapture` capability as supported
   and implements `PlanRawCapture` (`internal/parser/raw_capture.go`) so that
   each thread's mirror file is captured and fetcher state files never are.
   The agent definition must not set `RemoteSyncExcluded`: raw ingest rejects
   every provider with that flag (`validateProvider` in
   `internal/rawsync/manifest.go`), which would stop the mirror from reaching
   a hub. Remote host sync (`internal/remotesync`) instead skips this source
   through a second, narrower definition flag, which the builder adds. Today
   the helper `parser.RemoteSyncExcludedAgent` (`internal/parser/types.go`)
   returns true only for `RemoteSyncExcluded`; the builder changes it to
   return true when either flag is set, and makes every remotesync check use
   it, including the two that read `RemoteSyncExcluded` directly today
   (`internal/remotesync/resolve.go`, target resolution and its forbidden
   roots), so remote host sync fails closed and never ships the mirror directory
   or its state files. Raw ingest (`validateProvider` in
   `internal/rawsync/manifest.go`) keeps reading only `RemoteSyncExcluded`.

### Authentication

- Sync every ChatGPT account signed in to one of these Codex homes: `CODEX_HOME`
  from the daemon's own environment if set, otherwise `~/.codex`, plus every
  `[agents.codex] homes` entry. `dirs` and `CODEX_SESSIONS_DIR` point at
  session folders, not homes, and are ignored. Do not rely on `CODEX_HOME`
  from the user's interactive shell, which a background daemon usually doesn't
  inherit.
- Key each login by its ChatGPT workspace and user. The ID token's
  `https://api.openai.com/auth` claim carries `chatgpt_account_user_id` (the
  workspace membership) and the user ID (`chatgpt_user_id`, else `user_id`);
  the ID token is decoded in memory only. The key is `chatgpt_account_user_id`
  when present, else `tokens.account_id` plus the user ID, else the user ID
  alone when the login has no account ID. `account_id` alone names a
  workspace, so two users of one Business or Enterprise workspace stay
  separate. Homes with the same key sync once, using the first home in the
  order above; if that home's token gets HTTP 401, the next home with the same
  key is tried before the login is reported signed out. Sync state and mirror
  files are kept per key, in a directory named `<accountKey>` that the builder
  derives from the pair; it never contains token material.
- AgentsView reads `auth.json` only when the home's Codex config sets
  `cli_auth_credentials_store` to `file` (the default) or `auto`. In `keyring`
  and `ephemeral` modes Codex ignores `auth.json`, so a leftover token there
  is stale and is never used. A home in `keyring` mode, or in `auto` mode with
  no token in `auth.json`, is keyring-backed. AgentsView does not read the
  keyring; it shows "This Codex home keeps its ChatGPT login in the system
  keyring, which AgentsView does not read." instead of the signed-out message.
  A home in `ephemeral` mode keeps its login only in memory; it shows "This
  Codex home does not save its ChatGPT login, so AgentsView cannot use it."
  instead.
- At the start of each fetch, and before each live-capture connection, read
  `tokens.access_token`, `tokens.account_id`, and `tokens.id_token` from that
  home's `auth.json`; the ID token follows the same in-memory-only rule as the
  access token. Hold the token in memory only until that fetch ends or that
  connection's handshake completes; a joined connection does not keep it. Sync
  errors name the Codex home they affect, never the token.
- Never refresh. A missing token or HTTP 401 is a signed-out error that tells
  the user to run `codex login`. HTTP 403 is an access-denied error: OpenAI
  refused the request, and `codex login` is not offered as the fix because a
  403 does not show that the login is the problem. Both wait as described
  under Failure timing.
- Make fetch results visible, as described under Status and errors.
- Redact the token from every error string before it leaves the fetcher or the
  live token monitor, including echoed handshake headers. The subprotocol
  carries the token, so any handshake error formatting must be treated as
  sensitive.
- Identify the client honestly: send a `User-Agent` naming AgentsView and its
  version, and an `originator` naming AgentsView. The second probe confirmed
  the durable host and `query_v2` both accept this. If a later server release
  refuses it, stop and ask the user; never fall back to the desktop app's
  identity on your own.

### Failure timing

These rules cover the thread host. Plan usage has its own pauses (see Plan
usage), so a `query_v2` failure never pauses thread sync.

The scheduler retries a failed job sooner, not later: it waits `interval/16`
after the first failure and doubles up to the interval
(`internal/poller/poller.go`). Its backoff and `RetryAfterError` also apply to
the whole job, so one account's failure would change the timing for every
account. So both jobs share one persisted next-attempt time per account for the
thread host, kept in `account.json`, and return no error to the scheduler for
one account's failure. A 429 or 403 seen by sync also holds back live discovery,
and the reverse. The waits below are minimums: a job checks them at each
scheduled run, so a wait shorter than the job's interval ends at the next run.

- Signed out (no token, or HTTP 401): kept per Codex home, since a home with no
  token has no account key; this wait is kept in memory and resets on daemon
  restart. Retry when that home's `auth.json` changes, or after one hour.
- Refused (HTTP 403): one hour, doubling to 24 hours.
- Rate-limited (HTTP 429): the `Retry-After` value when present, otherwise 5
  minutes doubling to one hour.
- Network failure or HTTP 5xx: one minute, doubling to one hour.
- A successful request resets that account's wait.

Per-thread failures use the retry set under Incremental sync.

### JSON-RPC errors

The durable host is not open source, so the rules use what the probes saw
(Background) plus the open-source app-server (`codex-rs/app-server`), which uses
`-32600` for a request it cannot read, for an experimental field sent without
opt-in, and for a missing thread. The code alone therefore cannot tell a
protocol change from a missing thread; the message decides. The fetcher and the
live token monitor apply these rules in this order:

- **Thread environment ended** (`-32600` with
  `data.errorType: "environment_terminated"`, seen from `thread/resume` on an
  archived thread): the "error for this thread" category below, checked first
  so its message never matters.
- **Overloaded** (`-32001`) or **shutting down** (`-32600` with
  `data.reason: "serverShuttingDown"`): end the pass or join for that account,
  set the shared account wait as for HTTP 5xx, show the "OpenAI's service had
  an error" message, and add no thread to the retry set.
- **Protocol change:** `-32601` (the durable host answers an unsupported method
  this way), or `-32600` whose message starts with `Invalid request:` or names
  `experimentalApi`. The fetcher fails the account's pass with "Handshake
  refused or reply not understood" and waits as for HTTP 403 (one hour,
  doubling to 24 hours); the monitor stops live capture for the account with
  the same error, as for an unknown `resumeKind`, and does not set the sync
  job's wait; sync finds the change on its own next request.
- **Error on an anchored item read:** any `-32602` in reply to a
  `thread/items/list` call that used an item anchor. In the same pass, the
  fetcher re-reads that turn in full without the anchor; the thread joins the
  retry set only if that read also fails.
- **Any other code** in reply to `initialize` or `thread/list`: the account's
  pass fails with the HTTP 5xx wait and the "OpenAI's service had an error"
  message. In reply to a per-thread request (`thread/read`,
  `thread/turns/list`, `thread/items/list`, or the monitor's `thread/resume`):
  the "error for this thread" category. The fetcher puts the thread in the
  retry set; the monitor closes that connection, logs and counts it, and
  retries on the next run if the thread is still running; discovery, for a
  thread it has not joined, skips that thread and continues the run. This
  covers `environment_terminated` (`-32600` with that `data.errorType`) and a
  missing thread.

### Fetcher state

The fetcher keeps its sync state beside the mirror files, outside SQLite, in two
kinds of file, each written atomically:

- **Per thread:**
  `{dataDir}/sources/codex-cloud/<accountKey>/<threadId>.state.json` holds
  that thread's last mirrored `updatedAt`, resume point, verified mirror
  offset, `parentThreadId`, and for each turn its stored status, when the
  fetcher first saw the turn, and when it first saw the ended status (for the
  10-minute rules), plus the time of the last new item or `updatedAt` change
  (for the 24-hour stall rule). The changed-threads step reads it for each
  thread on the listing pages it walks; otherwise it is read and written only
  when the thread is selected, joined by the monitor, or walked for a usage
  root (below).
- **Per root:**
  `{dataDir}/sources/codex-cloud/<accountKey>/<rootId>.descendants.json` lists
  that usage root's known descendants. When a thread with a `parentThreadId`
  is mirrored, the fetcher follows the parent links through the per-thread
  files to its root and adds it to that root's list. If the parent has not
  been mirrored yet, the child is recorded in a waiting file for that parent,
  `{dataDir}/sources/codex-cloud/<accountKey>/<parentId>.waiting.json`, and is
  treated as a root until then. A waiting file is read only when its parent is
  mirrored, so parents that never appear (a dot's own conversation is not
  listable) cost nothing per pass. When that parent is mirrored, the fetcher
  moves each waiting thread, with its own descendant list, under the parent's
  root, marks that root usage-pending, and writes the "counted under" records.
  Building a usage group reads only the root's list, so the work follows the
  threads that changed, not the size of the archive.
- **Per account:** `{dataDir}/sources/codex-cloud/<accountKey>/account.json`
  holds the listing watermarks, the per-account waits, the plan-usage pauses,
  and the working sets: the retry set, check-again flags, unfinished threads,
  and usage-pending roots with their next-attempt times. These sets hold only
  threads with outstanding work, so the file's size follows recent activity,
  not the size of the archive.

The sync job and the live token monitor both write these files. All state writes
for one account go through one lock in the daemon, so neither overwrites the
other's changes. State files are not `*.jsonl.zstd`, so writing them never
triggers a parse.

Provider discovery reads only `*.jsonl.zstd` files. A full resync, which
rebuilds SQLite from the mirror files, leaves the state intact and fetches
nothing again. If state files are lost, the fetcher treats the affected threads
or account as never synced and fetches them again; mirror appends then repeat
records that the newest-record rule absorbs.

### Incremental sync

The server cannot return only the items that changed since a given time. So for
each thread the fetcher keeps a **resume point**: the last item of the longest
run of finished items from the start of the thread. Everything after it, the
**unfinished items** plus anything new, is fetched again on each pass until it
finishes. Every step is bounded by what changed since the last pass plus the
items after the oldest unfinished item in its turn (an item anchor reads forward
only), never by the total size of the archive. A long-running item early in a
turn, such as a background command, makes each pass re-read the rest of that
turn until it finishes.

An item counts as finished when any of these holds:

- its turn has ended (turn status `completed`, `interrupted`, or `failed`, from
  `thread/turns/list`) and the fetcher first saw that ended status at least 10
  minutes before this fetch started. Saved items lag the live work by minutes,
  so an item saved mid-flight in a just-ended turn stays unfinished, and its
  thread stays in the unfinished set, until that margin passes. The 10-minute
  margin is a guess from the probes; the builder checks it against a real
  turn;
- a later turn in the same thread has started and the fetcher first saw that
  later turn at least 10 minutes before this fetch started, so a turn that
  never reports a final status cannot pin the resume point forever, and the
  save lag is still covered;
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
- for each thread: the per-thread fields listed under Fetcher state, and in the
  account's working sets a **check-again flag** (below).

Each pass, for each listing separately:

1. **Changed threads.** Call `thread/list` with `sortKey: updated_at`,
   descending, one page at a time, with `sourceKinds` set to every source
   kind. When `sourceKinds` is omitted the server lists only interactive
   sources, which can leave out sub-agent threads. Collect threads whose
   `updatedAt` differs from the stored value. `updatedAt` has whole-second
   resolution, so several threads can share a timestamp: stop only after the
   first page that contains a thread strictly older than the listing
   watermark, and finish that whole page. With no watermark (first run, or a
   backfill that never finished), page through everything. An unchanged
   account costs one call per listing.
1. **Unfinished threads.** Also include every thread that the listing shows as
   `active`, whose last fetch saw a turn in `inProgress`, or that has
   unfinished items after its resume point. Other statuses (`idle`,
   `notLoaded`, `systemError`) do not select a thread: a stopped thread can be
   listed as `notLoaded`. The listing's `status` can say `idle` while a turn
   runs, so the stored turn statuses decide. A command can run silently for
   longer than the poll interval, saved items can lag the live work by
   minutes, and it is unverified whether finishing an item changes the
   thread's `updatedAt`, so these threads are fetched every pass until their
   turns finish. A turn that stays `inProgress` with no new item and no
   `updatedAt` change for 24 hours stops this every-pass selection; a later
   `updatedAt` change selects the thread again.
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
   turn statuses, newest first (the default order) with `itemsView: notLoaded`
   (instead of the default `summary` view), stopping at the turn that holds
   the resume point, so the call's cost follows the turns since the resume
   point, not the thread's length, then `thread/items/list` from the resume
   point, using an item anchor `{type: "item", itemId}` plus its `turnId`. An
   item anchor reads only within that turn (the protocol requires `turnId`
   with it), so the fetcher then reads each later turn in full by its
   `turnId`, oldest first, paging with the returned cursor. Reading turn
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
   failure category (see Status and errors) is stored with it. JSON-RPC error
   replies follow the rules under JSON-RPC errors (below Failure timing).
1. **Deleted threads.** No probe has seen a deleted thread, so the exact reply
   is not known (see Open questions). Only that specific not-found reply
   counts as a deletion; any other error goes to the retry set. Until the
   probe identifies the reply, nothing counts as a deletion: a deleted thread
   stays in the retry set, retried at the one-hour cap, and is counted in the
   "could not be imported" status. A deletion is final: remove the thread from
   the retry set, clear its check-again flag and any usage-pending mark, and
   stop fetching it, unless a later listing shows it with a new `updatedAt`,
   which selects it again. Everything already stored stays.
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
  `{dataDir}/sources/codex-cloud/<accountKey>/<threadId>.jsonl.zstd`.
- Each append writes one complete zstd frame containing JSONL records: thread
  metadata from `thread/list` or `thread/read`, turn records from
  `thread/turns/list`, item records exactly as returned, plan usage rows from
  `query_v2`, and token usage and turn-completion records from the live token
  monitor. Each record carries a small envelope naming its kind and fetch
  time.
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

The sync engine's append-only path is gated by the provider capability
`Source.IncrementalAppend`, which only Claude and the Codex JSONL family
declare, and its result type (`IncrementalOutcome` in
`internal/parser/provider.go`) cannot replace a stored item or carry usage
events. Usage events are also written by replacing a session's whole set
(`replaceSessionUsageEventsTx`). So the provider parses a changed mirror file in
full, as most file-based providers do:

- The parse builds each item from its newest record for `(turnId, item.id)`, so
  a re-fetched item replaces its earlier version and never appears twice.
- Only threads whose mirror file changed are parsed. Local work per pass is
  proportional to the size of the threads that changed in that pass, never to
  the size of the archive.
- A thread that changes on every pass while it runs is parsed in full on every
  pass. Live token records are buffered so they add at most one parse per
  minute per joined thread (see Live token capture). That cost grows with the
  thread's length; it is accepted for this version, and a benchmark parses the
  largest synthetic fixture to show the per-pass cost.
- A later version can add the engine work for incremental parsing (item
  replacement and appended usage events in the incremental result) if that
  benchmark shows a need.

### Mapping to AgentsView sessions

| Source                                                                                                      | AgentsView                                                                                               |
| ----------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| `userMessage`                                                                                               | user message                                                                                             |
| `agentMessage`                                                                                              | assistant message                                                                                        |
| `reasoning`                                                                                                 | thinking block                                                                                           |
| `commandExecution`                                                                                          | shell tool call with command and cwd; output, exit code, and duration in the tool result text            |
| `mcpToolCall`                                                                                               | tool call named by server and tool, with arguments; result or error and duration in the tool result text |
| `fileChange`                                                                                                | edit tool call                                                                                           |
| `webSearch`, `dynamicToolCall`, `collabAgentToolCall`, `functionCallOutput`, `imageView`, `imageGeneration` | tool call or tool result, by the nearest existing category                                               |
| `contextCompaction`                                                                                         | compaction marker                                                                                        |
| `plan`, `hookPrompt`, `subAgentActivity`, review-mode markers, `sleep`                                      | the closest existing representation; never dropped silently                                              |
| unknown item type                                                                                           | kept as a generic tool call or note, with its raw type name                                              |

Session fields:

- **Title:** `name`, falling back to `preview`.
- **Times:** `createdAt` and `updatedAt`. Per-item times come from `startedAtMs`
  and `completedAtMs`.
- **Model and reasoning effort:** sessions have no model field, so the thread's
  model and reasoning effort go on each assistant and thinking message, as
  other providers record them.
- **Origin label:** dot for `codex_work_cca` or `threadSource: aeon_child`,
  Codex web for `codex_work_web`, Codex cloud for `codex_cloud`, otherwise the
  raw originator. It goes in the existing `Entrypoint` field; no new column.
  `SessionKind` stays empty, as for interactive sessions, because its other
  values drive automated-session classification (`internal/db/automated.go`).
  My call: dot tasks show as normal sessions, not as automated workers.
- **Working directory and project:** from the first `environments[].cwd`,
  falling back to `cwd`. `environments` is marked experimental in the
  open-source protocol, and the fetcher does not opt in to the experimental
  API; the probe saw the field with AgentsView's handshake, and the builder
  confirms it still appears.
- **Relationships:** `forkedFromId` becomes a fork relationship (`RelFork`) and
  `parentThreadId` a sub-agent relationship (`RelSubagent`), each pointing at
  `codex-cloud:<threadId>`.

### Live token capture

OpenAI shows a thread's token counts only to a client that has joined the thread
while it runs (see Background). The live token monitor joins running threads,
records every token update, and leaves when the turn ends.

**Finding running threads.**

- A separate `poller.Job` runs every 30 seconds with jitter. Each run re-reads
  the live-capture gates (the list below); when any gate is closed it tells
  the monitor to flush and close every joined connection and finds nothing
  new. It follows `docs/agents/background-work.md`. The job only finds running
  threads and hands them to the monitor; it returns without waiting for joined
  threads.
- The monitor owns the joined connections: at most 10 per account, each with its
  own goroutine, all flushed and closed on daemon shutdown or when live
  capture turns off. Live capture is off when any gate is closed: `enabled` is
  false, `live_tokens` is false, `codex-cloud` is in `disabled_agents`, source
  sync is off (below), or `[agents.codex-cloud]` has a config error. Its
  memory is bounded by that cap, not by archive size.
- The listing's `status` can be stale, so it only narrows the search. Each run
  lists the non-archived listing by `updated_at`, newest first,, with
  `sourceKinds` set to every source kind as sync does, and checks the newest
  turn (`thread/turns/list`, limit 1, `itemsView: notLoaded`) of every thread
  that is listed as `active` or whose `updatedAt` is within the past 30
  minutes, skipping threads the monitor has already joined. A thread is
  running when that turn's status is `inProgress`.
- Work per run is bounded by recent activity, not by archive size. Discovery
  reads further pages while the last page still holds a thread listed as
  `active` or updated in the past 30 minutes. It also checks every thread that
  the sync job's unfinished set records with an `inProgress` turn, except
  threads stopped by the 24-hour stall rule, one `thread/turns/list` call
  each, so a running thread on a later page whose listing says `idle` is still
  found. A thread that starts a turn without updating `updatedAt` or its
  status for more than 30 minutes is missed; that is a documented limit.

**Joining.**

- Each joined thread gets its own websocket connection, because the durable host
  does not support `thread/unsubscribe` and closing the connection is the only
  way to leave. At most 10 threads per account are joined at once; others wait
  for a free slot.
- The monitor does not join a sub-agent thread until the user has run a probe
  confirming that joining a running sub-agent thread is a warm join with no
  side effects (see Open questions). A thread counts as a sub-agent thread
  when it has a `parentThreadId`, when its `threadSource` is `subagent`, or
  when its `source` is `SessionSource::SubAgent` (`v2/thread_data.rs` in the
  Codex protocol). Such threads are found but left unjoined; their tokens are
  not captured until then.
- Immediately before joining, the monitor checks the newest turn again and joins
  only if it is still `inProgress`. It then sends `thread/resume` with only
  `threadId` and `excludeTurns: true`.
- A JSON-RPC error reply to `thread/resume` (for example
  `environment_terminated`) follows JSON-RPC errors: most close the connection
  and are retried on the next run if the thread is still running.
- For a successful response, its `resumeKind`, a field only the hosted server
  returns, decides what happens; a missing `resumeKind` counts as "anything
  else":
    - `warm`: the thread is running and the monitor stays.
    - `cold`: the turn ended between the check and the join. The monitor logs the
      race, counts it in status, and closes the connection at once.
    - anything else: the monitor closes the connection, stops live capture for the
      account, and reports a protocol-change error. Capture stays off for that
      account until the daemon restarts or the user changes `live_tokens`.

**While joined.**

- The monitor reads only `thread/tokenUsage/updated`, `turn/completed`, and
  `model/rerouted` for that thread and ignores every other notification.
  `model/rerouted` is a stable notification that names the model a turn moved
  to; token records after it carry that model. The monitor does not opt in to
  the experimental API, so `thread/settings/updated` never arrives; a model
  change that only that notification reports is not tracked while joined.
- It answers only the token-refresh request
  (`account/chatgptAuthTokens/refresh`) with a JSON-RPC error, as the fetcher
  does, because a joined connection can stay open for hours. It never answers
  any other server-initiated request: approval prompts and questions stay for
  the user's own apps to answer.
- Messages on one websocket arrive in order, and the server sends a turn's token
  updates during the turn, before `turn/completed`. So when `turn/completed`
  arrives, every update for that turn has already arrived. The monitor then
  writes a turn-completion record, flushes its buffer, and leaves. It also
  leaves after 6 hours, or when live capture turns off.
- If the connection drops, the next run joins the thread again if it is still
  running.

**What gets stored.**

- When the monitor joins a thread that has no mirror file yet (live discovery
  can find a thread before sync mirrors it), it first writes the `thread` from
  the `thread/resume` response as a metadata record, so the file always starts
  with thread metadata. The provider skips a mirror file with no metadata
  record and logs it.
- Each token update becomes a token usage record with the time it was observed,
  `turnId`, `total`, `last`, `modelContextWindow`, and the current model: the
  model from the `thread/resume` response, updated by `model/rerouted`. The
  monitor buffers records in memory and appends them to the thread's mirror
  file as one frame at most once a minute, when the turn completes, and when
  it leaves the thread. A crash loses at most the buffered records; because
  totals are cumulative, the next update recovers the counts. Writes from the
  monitor and the fetcher to one mirror file are serialized.
- The provider turns these records into usage events (`ParsedUsageEvent`, stored
  in `usage_events`, declared through the `AggregateUsageEvents` capability),
  the same kind other agents produce, so they reach replicas, the DuckDB
  mirror, and cost views without new tables.
- Each event leaves `MessageOrdinal` nil. An event with an ordinal is priced as
  one request (`FromEvent` in `internal/usagefacts/fact.go`), which would pick
  a price band from the thread-total increase.
- Each event has `Source: "session"`, the value other providers use for
  thread-level totals. The engine skips `session` events when it derives a
  session's own token fields (`internal/ingest/prepare.go`), so, as Cline
  does, the provider sets `TotalOutputTokens` to the sum of output increases
  across baselines and `PeakContextTokens` from the largest `last` input
  count, with their `Has*` flags. Events with other sources are clamped at
  2,000,000 tokens per field (`clampUsageEventTokens` in
  `internal/db/validate.go`, `MaxPlausibleTokens` in `internal/usagefacts`),
  and a thread's running totals easily pass that; `session` events are only
  floored at zero.
- Each event has a non-empty model: the record's current model (from
  `ThreadResumeResponse.model`, updated by `model/rerouted`), else the
  thread's model from the listing, else `unknown`. Two model changes are not
  seen: a change that only `thread/settings/updated` reports, whose increases
  go to the earlier model until the next join, and a per-turn reroute that
  happened before the join, whose tokens go to the thread's configured model.
  The user-facing docs state both limits. The usage cache drops events with an
  empty model.
- Each event is the increase in `total`, field by field, since the previous
  record for that thread, dated when it was observed. A record with no
  increase produces no event. If a total ever decreases, the provider starts a
  new baseline, logs it, and records no negative event; the first record of
  the new baseline adds its full total, because a decrease means the server
  started a fresh counter. The `DedupKey` combines the thread ID, the baseline
  number, and the observed total, so a full reparse produces the same events
  and a total that recurs after a new baseline does not collide.
- Token fields map the way the local Codex provider maps them
  (`applyCodexTokenUsage` in `internal/parser/codex_collecting_sink.go`):
  OpenAI's `inputTokens` already includes cached reads and cache writes, so
  input is `inputTokens - cachedInputTokens - cacheWriteInputTokens`,
  `cachedInputTokens` becomes cache-read tokens, `cacheWriteInputTokens`
  becomes cache-creation tokens, and `outputTokens` becomes output, with the
  same clamps (uncached input floored at 0, cache writes capped at uncached
  input). `ReasoningTokens` stays 0, as in the local Codex provider:
  `reasoningOutputTokens` is already inside `outputTokens`, and the cost
  formula treats reasoning as a breakdown of output (`CostForTokensScoped` in
  `internal/export/pricing.go`), so this choice does not change cost.
- The first record for a thread has no earlier total, so its whole total is
  dated when it was observed, even when earlier turns ran on an earlier day.
  The user-facing docs state this limit.
- Because totals cover the thread from its start, one capture covers every turn
  that finished before it was observed.

**How it shows up.**

- Sessions with token usage count in token and cost views like any other agent.
  Cost comes from AgentsView's price list; models missing from it show tokens
  without cost.
- The provider computes each session's capture coverage from the mirror's turn
  and token usage records: the time of the last capture and the number of
  finished turns not captured. A finished turn is captured when a joined
  connection saw its `turn/completed` (its turn-completion record is in the
  mirror), or when a token record was observed after the turn completed.
  Otherwise, for example after the 6-hour leave or a dropped connection with
  no later capture, it is not captured. Turns before the first join are
  captured by the first join's total. It stores both in the new per-session
  table (see Plan usage), so every backend can query them.
- Session details say "Tokens captured live, as of T". When finished turns are
  not captured, they add "N turns not captured; plan usage covers them."
- Totals that include such sessions say so: "N sessions partly captured; their
  token counts are low." A session is partly captured when it has at least one
  usage event and at least one turn not captured. Plan-only sessions, which
  have no usage events, appear only in the "sessions left out" count. The
  partly-captured count follows the same rules as that count (see Replicas and
  mirror): computed at query time, per backend, from the per-session table,
  with the same filters as the totals.
- Sessions with no token usage follow the Plan usage display rules.
- Plan usage is still shown in session details for every session.

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
- **Settling.** A root whose row stays `partial` or `unavailable`, or that every
  response leaves out, unchanged, for 7 days after the thread's last change is
  settled. A root that is always left out settles as "Not reported by OpenAI".
  Automatic retries stop, the last values stay, and the row is marked final. A
  later change to the root or a descendant starts the cycle again.
- Roots with a usable row stay in a refresh set in `account.json` until 7 days
  after their last change, each with a next attempt one day after its row's
  `data_as_of`. Rows are refreshed from that set, so no pass scans every root.
- Each pass sends at most 5 requests, shared by pending roots and the refresh
  set: pending roots first, oldest first, then refresh roots by next attempt.
  Requests follow the desktop app's limits: at most 100 groups and 1,000
  thread IDs each. Roots left over wait for the next pass, so a large backlog
  never makes one pass unbounded.
- A group sends the root's `thread_id` and `created_at`, and its descendants in
  `descendant_thread_ids`.
- HTTP 429 from `query_v2` pauses plan usage for that account for the
  `Retry-After` value, or 5 minutes doubling to one hour without it; no usage
  request is sent until the pause ends.
- Plan usage has its own pauses and runs even while the thread host is waiting.
  It reads the token in the same home order as the thread fetch and skips
  homes in the thread host's signed-out wait. A 401 from `query_v2` on one
  home moves to the next home on the same key; it does not start the thread
  host's wait. Only when every home on the key has been tried, or none is
  usable, does plan usage pause for that account, until the `auth.json` of any
  home on that login key changes or one hour passes. A `query_v2` 401 on a
  home is remembered in memory until that home's `auth.json` changes or one
  hour passes, so later passes skip it until then, and the plan usage line
  shows the signed-out message; thread sync keeps its own status. HTTP 403 or
  404 means OpenAI does not offer this usage to the account: AgentsView
  records "Usage isn't available for this account", sends no usage requests
  for that account for 24 hours, and shows no error. Other failures use the
  per-root backoff above.
- The fetcher resolves roots, settling, and the 24-hour pause from its state,
  and records the outcome in the mirror files: a response row (with
  `data_as_of` and whether it is final) is appended to the root's file only
  when it differs from the last stored row, and a thread found to be a
  descendant gets a "counted under `<root>`" record in its own file. The
  provider stays per-file: it keeps the newest plan usage record of each file.

**Storage.** Usage rows are running totals for the thread, not increments, and a
percentage of a weekly limit does not add up across weeks. So AgentsView keeps
the latest row per root, replacing the previous one, with its `data_as_of`,
`data_status`, `usage_source`, the thread-level percentages and credits, the
per-group values, and whether the row is final. It does not create token usage
events. Descendants have no plan usage of their own; when a thread's newest plan
usage record says it is counted under a root, the provider clears that session's
plan usage fields, keeps its coverage fields, and records the root as its parser
session ID (`codex-cloud:<rootId>`), and the fetcher drops it from the usage
retries. The root is a session reference: on a hosted hub, where sessions are
stored under raw projection IDs (`internal/postgres/raw_projection.go`), it is
mapped to the public ID like other session references
(`internal/postgres/hosted_session_ids.go`), so the parent link works there. The
same per-session table holds the live-capture coverage described under Live
token capture, so descendants and sessions without plan usage still have a row.
The builder chooses the table and follows `docs/agents/storage.md`, including
the full-resync copy of orphaned and trashed sessions, which must copy their
plan usage rows too. In PostgreSQL the table also needs the hosted-tenant schema
and privilege entries that `usage_events` has. Like usage events, a plan usage
write bumps the session's `local_modified_at` and is part of every backend's
push fingerprint, so replicas pick up changes.

**Replicas and mirror.** The plan usage table is pushed to the PostgreSQL
(including CockroachDB) and ClickHouse replicas the same way other per-session
data is, and added to the DuckDB mirror with a `SchemaVersion` bump and rebuild,
following `docs/agents/storage.md`. The rule that leaves plan-usage-only
sessions out of token and cost totals applies in every backend's usage queries
(the SQLite usage queries and usage cache, and the PostgreSQL, DuckDB, and
ClickHouse usage queries served through `internal/service`), so every backend
returns the same totals. Plan-usage-only sessions still produce activity facts
in the usage cache: each assistant message whose model is not `<synthetic>`,
including an empty model, counts as activity (`FromMessage` in
`internal/usagefacts/fact.go`), so per-model views show their message counts.
That is correct, since the work happened; activity facts carry no tokens or
cost, and every token and cost figure comes from usage events, which these
sessions lack. Per-model token and cost columns show such a model's plan-only
sessions as "Plan usage", not zero. The cache has no record of plan usage, so
the "sessions left out" count is computed at query time, not from the cache:
each backend counts sessions in its own copy of the per-session table that have
no usage events, applying the same usage filter as the totals (agent, project,
machine, model, exclusions, automated scope, and the date window in the
requested timezone). A plan-only session has no usage events, so the date and
model filters test the same inputs as its activity facts: its assistant
messages' times and models. A session matches when at least one of its assistant
messages does, using the same relaxed message rule and separate model filter
that `GetUsageMatchingSessionCount` already uses
(`usageMatchingMessageEligibility` in `internal/db/usage.go`). A plan-only
session with no assistant message is matched by its `started_at` and is excluded
whenever an include-model filter is set; an exclude-model filter does not
exclude it. The partly-captured count instead matches a session when at least
one of its usage events matches the window and model filters, as the totals do.
The cache's contents do not change, so its format version stays. A parity test
checks that SQLite, each replica, and the mirror return the same plan usage and
the same totals.

**Display.**

- Session details show "Plan usage": the share of the weekly Codex limit, the
  share of the five-hour Codex limit when present, and credits, split by
  model, reasoning effort, and speed, with "as of T" in local time. A note
  explains that credits are OpenAI's unit for usage beyond what the plan
  includes, so 0 credits means the plan covered the work. These credits are
  stored only in the plan usage table, never in the existing Copilot
  `ai_credits` fields, and `codex-cloud` does not set `AICreditsDenominated`.
  It also does not set `NoPerMessageTokenData`: the existing "no token data"
  notice (`UnsupportedUsage` in `internal/service/direct.go`) would fire for a
  plan-only filter and contradict the "sessions billed as plan usage" count,
  which replaces it for this source.
- Session details show one of these when there is no complete row:
    - "Plan usage: not checked yet": no request has finished.
    - "Plan usage: still updating, as of T": a `partial` row that is still
      pending.
    - "Plan usage: not reported yet, as of T": an `unavailable` row, or a root the
      response left out, that is still pending.
    - "Partial plan usage, as of T": a settled `partial` row.
    - "Not reported by OpenAI": a settled `unavailable` row, or a root that
      settled with no row.
    - "Counted under thread `<root title>`", with a link to the usage root's
      session, which can be a grandparent or further up: a descendant.
    - "Usage isn't available for this account": see Fetching.
- Sessions with no live token usage never appear with zero tokens or zero cost
  on any surface:
    - Session lists show "Plan usage" in the token and cost cells and sort these
      sessions the way sessions without token data already sort.
    - Totals in the UI and in CLI usage reports leave them out and say how many
      sessions were left out and why ("N sessions billed as plan usage, not
      priced").
    - The HTTP API keeps its existing shape: the presence flags (`has_token_data`,
      `has_cost`, and the session's `has_*` token flags) are false, and a
      separate plan usage object carries the plan figures.
    - Exports write empty cost and token values and include the plan usage fields.

### Configuration

- Settings live in the existing per-agent table, `[agents.codex-cloud]`:
  `enabled` (default `false`), `endpoint` (default
  `wss://codex-cloud-backend.chatgpt.com/`), `usage_endpoint` (default
  `https://chatgpt.com/backend-api/wham/usage/thread_usage/query_v2`), and
  `live_tokens` (default `true`). The endpoint overrides exist for tests and
  future moves. They must use `wss` and `https`; `ws` and `http` are accepted
  only for loopback addresses, so a typo can never send the token in clear
  text. `live_tokens = false` turns off live token capture, and with it the
  only request that touches a thread on OpenAI's side.
- The shared `[agents.<id>]` reader (`applyAgentDirectories`) parses only `dirs`
  and `homes` and silently ignores other keys. Add parsing and validation for
  these four keys; `dirs` and `homes` are unknown keys for this source. Today
  an error from that reader fails the whole config load. Errors in
  `[agents.codex-cloud]` must not take that path: an unknown key or a bad
  value is a config error for this source only: the error appears at startup
  and in the status area (see Status and errors), fetching and live capture
  stay off until it is fixed (existing mirror files still parse, as with
  `enabled = false`), and the rest of the config and every other agent still
  load. A different case: if `config.toml` as a whole fails to parse when a
  job re-reads it, the job keeps the last good settings and shows
  "`<config path>` could not be read: `<reason>`. Cloud import keeps its last
  good settings."
- `codex-cloud` is registered as a file-based provider so that `disabled_agents`
  accepts it (`internal/config/config.go` rejects other providers there). Its
  root is `{dataDir}/sources/codex-cloud`, resolved from the data directory
  rather than `$HOME` or an environment variable; the builder adds that kind
  of root to discovery, and `ResolveDirs` returns it, so remote host sync's
  forbidden roots (built from `ResolveDirs` in
  `internal/remotesync/resolve.go`) include it. It is excluded explicitly from
  the settings provider list, which otherwise shows every file-based provider.
- Unlike other agents, which are on unless listed in `disabled_agents`, fetching
  is off until the user sets `enabled = true`, because it uses a private,
  undocumented endpoint as the user's ChatGPT account. `enabled` gates
  fetching and live capture only: existing mirror files, and raw captures a
  hub receives, still parse.
- `disabled_agents` wins: if `codex-cloud` is listed there, nothing is fetched,
  captured, or parsed, even when `enabled = true`.
- When the daemon runs without source sync (`--no-sync`, or an archive-only
  archive, which sets `NoSync` in `cmd/agentsview/main.go`), both jobs do
  nothing and the monitor joins nothing; status shows "Off: source sync is off
  for this daemon."
- Poller jobs can only be registered at daemon start, so both jobs are always
  registered. The daemon reloads settings only when they are saved through the
  settings API, and this source is configured in `config.toml` only, so both
  jobs re-read `[agents.codex-cloud]`, `disabled_agents`, and
  `[agents.codex] homes` from `config.toml` at the start of each run.
  `CODEX_HOME` comes from the daemon's environment, which is fixed at start. A
  hand edit therefore changes fetching and live capture on the next run
  without a restart. Parsing follows the engine's in-memory provider list, so
  adding `codex-cloud` to `disabled_agents` by hand stops parsing only after a
  restart or a save through the settings API.
- The first version is configured in `config.toml` only. The settings screen
  does not show it yet.

### Status and errors

Poller status lives only in memory, and AgentsView has no general sync-error
display today. This source adds one for itself: a new status endpoint in the
daemon's HTTP API (the raw-sync status endpoint is the nearest existing
pattern), the UI's sync status area, and `agentsview serve status`. All times
are in the computer's local time.

The source appears only when the user has configured it: `[agents.codex-cloud]`
exists in `config.toml`, or `codex-cloud` is in `disabled_agents`. Users who
never set it up see nothing.

Each ChatGPT login shows one state, labeled with the Codex homes signed in to
it. A Codex home with no usable login (signed out, keyring-backed, or
`ephemeral`) has no login key, so it gets its own row with the matching message.
States:

- **Off: listed in `disabled_agents`.** Shown even when `enabled = true`, since
  `disabled_agents` wins.
- **Off: `enabled` is false.**
- **Off: source sync is off for this daemon** (`--no-sync` or an archive-only
  archive).
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

Live token capture has its own line: "Live tokens: watching N running threads"
(with "M joins arrived after the turn ended" when that count is not zero), "Live
tokens: off", or a live-capture error from the table below.

Plan usage has its own line under the home's state, so a usage problem never
hides or replaces thread status. It shows "Plan usage: not checked yet", "Plan
usage as of T", "Usage isn't available for this account", or a usage error from
the table below.

| Cause                                        | Message                                                                                                                                     |
| -------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| No token, or HTTP 401 from the thread host   | Signed out of ChatGPT in this Codex home. Run `codex login`, then AgentsView picks up the new login on its next sync.                       |
| HTTP 403 from the thread host                | OpenAI refused this request. AgentsView retries automatically. If this continues, check that this ChatGPT account can use Codex.            |
| Network failure                              | Can't reach OpenAI. AgentsView retries automatically.                                                                                       |
| HTTP 429                                     | OpenAI is limiting requests from this account. AgentsView retries at T.                                                                     |
| HTTP 5xx                                     | OpenAI's service had an error. AgentsView retries automatically.                                                                            |
| JSON-RPC error reply for one thread          | OpenAI returned an error for this thread. It may have been deleted.                                                                         |
| Handshake refused or reply not understood    | OpenAI changed its service in a way this version of AgentsView can't read. Update AgentsView. Sessions already imported are safe.           |
| Keyring-backed home, no token in `auth.json` | This Codex home keeps its ChatGPT login in the system keyring, which AgentsView does not read.                                              |
| `ephemeral` credential store                 | This Codex home does not save its ChatGPT login, so AgentsView cannot use it.                                                               |
| Bad value in `[agents.codex-cloud]`          | `<config path>` has an invalid value for `<key>` under `[agents.codex-cloud]`: `<reason>`. Cloud import is off until then.                  |
| HTTP 401 from plan usage                     | Plan usage: signed out of ChatGPT in this Codex home. Run `codex login`.                                                                    |
| Unknown key in `[agents.codex-cloud]`        | `<config path>` has an unknown setting `<key>` under `[agents.codex-cloud]`. Remove it or correct its name. Cloud import is off until then. |

Raw error details go to the daemon log, with the token redacted, the endpoint
named (thread host or plan usage), and the thread ID for per-thread failures.
The next successful pass clears the error.

### Several machines

The same ChatGPT account may be synced from more than one computer. Session IDs
come from the thread ID alone, so every computer produces the same ID for the
same thread. In PostgreSQL the first machine to push a session owns it; the
other machine's pushes of that session are skipped as ownership conflicts
(`internal/postgres/push.go`). Live tokens or plan usage captured only on the
second machine therefore never reach PostgreSQL, and both machines would join
running threads. The user-facing docs recommend enabling the source, or at least
live capture, on one machine only. The builder checks how ClickHouse handles the
same session from two machines and documents it.

## Testing

- A fake durable host on a local websocket server and a fake `query_v2` HTTPS
  server, built from synthetic fixtures. The fixtures contain no private
  names, paths, IDs, or tokens.

- Handshake: the token is sent only as the subprotocol, and handshake errors
  never contain it.

- Allowlist: a non-allowlisted method or URL fails without being sent.

- Incremental behavior: an unchanged account costs one `thread/list` call per
  listing. A thread that grows by k items transfers about k items plus the
  items after its oldest unfinished item in that turn, not the whole thread.
  Add the cardinality-scaling regression `docs/agents/background-work.md`
  requires: small versus large archive, with deletions, tombstones, and a
  persistent archive in the same test, unchanged work and the fetcher state
  read per pass both stay bounded.

- Time: hour- and day-scale behavior (backoff, settling, the 24-hour stall rule,
  waits) runs on an injected clock so tests do not wait in real time.

- State: the sync job and the monitor writing state for one account at once lose
  neither's changes; a usage group is built from the root's descendant list
  without reading other threads' state.

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

- Error codes: `-32601`, and `-32600` with an `Invalid request:` message or an
  `experimentalApi` message, fail the pass as a protocol change and wait as
  for a 403; `-32602` on an anchored `thread/items/list` re-reads the turn
  without the anchor in the same pass, and only a second failure puts the
  thread in the retry set; overloaded and shutting-down replies end the pass
  with the account wait and no retry-set entry; other error replies to
  `thread/list` fail the pass with the 5xx wait; a `-32600` "no rollout found"
  reply to a fetcher thread request puts that thread in the retry set; an
  `environment_terminated` reply to the monitor's `thread/resume` closes the
  connection and is retried on the next run.

- Failures: a thread that keeps failing and stays at the top of the listing is
  not fetched again before its next-attempt time, backs off to the one-hour
  cap, does not hold back the listing watermark, and is fetched once it
  recovers.

- Deletion (blocked on the deleted-thread probe in Open questions): a thread the
  server reports as gone leaves the retry set, is not fetched again, keeps its
  stored session, and does not keep the "could not be imported" state up.
  Until then: a thread whose fetch fails stays in the retry set at the
  one-hour cap and is counted in status.

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
  provider, survives recovery, and the next parse yields the same session with
  no duplicate items. A normal append checks only the bytes after the stored
  offset; a missing or too-large offset triggers a full scan.

- Parsing: an item that finishes after a resume point replaces its stored
  message or tool call, with no duplicate. A benchmark parses the largest
  synthetic fixture in full.

- Mapping: every item type in the protocol schema, plus an unknown type.

- Plan usage:

    - a newer row replaces the older one;
    - a usage request that fails during the first import is retried at its
      next-attempt time until a usable row arrives;
    - an unsettled `unavailable` or left-out root shows "Plan usage: not reported
      yet";
    - when due pending roots and due refresh roots exceed the 5-request budget,
      pending roots go first and leftover refresh roots run on the next pass;
    - a `partial` or `unavailable` row backs off and settles after 7 days without
      change, keeping its last values, and a later thread change reopens it;
    - a large backlog of pending roots sends at most 5 requests per pass;
    - nested sub-agent threads (child and grandchild) are all sent with their
      root, never as their own group, and a change in a grandchild refreshes the
      root;
    - a thread that turns out to be a descendant loses its plan usage values,
      keeps its coverage row, and shows "Counted under thread";
    - a root with a usable row is refreshed a day after its row's `data_as_of`
      while it changed in the past week, without scanning other roots;
    - a 403 from `query_v2` shows "Usage isn't available for this account", stops
      usage requests for 24 hours, and leaves thread sync status unchanged.

- Live token capture:

    - a thread listed as `idle` whose newest turn is `inProgress` is found and
      joined;
    - `warm` keeps the connection until `turn/completed`, writes the
      turn-completion record, flushes, then leaves;
    - a thread already joined is not joined again by a later discovery run;
    - a sub-agent thread marked only by `threadSource` or `source` is not joined;
    - a `model/rerouted` notification changes the model on later token records;
    - turns before the first join count as captured; a turn after the last capture
      that no joined connection saw complete counts as not captured;
    - usage events have a nil `MessageOrdinal`;
    - `cold` is logged, counted in status, and the connection closes at once;
    - an unknown `resumeKind` stops capture for the account with an error;
    - server-initiated requests while joined are never answered, except the token
      refresh, which gets an error;
    - token records carry the model from the `thread/resume` response;
    - after a baseline reset, the session's output total still includes the
      increases from before and after the reset;
    - a thread still running after 6 hours is left and joined again on a later
      run;
    - a running sub-agent thread is found and not joined;
    - discovery reads a second listing page while the first page holds only recent
      or `active` threads, and stops after the first page that holds none; a
      running thread listed `idle` on a later page is found through the sync
      job's unfinished set;
    - an ordinary error reply to `thread/resume` is retried on the next run and
      does not stop capture; `-32601`, or `-32600` with an `Invalid request:` or
      `experimentalApi` message, stops capture for the account without setting
      the sync job's wait with the protocol-change error; an overloaded reply
      sets the shared account wait;
    - a thread joined before sync mirrored it gets a mirror file that starts with
      its metadata; a mirror file with no metadata is skipped;
    - after a decrease, the first record of the new baseline adds its full total,
      and the sum across baselines is exact;
    - a turn whose join ended (6-hour leave or dropped connection) before it
      completed, with no later capture, counts as not captured;
    - a 429 seen by sync stops live discovery requests for that account;
    - a missing `resumeKind` is treated as unknown;
    - the session's output total and peak context come from captured totals;
    - no more than 10 threads per account are joined at once;
    - successive totals become `session` usage events equal to their increases,
      including increases above 2,000,000 tokens, each with a non-empty model
      and `ReasoningTokens` 0; a decrease starts a new baseline with no negative
      event; a total that recurs after a new baseline does not collide; a
      repeated total after a reconnect adds nothing;
    - `live_tokens = false` sends no `thread/resume`;
    - many token updates within a minute produce one mirror append, and turn
      completion flushes the buffer and triggers sync;
    - the monitor's allowlist rejects `thread/resume` with any parameter other
      than `threadId` and `excludeTurns: true`.

- Failure timing: a signed-out home waits for an `auth.json` change or an hour,
  not the scheduler's shorter retry; one account's failure or 429 does not
  delay another account.

- Fetcher state: a full resync leaves state files intact and fetches nothing
  again; lost state files lead to a re-fetch with no duplicate session items.

- Raw capture: a hub that receives the mirror files parses the same sessions,
  and a hosted hub's PostgreSQL projection writes their plan usage and
  coverage rows, with the "counted under" root mapped to its public session
  ID; state files are never captured.

- Full resync: orphaned and trashed sessions keep their plan usage rows.

- Plan usage 401: pauses plan usage only; thread sync status is unchanged, and
  plan usage resumes after the `auth.json` of any home on the key changes,
  including a home other than the first. After a `query_v2` 401 on the first
  home, plan usage uses the next home's token, and runs while the thread host
  is waiting.

- Plan usage 404 and 429: a 404 behaves like a 403; a 429 pauses usage requests
  for the `Retry-After` value.

- Gates: setting `enabled = false`, `live_tokens = false`, or listing
  `codex-cloud` in `disabled_agents` flushes and closes every joined
  connection. A `--no-sync` or archive-only daemon sends no request to OpenAI
  and shows its Off state. A config error in `[agents.codex-cloud]` also
  flushes and closes every joined connection.

- Sync trigger: several joined turns completing together queue at most one extra
  sync run, and `ErrTriggerPending` is not treated as an error.

- Server requests: the fetcher and the monitor answer only the token-refresh
  request, with an error, and leave an approval request unanswered.

- Later turns: a thread whose new turn started after the resume point gets that
  turn's items fetched in full.

- Sub-agent threads: a sub-agent thread is listed, mirrored, and linked to its
  parent.

- Turn listing: a fetch of a long thread reads only the turns since the resume
  point.

- Stopped threads: an unchanged account whose threads are listed as `notLoaded`
  costs one `thread/list` call per listing and no turn checks.

- Turn lag after a new turn: an item saved mid-flight in turn N is fetched again
  for the margin after turn N+1 starts.

- Usage left out: a root every response leaves out settles after 7 days and
  shows "Not reported by OpenAI".

- Filtered totals: the "sessions left out" and "sessions partly captured" counts
  follow the same filters as the totals in every backend, and the parity test
  covers both; a plan-only session is counted as left out, never as partly
  captured.

- Accounts: two homes signed in as different users of one workspace sync
  separately; two homes signed in as the same user sync once. A keyring-backed
  home shows the keyring message, an `ephemeral` home its own message, and a
  `keyring` home with a leftover token in `auth.json` never uses that token.
  With two homes on one key, a 401 on the first falls back to the second, sync
  succeeds, and the first home's signed-out wait starts.

- Turn lag: an item saved mid-flight in a turn that just ended is fetched again
  until the margin passes, and its final version is stored.

- Late parent: a child mirrored before its parent moves under the parent's root
  when the parent arrives, and the root becomes usage-pending.

- Raw ingest accepts `codex-cloud` uploads; remote host sync does not list the
  mirror directory.

- Mapping: the thread's model and reasoning effort appear on its assistant and
  thinking messages. Capture coverage (last capture time and uncaptured turn
  count) is stored per session and returned the same by every backend.

- Cost surfaces: sessions with only plan usage are left out of token and cost
  totals in the UI, CLI reports, API, and exports; the API reports their
  presence flags as false; totals state how many sessions were left out; no
  surface shows them as zero cost. A session with live token usage counts in
  totals like any other agent.

- Replicas and mirror: SQLite, PostgreSQL, ClickHouse, and DuckDB return the
  same plan usage and the same totals for the same data, and a plan usage
  change reaches each of them.

- Config: a `config.toml` that fails to parse on re-read keeps the last good
  settings and shows its message. An endpoint override with `http` or `ws` to
  a non-loopback host is rejected. An unknown key in `[agents.codex-cloud]`
  produces the message above with the config path, the source stays off, and
  local agents still load. `disabled_agents` wins over `enabled = true` and
  shows its own Off state. A hand edit to `[agents.codex-cloud]` takes effect
  on the next run without a restart; `enabled = false` still parses existing
  mirror files.

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
  private-endpoint caveat, what live token capture does on OpenAI's side and
  how to turn it off, why turns AgentsView never saw running show plan usage
  instead of tokens, and these limits, in the user-facing docs:
    - a thread's first token record is dated when it was observed, even when
      earlier turns ran on an earlier day;
    - two model changes the monitor does not see (a change only
      `thread/settings/updated` reports, and a reroute before the join);
    - running sub-agent threads are not joined until the probe in Open questions
      is done;
    - enable the source, or at least live capture, on one machine only (see
      Several machines);
    - a turn that starts without changing the thread's `updatedAt` or status for
      more than 30 minutes is not captured live.

## Open questions

1. **Turn listing view.** The probes called `thread/turns/list` with `limit`
   only. Newest first is the default, so no `sortDirection` is sent. Before
   the fetcher and the live token monitor rely on `itemsView: notLoaded`, the
   user runs one read-only probe with it on the durable host. If it is
   rejected, both omit it and pay the cost of the default `summary` view.

1. **How the server reports a deleted thread.** The user runs a probe that reads
   a thread they have deleted (for example after resetting a dot) and records
   the exact reply. Until then, no reply counts as a deletion.

1. **Joining sub-agent threads.** The resume probes covered dot threads only.
   Before the monitor joins sub-agent threads (as defined under Joining), the
   user runs the same warm and cold resume probes on a running and a stopped
   sub-agent thread. Until then those threads are not joined.

1. **Descendant threads.** The desktop app finds a thread's sub-agent threads
   with a local discovery step. No listed thread had a `parentThreadId`, so
   whether `parentThreadId` finds them all is unverified. The builder checks
   this against a thread that used sub-agents; `thread/list` also accepts
   `parentThreadId` and `ancestorThreadId` filters, which give a direct check.
   Those filters are experimental and need the `experimentalApi` capability in
   `initialize`, which the fetcher does not send, so the check is a probe the
   user runs.

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
- Live token capture race: a turn that ends between the monitor's check and its
  `thread/resume` makes the call a cold resume. The tests showed what that
  does: the thread is loaded on OpenAI's side and starts its MCP servers;
  nothing runs and nothing changes. The monitor detects it from
  `resumeKind: "cold"`, logs it, and leaves at once.
- OpenAI may change what `thread/resume` does on a running or stopped thread.
  The monitor checks `resumeKind` on every call and stops live capture for the
  account on any value other than `warm` or `cold`.

## Left to the builder

- Package and type names, file layout, and how the fetcher hands newly mirrored
  threads to the sync engine.
- Page sizes within protocol limits.
- The table for plan usage rows and how session details and cost views render
  them, within the rules under Plan usage.
