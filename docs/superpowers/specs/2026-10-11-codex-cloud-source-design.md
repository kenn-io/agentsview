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

- The server accepted AgentsView's identity in `User-Agent`, `originator`, and
  `clientInfo.name`. These probes still sent the `codex-client.desktop`
  subprotocol; Open questions records the later subprotocol probe.
- `thread/read` and `thread/turns/list` (with `limit`) also answered. Turn
  records carry `id`, `rootTurnId` (which can differ from `id`), `status`,
  `startedAt`, `completedAt`, `durationMs`, `error`, `items`, and `itemsView`.
  Coverage and "newest turn" use `id`. Without the `itemsView` request
  parameter, the hosted server answered with `itemsView: "full"`: every turn
  came back with all its items (199 in one turn). Open-source Codex defaults
  to `summary` instead, so the hosted default differs.
- 33 active and 3 archived threads, all with `source`
  `{"custom": "codex-cloud-agent"}`. The default listing (no `sourceKinds`)
  returned them all. A listing with `sourceKinds` set to every
  `ThreadSourceKind` returned no threads, because the custom source matches no
  kind. No sub-agent thread was seen in either listing, and the dot's own
  conversation is not on this server.
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
    - Request: headers `Authorization: Bearer <access token>`,
      `Content-Type: application/json`, AgentsView's `User-Agent` and
      `originator`, and `ChatGPT-Account-Id` when the login has an account ID;
      body `{"threads": [{thread_id, created_at, descendant_thread_ids}]}`,
      where `created_at` is the thread's `createdAt` (Unix seconds) as an ISO
      8601 UTC timestamp string. The websocket's rejection of a bearer header
      does not apply here. The app sends at most 100 groups and 1,000 thread IDs
      per request, and puts each thread's sub-agent threads in
      `descendant_thread_ids` so their usage counts toward the parent.
    - Response `{data_as_of, comparison_plan, threads}`. `data_as_of` is an ISO
      8601 UTC string; the percentages are numbers or null; credits are decimal
      strings or null. Each row has `thread_id`, `data_status` (`available`,
      `partial`, or `unavailable`), `usage_source` (for example
      `included_plan`), `weekly_limit_percent`, `five_hour_limit_percent`,
      `balance_usage_credits` (a decimal string, which can use exponent form
      such as `0E-10`), and `groups` of
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
  dot thread joined it. The response had `resumeKind: "warm"` (seen in the
  probe's console output; the saved output file lacks the resume response).
  The server then sent `thread/tokenUsage/updated` with params
  `{threadId, turnId, tokenUsage}`, where `tokenUsage` holds `total` (running
  totals since the thread started), `last` (the latest model call), and
  `modelContextWindow`; `total` and `last` each have `inputTokens`,
  `cachedInputTokens`, `cacheWriteInputTokens`, `outputTokens`,
  `reasoningOutputTokens`, and `totalTokens`. Every counter, and
  `modelContextWindow`, arrived as a JSON number with a fractional part (for
  example `4603049.0`). The dot sent no requests to AgentsView and kept
  working.
- `thread/unsubscribe` is unsupported on the durable host (`-32601`,
  "unsupported app-server method: thread/unsubscribe"). Closing the connection
  leaves the thread.
- `thread/resume` on a stopped, unarchived dot thread (all turns completed)
  returned `resumeKind: "cold"`. It started no turn and no new environment
  (the environment ID was unchanged), and `thread/read` afterward showed the
  thread's status, `updatedAt`, and items unchanged (seen in the probe's
  console output; the saved file lacks that snapshot). The resume response
  itself carried `updatedAt` and `recencyAt` set to the time of the resume.
  Whether the listing order moved was not recorded. It did load the thread
  into a new server-side process, which started every MCP server configured
  for the dot (seven ready, seven failed to start) and sent one warning. It
  sent no token totals, with or without turns requested.
- On an archived thread, `thread/resume` failed with JSON-RPC error `-32600`
  whose `data.errorType` is `environment_terminated`, and changed nothing.
- The listing's `status` can be stale: a thread listed as `idle` had a turn in
  progress and kept adding items.
- Saved items lag the live work: during a running turn, `thread/items/list` did
  not grow for minutes while live item notifications arrived.

Thread data, 2026-10-13, from the account owner's listing:

- 30 of 39 threads were delegated dot tasks: `threadSource: "aeon_child"`,
  `parentThreadId: null`, and `source` `{"custom": "codex-cloud-agent"}`. The
  parent is named only in the thread's first `userMessage`, which starts with
  `<codex_delegation><source_thread_id>PARENT-UUID</source_thread_id>`. Later
  messages from the parent arrive as `functionCallOutput` items (name
  `send_message`, namespace `cloud_threads`) carrying the same tag. The parent
  is the dot's own conversation, which is not listable.

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
- Token counts for threads AgentsView never joined while they ran, including
  turns that end before the monitor joins them. OpenAI does not return them
  for plan-billed accounts (see Background); those threads have plan usage
  only. A thread joined once gets its earlier turns through the first running
  total (see Live token capture).
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
  `thread/resume` (see Live token capture). In open-source Codex a join also
  records the joining client's name and version in the running session, which
  later turns' initial context uses; the hosted server likely does the same,
  so a joined thread's later turns may see AgentsView named as a client (see
  Risks).
- Restarting a dot's cloud environment, starting a turn, or answering a dot's
  approval prompt or question.
- The ChatGPT token appearing in logs, errors, files, the archive, or test
  fixtures.
- Network cost growing with the size of the archive or with the square of a
  thread's length, or local work per pass growing with the size of the
  archive. Two exceptions are accepted. The cases within one running turn are
  listed under Risks. Locally, the sync engine lists every mirror file on each
  pass to compare fingerprints, as it does for every file-backed agent; that
  is a directory walk and a file status check per thread, with no network call
  and no parse of an unchanged file.
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
   plus one HTTPS call for plan usage. That library limits an incoming message
   to 32 KiB by default and closes the connection when a message is larger,
   while the probes' replies reached about 530 KB, so the client sets an
   explicit, bounded read limit sized for the largest page (the builder picks
   it, in the tens of MiB). A reply over that limit closes the connection; the
   fetcher reconnects, retries an oversized `thread/items/list` page once with
   `limit: 1` (an oversized turn listing is not retried, because with
   `itemsView: notLoaded` its turns carry no items), and only then records a
   failure of that thread (retry set, "reply too large") and continues the
   pass with the next thread, so one huge page never fails the whole account.
   An allowlist enforced in the client permits only `initialize`, the
   `initialized` notification, `thread/list`, `thread/items/list`, and
   `thread/turns/list` on the websocket, and only the configured
   `usage_endpoint` URL over HTTPS (or HTTP for a loopback override; see
   Configuration). Anything else fails before it is sent. The fetcher joins no
   thread and never answers a server-initiated request, so it can never
   decline an approval prompt or race another client's answer. On a token
   refresh request (`account/chatgptAuthTokens/refresh`) it closes the
   connection without answering and ends the pass early: what was already made
   durable stays, the cut-off threads go to no retry set, the watermarks do
   not advance, and the plan usage step does not run. The request starts the
   refresh-requested wait for that Codex home (see Failure timing), so passes
   do not end early over and over while nothing refreshes the token.
   Notifications are ignored.
1. **Live token monitor.** A second client mode used only for live token
   capture. Its allowlist adds `thread/resume`, accepted only with `threadId`
   and `excludeTurns: true` and no other parameters. Like the fetcher, it
   never answers a server-initiated request (see Live token capture).
1. **Scheduler.** `poller.Job` values, which `docs/agents/background-work.md`
   requires for interval jobs against external APIs: one for sync and one for
   live token discovery. Both use the scheduler's jitter and cooldown. Sync
   runs every 5 minutes; live token discovery every 30 seconds. When a joined
   turn completes, the monitor calls the sync job's `TriggerNow` from its own
   goroutine, because the call blocks until the run ends, so re-fetching the
   trace starts without waiting for the next interval. When a trigger is
   already pending, `TriggerNow` drops the new request and returns a formatted
   error today; the pending trigger still runs. The builder adds an exported
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
   and implements both `PlanRawCapture` and
   `StreamingRawCaptureSourceProvider` (`DiscoverRawCaptureSourcesEach`), all
   in `internal/parser/raw_capture.go`; raw discovery rejects a provider that
   declares the capability without the streaming interface, and hosted
   derivation uses that same discovery. This is so that each thread's mirror
   file is captured and fetcher state files never are. Mirror files declare
   `RawCaptureAppendOne` (`internal/parser/raw_capture.go`): each pass appends
   frames, and a torn-frame truncation during recovery changes the existing
   prefix, which forces raw capture to replace that file rather than extend
   it. The agent definition must not set `RemoteSyncExcluded`: raw ingest
   rejects every provider with that flag (`validateProvider` in
   `internal/rawsync/manifest.go`), which would stop the mirror from reaching
   a hub. Remote host sync (`internal/remotesync`) instead skips this source
   through a second, narrower definition flag, which the builder adds. Today
   the helper `parser.RemoteSyncExcludedAgent` (`internal/parser/types.go`)
   returns true only for `RemoteSyncExcluded`; the builder changes it to
   return true when either flag is set, and makes every remotesync check use
   it, including the two that read `RemoteSyncExcluded` directly today
   (`internal/remotesync/resolve.go`, target resolution and its forbidden
   roots), so remote host sync fails closed and never ships the mirror
   directory or its state files. Raw ingest (`validateProvider` in
   `internal/rawsync/manifest.go`) keeps reading only `RemoteSyncExcluded`.

### Authentication

- Sync every ChatGPT account signed in to one of these Codex homes: `CODEX_HOME`
  from the daemon's own environment if set, otherwise `~/.codex`, plus every
  `[agents.codex] homes` entry. `dirs` and `CODEX_SESSIONS_DIR` point at
  session folders, not homes, and are ignored. Do not rely on `CODEX_HOME`
  from the user's interactive shell, which a background daemon usually doesn't
  inherit.
- Key each login by its ChatGPT workspace and user. The ID token's
  `https://api.openai.com/auth` claim carries the user ID (`chatgpt_user_id`,
  else `user_id`); the ID token is decoded in memory only. The key is
  `tokens.account_id` plus the user ID, or the user ID alone when the login
  has no account ID. `account_id` alone names a workspace, so two users of one
  Business or Enterprise workspace stay separate. The optional
  `chatgpt_account_user_id` claim is not used, because an older ID token can
  lack it and the same login would then get two keys. Homes with the same key
  sync once, using the first home in the order above; if that home's token
  gets HTTP 401, the next home with the same key is tried before the login is
  reported signed out. Sync state and mirror files are kept per key, in a
  directory named `<accountKey>` that the builder derives from the pair; it
  never contains token material.
- AgentsView reads `auth.json` only when the home's Codex config sets
  `cli_auth_credentials_store` to `file` (the default) or `auto`. In `keyring`
  and `ephemeral` modes Codex ignores `auth.json`, so a leftover token there
  is stale and is never used. A home in `keyring` mode, or in `auto` mode with
  no token in `auth.json`, is keyring-backed. In `auto` mode Codex reads the
  keyring first and deletes `auth.json` after a keyring save, logging only a
  warning if that fails, so a leftover `auth.json` can be stale: when its
  token gets HTTP 401 in `auto` mode, AgentsView says OpenAI did not accept
  the login in `auth.json` and that Codex may keep the current one in the
  keyring, not the signed-out message. AgentsView does not read the keyring. A
  home in `ephemeral` mode keeps its login only in memory. Each of these
  messages (see Status and errors) gives the fix: set
  `cli_auth_credentials_store = "file"` in the home's `config.toml` and run
  `codex login`. Each also warns that this stores the ChatGPT login in a plain
  file that any program running as the user can read.
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
  the durable host and `query_v2` both accept this. The client subprotocol
  must not name the desktop app either: AgentsView sends
  `codex-client.agentsview`, which the server accepts, and the server selects
  `codex-app-server` (see Open questions). If a later server release refuses
  it, stop and ask the user; never fall back to the desktop app's identity on
  your own.

### Failure timing

These rules cover the thread host. Plan usage has its own pauses (see Plan
usage), so a `query_v2` failure never pauses thread sync.

The scheduler retries a failed job sooner, not later: it waits `interval/16`
after the first failure and doubles up to the interval
(`internal/poller/poller.go`). Its backoff and `RetryAfterError` also apply to
the whole job, so one account's failure would change the timing for every
account. So both jobs share one persisted next-attempt time per account for the
thread host (any account-level failure seen by either job sets it, except a
protocol change seen by the monitor or discovery; see JSON-RPC errors), kept in
`account.json`, and return no error to the scheduler for one account's failure.
A 429, 403, 5xx, or network failure seen by sync also holds back live discovery,
and the reverse. The waits below are minimums: a job checks them at each
scheduled run, so a wait shorter than the job's interval ends at the next run.

- Signed out (no token, or HTTP 401): kept per Codex home, since a home with no
  token has no account key; this wait is kept in memory and resets on daemon
  restart. Retry when that home's `auth.json` changes, or after one hour.
- Refresh requested (the thread host sent `account/chatgptAuthTokens/refresh`):
  kept per Codex home in memory, like signed out. Thread sync, plan usage,
  live discovery, and live joins skip that home until its `auth.json` changes,
  or one hour passes; AgentsView never refreshes the token itself. As after a
  401, the next pass uses the next home with the same login key if there is
  one; the pass that saw the request still ends early.
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
- **Protocol change:** `-32601` whose message starts with
  `unsupported app-server method` (the durable host answers an unsupported
  method this way; any other `-32601` on a per-thread request is an error for
  that thread), or `-32600` whose message starts with `Invalid request:` or
  names `experimentalApi`. The fetcher fails the account's pass with
  "Handshake refused or reply not understood" and waits as for HTTP 403 (one
  hour, doubling to 24 hours); the monitor, and the live discovery job for its
  own requests, stop live capture for the account with the same error, as for
  an unknown `resumeKind`, and does not set the sync job's wait; sync finds
  the change on its own next request.
- **Rejected parameter elsewhere:** `-32602` in reply to any call other than an
  anchored `thread/items/list`, for example a rejected `itemsView` on
  `thread/turns/list`, is a protocol change (above), not a per-thread error.
- **Error on an anchored item read:** any `-32602` in reply to a
  `thread/items/list` call that used an item anchor. In the same pass, the
  fetcher re-reads that turn in full without the anchor, skips records for
  items before the resume point, and puts the thread in the retry set only if
  that read also fails.
- **Any other error reply** (any other code, or a `-32600` with another message
  such as "no rollout found") in reply to `initialize` or `thread/list`: the
  account's pass fails with the HTTP 5xx wait and the "OpenAI's service had an
  error" message. In reply to a per-thread request (`thread/turns/list`,
  `thread/items/list`, or the monitor's `thread/resume`): the "error for this
  thread" category. The fetcher puts the thread in the retry set; the monitor
  closes that connection, logs it, and retries on the next run if the thread
  is still running; discovery, for a thread it has not joined, skips that
  thread and continues the run. This covers `environment_terminated` (`-32600`
  with that `data.errorType`) and a missing thread.

### Fetcher state

The fetcher keeps its sync state beside the mirror files, outside SQLite, in two
kinds of file, each written atomically:

- **Per thread:**
  `{dataDir}/sources/codex-cloud/<accountKey>/<threadId>.state.json` holds
  that thread's last mirrored `updatedAt`, resume point, verified mirror
  offset, resolved parent (see Mapping), and for each turn its stored status,
  when the fetcher first saw the turn, and when it first saw the ended status
  (for the 10-minute rules), when it first saw the thread archived, plus the
  time of the last new item or `updatedAt` change (for the 24-hour stall
  rule), digests of the newest stored thread metadata record, of each turn,
  and of each item after the resume point (to tell whether a re-fetched record
  changed), the thread's `createdAt` (for usage groups), the digest and kind
  of the last plan usage record of any kind in its file (updated by every
  writer of a row or outcome record), and the root it is currently counted
  under. Every writer that appends to a mirror file (step 7, the plan usage
  step, a late-parent move, and the live token monitor) saves the fields it
  changed together with the new offset, after the fsync. The changed-threads
  step reads it for each thread on the listing pages it walks; otherwise it is
  read and written only when the thread is selected, joined by the monitor, or
  walked for a usage root (below). Live discovery reads neither this file nor
  the account's working sets: it decides from the listing record and the
  newest turn (see Finding running threads).
- **Per root:**
  `{dataDir}/sources/codex-cloud/<accountKey>/<rootId>.descendants.json` lists
  that usage root's known descendants. When a thread with a resolved parent is
  mirrored, the fetcher follows the parent links through the per-thread files
  to its root and adds it to that root's list. If the parent has not been
  mirrored yet, the child is recorded in a waiting file for that parent,
  `{dataDir}/sources/codex-cloud/<accountKey>/<parentId>.waiting.json`, and is
  treated as a root until then. A waiting file is read only when its parent is
  mirrored, so parents that never appear (a dot's own conversation is not
  listable) cost nothing per pass. When that parent is mirrored, the fetcher
  moves each waiting thread, with its own descendant list, under the parent's
  root, marks that root usage-pending, and writes a "counted under" record for
  the moved thread and for every thread in its descendant list. It then
  removes the moved thread's own `.descendants.json`, refresh-set entry, and
  usage-pending mark. Building a usage group reads only the root's list, so
  the work follows the threads that changed, not the size of the archive.
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
or account as never synced and fetches them again. A state file that does not
decode counts as lost: it is moved to `<name>.corrupt` and logged. A lost
per-thread file leaves a stored offset of zero, so the next append treats every
frame in the mirror file as kept frames and rebuilds the digests from them (see
Mirror format); only records that changed are appended. If a thread's mirror
file is missing or shorter than its stored offset, the fetcher resets that
thread's resume point and forgets its digests before fetching, so the fetch
reads the whole thread and writes it again.

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
  `thread/turns/list`) and the save-lag margin has passed for that end. The
  margin has passed when the turn's `completedAt` is at least 12 minutes
  before this fetch started: 10 minutes of save lag plus the 2-minute clock
  allowance used under Check-again threads, since `completedAt` comes from
  OpenAI's clock. When `completedAt` is missing or inside the margin, the
  fetcher uses when it first saw the ended status instead, and the margin
  passes 10 minutes after that. So the first import of a long-finished thread
  needs no repeat fetch. Saved items lag the live work by minutes, so an item
  saved mid-flight in a just-ended turn stays unfinished, and its thread stays
  in the unfinished set, until that margin passes. The 10-minute margin is a
  guess from the probes (see Open questions);
- a later turn in the same thread has started and the save-lag margin has passed
  for its start, judged the same way from its `startedAt`, so a turn that
  never reports a final status cannot pin the resume point forever, and the
  save lag is still covered;
- the thread is archived and the fetcher first saw it archived at least 10
  minutes before this fetch started. Archiving does not end the save lag: a
  user can archive a thread seconds after its turn ends, while a command's
  final output is still unsaved;
- `completedAtMs` is set and, for items with a `status`, that status is
  terminal.

The resume point stops at the first unfinished item, because Codex runs tool
calls in parallel and a later command can finish before an earlier one; skipping
past an unfinished item would lose its final output. The resume point also never
leaves a turn until the save-lag margin (below) has passed for that turn's end
or for the start of a later turn, even when every item saved so far is finished:
items saved after a turn ends would otherwise fall behind the resume point and
never be fetched.

Per account, the fetcher stores:

- a **listing watermark** for each of the four listings (below): the newest
  `updatedAt` that listing returned in its last complete pass. They are
  separate, so an update that lands between two listings is never hidden by
  another listing's newer timestamps;
- a **retry set**: threads whose last fetch failed, each with an attempt count,
  a next-attempt time, the time of the first failure, and the thread's latest
  listing record. A retry starts by appending that record when it differs from
  the stored metadata, because the watermark may since have moved past the
  thread's listing page and a mirror file without a metadata record is never
  parsed. When any listing returns a thread that is in the retry set, the
  entry's listing record is replaced with the new one, even when backoff skips
  the fetch;
- for each thread: the per-thread fields listed under Fetcher state, and in the
  account's working sets a **check-again flag** (below).

There are four listings: the default listing and the sub-agent listing, each for
active and for archived threads. The sub-agent listing sets `sourceKinds` to
`subAgent`, `subAgentReview`, `subAgentCompact`, `subAgentThreadSpawn`, and
`subAgentOther`. In open-source Codex, a listing with no `sourceKinds` returns
only interactive sources, which leaves out sub-agent threads
(`codex-rs/app-server/src/filters.rs`). The hosted server's default listing
returned threads outside that set, so it differs, and whether it includes
sub-agent threads is unverified (see Open questions). The sub-agent listing
covers them either way, at one extra call per listing when nothing changed. Each
pass reads the four listings, then builds one selection for the account: the
union of every listing's changed threads with the account-wide unfinished set,
check-again flags, and retry set, so a thread is fetched at most once per pass.
Each listing's watermark advances only after that listing's own changed threads
are durable or in the retry set. The steps, with the first and last run per
listing:

1. **Changed threads.** Call `thread/list` with `sortKey: updated_at`,
   descending, one page at a time, with that listing's `sourceKinds` (none for
   the default listing). A listing with every source kind returned no threads,
   so the default listing must not set `sourceKinds` (see Background). Collect
   threads whose `updatedAt` differs from the stored value. `updatedAt` has
   whole-second resolution, so several threads can share a timestamp: stop
   only after the first page that contains a thread strictly older than the
   listing watermark, and finish that whole page. With no watermark (first
   run, or a backfill that never finished), page through everything. An
   unchanged account costs one call per listing. The early stop is safe only
   if the server really sorts by `updatedAt`. The listing probe in Open
   questions found `updated_at` descending order within and across pages;
   still, a page whose `updatedAt` values ever increase from one row to the
   next, on the same page or from the previous page, is a protocol change for
   the account, never an early stop. Listing calls never send `modelProviders`:
   the server rejects it with `-32602`. Known limit: in open-source Codex a
   listing without `modelProviders` returns only threads of the server's own
   provider. Every probed thread had `modelProvider: "local"`, so a thread
   under another provider, if the hosted server ever has one, would not be
   imported.
1. **Unfinished threads.** Also include every thread that the listing shows as
   `active`, whose last fetch saw a turn in `inProgress`, that has unfinished
   items after its resume point, or that has a turn whose save-lag margin has
   not passed, so items saved after the turn ended are still fetched. Other
   statuses (`idle`, `notLoaded`, `systemError`) do not select a thread: a
   stopped thread can be listed as `notLoaded`. The listing's `status` can say
   `idle` while a turn runs, so the stored turn statuses decide. A command can
   run silently for longer than the poll interval, saved items can lag the
   live work by minutes, and it is unverified whether finishing an item
   changes the thread's `updatedAt`, so these threads are fetched every pass
   until their turns finish. A turn that stays `inProgress` with no new item
   and no `updatedAt` change for 24 hours stops this every-pass selection,
   including selection because the listing shows the thread as `active` or
   because its check-again flag is set; a later `updatedAt` change selects the
   thread again. A thread retired under Unreadable threads is not selected
   because the listing shows it as `active`.
1. **Check-again threads.** Also include every thread whose check-again flag is
   set. `updatedAt` comes from OpenAI's clock and the fetch start from the
   local clock, so both rules below allow 2 minutes of clock difference. A
   fetch sets the flag only when it started less than 2 minutes after the
   thread's `updatedAt`, so an update later in that same second could be
   hidden behind an equal timestamp. A fetch of an older thread, such as a
   first import of a finished thread, does not set it. A later fetch clears it
   only when that fetch starts at least 2 minutes after the thread's stored
   `updatedAt`, finds no change, and the thread is no longer selected as
   unfinished. "No change" means the listing returned the same `updatedAt` and
   status (a thread beyond the listing pages walked this pass counts as
   unchanged there), and the item fetch returned no record that differs from the
   newest stored record for that item. A second update within the same second
   as the first is therefore always fetched, however long the poll interval.
1. **Retry set.** Add retry-set threads whose next-attempt time has passed.
1. **Apply backoff.** Remove from the selection every thread in the retry set
   whose next-attempt time has not passed, however it was selected. A failing
   thread that stays at the top of the listing still waits out its backoff.
1. **Fetch.** For each selected thread, first call `thread/turns/list` for the
   turn statuses, newest first (the default order) with `itemsView: notLoaded`
   (never omitted: the hosted default is `full`, which returns every item; see
   Background), stopping at the turn that holds the resume point, so the
   call's cost follows the turns since the resume point, not the thread's
   length, then `thread/items/list` from the resume point, using an item
   anchor `{type: "item", itemId}` in the `cursor` field plus its `turnId`.
   The fetcher discards any returned record for the anchor item itself, so it
   never depends on whether the anchor is inclusive. An item anchor reads only
   within that turn (the protocol requires `turnId` with it), so the fetcher
   then reads each later turn in full by its `turnId`, oldest first, paging
   with the returned cursor. Reading turn statuses first means a turn that
   ends between the two calls is not yet treated as ended, so the items it
   finalizes are fetched again on the next pass.
1. **Make it durable, then advance.** Append to the mirror only the fetched
   records that differ from the newest stored record for the same key (thread
   metadata, turn, or `(turnId, item.id)`), compared by the digests in the
   per-thread state, so an unchanged re-fetch leaves the mirror file
   byte-identical and causes no parse. Then fsync the file (and its directory
   when the file is new, except on Windows, which cannot sync a directory) if
   anything was appended, and only then save the thread's new `updatedAt`,
   resume point, and mirror offset, and remove it from the retry set. A crash
   before the save repeats the fetch; it never skips records.
1. **Failures.** A thread whose fetch fails joins or stays in the retry set with
   exponential backoff, capped at one hour, and the pass goes on. The latest
   failure category (see Status and errors) is stored with it. JSON-RPC error
   replies follow the rules under JSON-RPC errors (below Failure timing).
1. **Deleted threads.** No probe has seen a deleted thread, so the exact reply
   to `thread/turns/list` and `thread/items/list`, the only per-thread calls
   the fetcher sends, is not known (see Open questions). Only that specific
   not-found reply counts as a deletion; any other error goes to the retry
   set. Until the probe identifies the reply, nothing counts as a deletion,
   and a deleted thread ends through the next step. A deletion is final:
   remove the thread from the retry set, clear its check-again flag and any
   usage-pending mark, and stop fetching it, unless a later listing shows it
   with a new `updatedAt`, which selects it again. Everything already stored
   stays.
1. **Unreadable threads.** A thread that has failed for 7 days, with no new
   `updatedAt` in any listing in that time, stops being retried. This covers a
   deleted thread before the deletion reply is known, and a thread with one
   item larger than the read limit even at `limit: 1`. The fetcher removes it
   from the retry set, the unfinished set, and the check-again flags, saves
   the retry entry's listing `updatedAt` as the thread's stored `updatedAt`,
   marks it retired in `account.json`, and logs the thread ID with its last
   failure. Everything already stored stays, and the thread no longer counts
   in the "could not be imported" status. While it is retired, a listing that
   shows it as `active` does not select it, so OpenAI keeping a failing thread
   `active` cannot restart its retries forever. A later listing with a new
   `updatedAt` selects it again, and any fetch of it clears the mark. A new
   `updatedAt` seen while the thread is in the retry set restarts its 7 days.
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
  metadata from `thread/list`, turn records from `thread/turns/list`, item
  records exactly as returned, plan usage rows from `query_v2`, and token
  usage and turn-completion records from the live token monitor. Each record
  carries a small envelope naming its kind and fetch time.
- Compression is zstd only, using `github.com/klauspost/compress/zstd` (already
  a direct dependency; the Go standard library has no zstd encoder). No other
  codec, and no option to choose one.
- Every frame carries a zstd content checksum. There is no header frame.
- The fetcher stores the verified end offset of the last complete, checksummed
  frame with the thread's sync state. Before appending, the writer checks only
  the bytes after that offset:
    - complete, checksummed frames there are kept. They come from an append that
      finished before a crash prevented the state save, and the provider may
      already have parsed them. The writer moves the stored offset past them. It
      also rebuilds the per-thread digests (including the last plan usage
      record's digest and kind) from the records in those frames, so the next
      fetch's dedup sees them. Records that repeat anyway are harmless, because
      the newest record for an item wins;
    - an incomplete frame at the very end is a torn append. The writer truncates
      it and never builds on it.
- The writer scans the whole file only when the stored offset is missing or lies
  past the end of the file.
- After appending, the writer fsyncs the file, and the directory when the file
  is new, before the new offset is saved. On Windows, which cannot sync a
  directory, the directory sync is skipped, as raw capture does
  (`internal/rawcapture/files.go`).
- Readers decode only complete frames and never keep records from a torn frame.
- `internal/parser/deepseek_harness_format.go` cannot be reused as is: it
  requires a header frame and, as written, decodes the whole file and keeps
  lines from a torn last frame. Share only its frame inspection and decoder
  memory limits, and apply the rules above.
- A re-fetched item is appended only when it differs from its newest stored
  record. When the same record appears twice (after a crash before the state
  save, or a lost state file), the newest record for a `(turnId, item.id)`
  wins.
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
- Only threads whose mirror file changed are parsed. Parse work per pass is
  proportional to the size of the threads that changed in that pass, never to
  the size of the archive. Discovery still lists every mirror file to compare
  fingerprints, as for every file-backed agent (see Anti-goals).
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
- **Times:** `createdAt` and `updatedAt` from listing records. A mirror file
  that so far holds only the monitor's first metadata record has no
  `updatedAt`; its end time is the newest item `completedAtMs` or token record
  time until a listing record arrives. Per-item times come from `startedAtMs`
  and `completedAtMs`.
- **Model and reasoning effort:** sessions have no model field, so the thread's
  model and reasoning effort go on each assistant and thinking message, as
  other providers record them.
- **Origin label:** dot for `codex_work_cca` or `threadSource: aeon_child`,
  Codex web for `codex_work_web`, Codex cloud for `codex_cloud`, otherwise the
  raw originator. It goes in the existing `Entrypoint` field; no new column.
  `SessionKind` stays empty, as for interactive sessions, because its other
  values drive automated-session classification (`internal/db/automated.go`).
  Dot tasks therefore show as normal sessions, not as automated workers.
- **Working directory and project:** from the first `environments[].cwd` of the
  newest listing metadata record that has a non-empty `environments` (the
  monitor's first record is not used, because a resume response abbreviates
  the home directory as `~`) (the field is null once a thread is not loaded),
  falling back to `cwd` only when no record ever had one. `environments` is
  marked experimental in the open-source protocol, and the fetcher does not
  opt in to the experimental API; the second probe saw the field with
  AgentsView's handshake, and the fetcher falls back to `cwd` if it ever
  disappears.
- **Relationships:** a thread's **resolved parent** is its `parentThreadId`;
  when that is null, the `parent_thread_id` its `source` carries for a spawned
  sub-agent (`SubAgentSource.thread_spawn` in the Codex protocol; in the v2
  JSON the path is `source.subAgent.thread_spawn.parent_thread_id`, camelCase
  outside and snake_case inside); when both are absent, the `source_thread_id`
  of a `<codex_delegation>` tag at the start of the thread's first
  `userMessage` (a delegated dot task; see Background). Later
  `functionCallOutput` items that carry the same tag do not change it. The
  fetcher reads the tag from the first `userMessage` it fetches and keeps the
  result in the per-thread state; the provider reads it from the mirror file.
  A delegated dot task is a sub-agent child for linking and plan usage grouping.
  Its parent, the dot's own conversation, is not listable, so the child waits
  for that parent under the waiting-file rule (see Fetcher state) and stays
  its own usage root until the parent is mirrored. The resolved parent becomes
  a sub-agent relationship (`RelSubagent`) and `forkedFromId` a fork
  relationship (`RelFork`), each pointing at `codex-cloud:<threadId>`. A
  session holds one parent (`ParentSessionID` in `internal/parser/types.go`),
  so when a thread has both, the resolved parent wins, because usage roots
  follow it.

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
- **Join rule:** the monitor joins a thread only when its newest turn is
  `inProgress` and its listing `updatedAt` is within the last 5 minutes. A
  thread that finished long ago can still show an `inProgress` turn (see the
  save lag under While joined), and resuming it would be a cold resume: it
  loads the thread on OpenAI's side, starts the dot's MCP servers, and might
  continue an active goal (see Risks). The recency rule keeps the monitor away
  from such threads. It compares OpenAI's `updatedAt` with the local clock, so
  the window is wide enough that a clock a few minutes off does not skip
  running threads. A clock more than about 5 minutes fast still skips them
  all, and a slow clock admits threads that stopped longer ago, each limited
  to one cold resume.
- The listing's `status` can be stale, so discovery ignores it. Each run lists
  the non-archived default listing by `updated_at`, newest first, with no
  `sourceKinds`, as sync does. It checks the newest turn (`thread/turns/list`,
  limit 1, `itemsView: notLoaded`) of every thread whose `updatedAt` is within
  the last 5 minutes. It skips, without a turn check, threads the monitor has
  already joined and sub-agent threads (see Joining). A thread is running when
  that turn's status is `inProgress`.
- Work per run is bounded by recent activity, not by archive size. Discovery
  reads further pages only while the last page still holds a thread updated in
  the last 5 minutes. It reads no other listing and none of the sync job's
  working sets.
- Accepted limit: a turn that ends before the monitor joins it gets no live
  tokens; its thread has plan usage only, unless a later join's running total
  covers it. If a running thread's `updatedAt` does not change while its turn
  runs, a turn that the monitor does not join within 5 minutes of the last
  change (daemon down, join cap full) is not captured live either.

**Joining.**

- Each joined thread gets its own websocket connection, because the durable host
  does not support `thread/unsubscribe` and closing the connection is the only
  way to leave. At most 10 threads per account are joined at once; others wait
  for a free slot.
- The monitor does not join a sub-agent thread until the user has run a probe
  confirming that joining a running sub-agent thread is a warm join with no
  side effects (see Open questions). For joining, a thread counts as a
  sub-agent thread when its listing record has a `parentThreadId` or a
  `thread_spawn` parent in its `source` (the first two resolved-parent rules
  under Mapping), when its `threadSource` is `subagent`, or when its `source`
  is `SessionSource::SubAgent` (`v2/thread_data.rs` in the Codex protocol).
  Such threads are left unjoined; their tokens are not captured until then. A
  delegated dot task (`threadSource: "aeon_child"`, parent named only by its
  `<codex_delegation>` tag) does not count: it is in the default listing, not
  the sub-agent listings, and is joined under the same join rule as any other
  thread.
- Immediately before joining, the monitor checks the newest turn again and joins
  only if it is still `inProgress` and the join rule still holds. It then
  sends `thread/resume` with only `threadId` and `excludeTurns: true`.
- A JSON-RPC error reply to `thread/resume` (for example
  `environment_terminated`) follows JSON-RPC errors: most close the connection
  and are retried on the next run if the thread is still running.
- For a successful response, its `resumeKind`, a field only the hosted server
  returns, decides what happens; a missing `resumeKind` counts as "anything
  else":
    - `warm`: the thread is running and the monitor stays.
    - `cold`: the turn ended between the check and the join, or the turn listing
      shows a turn as `inProgress` that the server no longer runs. The join rule
      narrows this to threads updated in the last 5 minutes. The monitor logs it
      and closes the connection at once. It records the newest turn's ID in the
      per-thread state and does not join that thread again until its turn
      listing shows a newer turn ID, so a turn stuck in `inProgress` causes at
      most one cold resume. The resume response's `updatedAt` is the time of the
      resume, so the monitor never stores it; only the fetcher's listing sets a
      thread's stored `updatedAt`.
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
- It never answers a server-initiated request: approval prompts and questions
  stay for the user's own apps to answer. In open-source Codex a server
  request goes to every connection joined to the thread and the first reply
  wins (unverified on the hosted server; see Open questions), so any answer
  from AgentsView, even an error, could beat the user's app. A joined
  connection can stay open for hours, so on a token refresh request
  (`account/chatgptAuthTokens/refresh`) the monitor flushes its buffered
  records, closes the connection without answering, and starts the
  refresh-requested wait for that Codex home (see Failure timing); a later run
  joins again through another home with the same login key, or after the wait
  when there is none, if the thread is still running.
- Messages on one websocket arrive in order, and the server sends a turn's token
  updates during the turn, before `turn/completed`. So when `turn/completed`
  arrives, every update for that turn has already arrived. The monitor then
  writes a turn-completion record, flushes its buffer, records that turn's ID
  in the per-thread state, and leaves. As after a cold resume, it does not
  join the thread again until its turn listing shows a newer turn ID, because
  the save lag can keep the finished turn listed as `inProgress` for minutes.
  It also leaves after 6 hours, or when live capture turns off.
- If the connection drops, the next run joins the thread again if it is still
  running.

**What gets stored.**

- When the monitor joins a thread that has no mirror file yet (live discovery
  can find a thread before sync mirrors it), it first writes the `thread` from
  a `warm` `thread/resume` response as a metadata record, without `updatedAt`
  and `recencyAt`, which a resume response sets to the time of the resume (see
  Background); those fields come only from listing records, so the file always
  starts with thread metadata. The provider skips a mirror file with no
  metadata record and logs it.
- Token counters arrive as JSON numbers that can carry a fractional part such as
  `.0`. The monitor decodes them as numbers and accepts only integral values;
  a non-integral value is a protocol change that stops live capture for the
  account. Synthetic fixtures use the `.0` form.
- Each token update becomes a token usage record with the time it was observed,
  a join ID (a random ID per joined connection, also written on that
  connection's turn-completion records), `turnId`, `total`, `last`,
  `modelContextWindow`, and the current model: the model from the
  `thread/resume` response, updated by `model/rerouted`. The monitor buffers
  records in memory and appends them to the thread's mirror file as one frame
  at most once a minute, when the turn completes, and when it leaves the
  thread. A crash loses at most the buffered records; because totals are
  cumulative, the next update recovers the counts. Writes from the monitor and
  the fetcher to one mirror file are serialized.
- The provider turns these records into usage events (`ParsedUsageEvent`, stored
  in `usage_events`, declared through the `AggregateUsageEvents` capability),
  the same kind other agents produce, so they reach replicas, the DuckDB
  mirror, and cost views without new tables.
- Price bands: GPT-6 models cost more above 272,000 input tokens per request
  (`docs/internal/session-format-sources.md`), and bands apply only to
  request-scoped events (`CostForTokensScoped` in
  `internal/export/pricing.go`). When an update's `total` increase equals its
  `last` exactly, the increase is one model call: the provider emits it as a
  request-scoped event (a new source the builder adds to
  `SourceIsRequestScoped` in `internal/usagefacts`), so the band applies. Any
  other increase spans several calls (the first record of a join, or updates
  missed while not joined) and is emitted as a thread-level event with
  `MessageOrdinal` nil, priced at base rates, because a band chosen from a
  multi-call increase would be wrong. Session token fields count each increase
  once, whichever kind of event carries it. A multi-call increase can
  understate the cost of long-context calls inside it; that is a documented
  limit.
- Each thread-level event has `Source: "session"`, the value other providers use
  for thread-level totals; a request-scoped event holds one call, well under
  the 2,000,000 clamp below. The engine skips `session` events when it derives
  a session's own token fields (`internal/ingest/prepare.go`), so, as Cline
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
  that finished before it was observed, within one baseline. After a new
  baseline, the first record's total starts from the fresh counter, so it does
  not cover turns that finished after the last record of the previous
  baseline.
- A reset is detected only as a decrease. If the counters restarted while no
  join was open and grew past every earlier value before the next join, the
  provider adds only the difference and counts the turns in between as
  covered, which undercounts. No probe has shown a reset: a join to a
  long-running thread reported a total in the millions, which suggests the
  hosted server keeps totals for the thread's life. The user-facing docs state
  this limit, and Open questions asks for the evidence that settles it.

**How it shows up.**

- Sessions with token usage count in token and cost views like any other agent.
  Cost comes from AgentsView's price list; models missing from it show tokens
  without cost.
- The provider computes each session's capture coverage from the mirror's turn
  and token usage records: the time of the last capture and the number of
  finished turns not captured. A finished turn is captured when the mirror
  holds a token record for a later turn of the same thread in the same
  baseline (totals are cumulative and cover every earlier turn of that
  baseline), or a token record for that turn followed by that turn's
  turn-completion record with the same join ID. A join that drops and a later
  join that sees only `turn/completed` carry different join IDs, so the gap
  shows as not captured. A turn-completion record alone does not count: with
  `excludeTurns: true` the server sends no token update on join for a thread
  with `historyMode: "legacy"`, as every probed thread had
  (`thread_processor.rs`, the cheap resume path), so a join that lands after a
  turn's last model call sees `turn/completed` with no counts. No clock
  comparison is involved. Otherwise, for example after the 6-hour leave or a
  dropped connection with no later capture, it is not captured. A token record
  that arrives right after the resume response (open-source Codex replays one
  for paginated-history threads) is recorded like any other, even when its
  `turnId` names an earlier, finished turn. It stores both in the new
  per-session table (see Plan usage), so every backend can query them.
- Session details say "Tokens captured live, as of T". When finished turns are
  not captured, they add "N turns not captured", followed by "plan usage
  covers them" only when the session has a usable plan usage row.
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
unless a known ancestor exists through its parent chain. A thread's parent here
is its resolved parent (see Mapping). Each root's group contains every known
descendant at any depth, with duplicate IDs removed. A thread is sent in exactly
one group.

**Fetching.**

- Plan usage is a separate step that runs after the thread step in each pass.
  Its failures never change thread sync status, its backoff, or the listing
  watermarks.
- Each root carries a persisted **usage-pending** mark. The mark is set when the
  root or any of its descendants is fetched, when the root has no usage row,
  when a usage request for it fails, when the response leaves it out, and
  while its row is `partial` or `unavailable`. Only a usable row, or settling
  (below), clears it. A usage attempt's outcome sets the root's next-attempt
  time. A thread fetch that sets the mark on a root already pending keeps that
  time. A root that becomes pending with no usable row is due at once; a root
  that re-enters pending while it has a usable row (a running thread is
  fetched every pass) has its own re-entry wait: 5 minutes after its last
  attempt at first, doubling with each usable answer while it keeps
  re-entering, up to 6 hours. The re-entry wait resets to 5 minutes when no
  thread in the root's group has a turn in progress, or after 24 hours with no
  change to the root or a descendant. So a running root is not re-queried every
  pass. A root that runs for hours can reach the 6-hour wait, so its final
  usage can lag up to 6 hours after the thread finishes.
- Each pending root has its own next-attempt time. A failed or unresolved
  attempt doubles the wait, from 5 minutes up to 6 hours.
- **Settling.** Only answered requests (HTTP 200) settle a root. A root settles
  when 7 days have passed since the last change to it or a descendant, and a
  request answered after that point still finds its row `partial` or
  `unavailable`, or leaves it out. A root that is always left out settles as
  "Not reported by OpenAI". A root that already has a usable row and is then
  left out settles by appending a copy of that row marked final, not with a
  "settled with no row" record. Automatic retries stop, the last values stay,
  and the row is marked final. A later change to the root or a descendant
  starts the cycle again.
- **Could not check.** A pending root whose request fails after the 7 days since
  its last change stops retrying with a "could not check" outcome record,
  because only an answer after that point can settle it. An answer earlier in
  the 7 days does not prevent this. Its last row, if any, stays and is not
  marked final, and session details show "AgentsView could not check plan
  usage". A later change to the root or a descendant starts the cycle again.
- Roots with a usable row stay in a refresh set in `account.json` until 7 days
  after their last change, each with a next attempt at the later of one day
  after its row's `data_as_of` and one day after its last attempt. Rows are
  refreshed from that set, so no pass scans every root.
- Each pass sends at most 5 requests, shared by pending roots and the refresh
  set: pending roots first, oldest first, then refresh roots by next attempt.
  Requests follow the desktop app's limits: at most 100 groups and 1,000
  thread IDs each, counting each root's own ID. A root whose group alone has
  more than 1,000 IDs, so 1,000 or more descendants, is never sent; it settles
  at once with a "too many sub-agent threads for OpenAI's usage report"
  outcome record, and session details show that message. Roots left over wait
  for the next pass, so a large backlog never makes one pass unbounded.
- A group sends the root's `thread_id` and `created_at`, and its descendants in
  `descendant_thread_ids`.
- HTTP 429 from `query_v2` pauses plan usage for that account for the
  `Retry-After` value, or 5 minutes doubling to one hour without it; no usage
  request is sent until the pause ends.
- Plan usage has its own pauses and runs even while the thread host is waiting.
  It reads the token in the same home order as the thread fetch and skips
  homes in the thread host's signed-out or refresh-requested wait. A 401 from
  `query_v2` on one home moves to the next home on the same key; it does not
  start the thread host's wait. Only when every home on the key has been
  tried, or none is usable, does plan usage pause for that account, until the
  `auth.json` of any home on that login key changes or one hour passes. A
  `query_v2` 401 on a home is remembered in memory until that home's
  `auth.json` changes or one hour passes, so later passes skip it until then,
  and the plan usage line shows the signed-out message; thread sync keeps its
  own status. HTTP 403 or 404 means OpenAI does not offer this usage to the
  account: AgentsView records "Usage isn't available for this account", sends
  no usage requests for that account for 24 hours, and shows no error. Other
  failures use the per-root backoff above.
- The fetcher resolves roots, settling, and the 24-hour pause from its state,
  and records each outcome in the mirror files, so a full resync or a hub can
  rebuild what session details show:
    - a response row (with `data_as_of` and whether it is final) is appended to
      the root's file only when it differs from the last plan usage record of
      any kind in that file, so the same row returning after an outcome record
      is stored again;
    - a "left out" record goes to each root in that pass's request that the
      response omitted, and a "settled with no row" record when such a root
      settles;
    - an "account unavailable" record goes to each root in the request that got
      the 403 or 404, so the work stays bounded by the request;
    - a "too many sub-agent threads" record goes to a root whose group is too
      large to send, and a "could not check" record to a root whose request
      fails after the 7-day point;
    - a thread found to be a descendant gets a "counted under `<root>`" record in
      its own file, even while usage requests are paused, since it needs no
      request. The provider stays per-file. It keeps the newest response row and
      the newest outcome record of each file separately: session details show
      the row when one exists, and show an outcome message only when no row
      exists or the outcome is newer than the row and says the row is stale
      (left out, account unavailable, too many sub-agent threads, or could not
      check, after the row), in which case both appear. A "counted under" record
      newer than the row overrides it: the plan usage values are cleared and
      only "Counted under thread" shows.

**Storage.** Usage rows are running totals for the thread, not increments, and a
percentage of a weekly limit does not add up across weeks. So AgentsView keeps
the latest row per root, replacing the previous one, with its `data_as_of`,
`data_status`, `usage_source`, the thread-level percentages and credits, the
per-group values, whether the row is final, and the newest outcome record's kind
and time (left out, settled with no row, account unavailable, too many sub-agent
threads, could not check, or counted under), so every backend can show the same
session details. The provider resolves the order at parse time from the records'
positions in the mirror file and stores an outcome only when it is newer than
the newest row; backends never compare times to decide it. It does not create
token usage events. Descendants have no plan usage of their own; when a thread's
newest plan usage record says it is counted under a root, the provider clears
that session's plan usage fields, keeps its coverage fields, and records the
root as its parser session ID (`codex-cloud:<rootId>`), and the fetcher drops it
from the usage retries. The root is a session reference: on a hosted hub, where
sessions are stored under raw projection IDs
(`internal/postgres/raw_projection.go`), it is mapped to the public ID like
other session references (`internal/postgres/hosted_session_ids.go`). That
mapper resolves a reference only through a link edge, and `writeRawLinks`
(`internal/postgres/raw_projection_links.go`) writes only the direct parent and
tool targets, so for a grandchild the root would be cleared. The builder adds a
usage-root link kind there, so the root reference resolves at any depth. The
same per-session table holds the live-capture coverage described under Live
token capture, so descendants and sessions without plan usage still have a row.
The builder chooses the table and follows `docs/agents/storage.md`, including
the full-resync copy of orphaned and trashed sessions, which must copy their
plan usage rows too. In PostgreSQL the table also needs the hosted-tenant schema
and privilege entries that `usage_events` has, and a probe in
`CheckSchemaCompat` (`internal/postgres/schema.go`) so a reader against an older
schema fails at startup. Every backend's session usage reader (`GetSessionUsage`
in the `db.Store` interface, and the hosted reports wrapper) returns the plan
usage and coverage row. Like usage events, a plan usage write bumps the
session's `local_modified_at` and is part of every backend's push fingerprint,
so replicas pick up changes.

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
checks that SQLite, each replica, and the mirror return the same plan usage, the
same outcome kind and time, and the same totals.

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
    - "Too many sub-agent threads for OpenAI's usage report": see Fetching.
    - "AgentsView could not check plan usage": see Could not check.
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
  future moves. A non-loopback override must name a host that is
  `chatgpt.com`, `openai.com`, or a subdomain of either, so a misconfigured
  override can never send the real token to another host. They must use `wss`
  and `https`; `ws` and `http` are accepted only for loopback addresses, so a
  typo can never send the token in clear text. Neither client follows
  redirects: `coder/websocket` and Go's `http.Client` follow them by default,
  a redirect would carry the subprotocol (and with it the token) or the
  `Authorization` header to a URL the allowlist never checked, so both set
  `CheckRedirect` to refuse, and any 3xx reply is "Handshake refused or reply
  not understood". `live_tokens = false` turns off live token capture, and
  with it the only request that touches a thread on OpenAI's side.
- The shared `[agents.<id>]` reader (`applyAgentDirectories`) parses only `dirs`
  and `homes` and silently ignores other keys. It must skip the
  `[agents.codex-cloud]` table entirely, because it would otherwise treat
  `dirs` as a directory override and fail the whole load on `homes`. Add
  parsing and validation for these four keys; `dirs` and `homes` are unknown
  keys for this source. Today an error from that reader fails the whole config
  load. Errors in `[agents.codex-cloud]` must not take that path: an unknown
  key or a bad value is a config error for this source only: the error appears
  at startup and in the status area (see Status and errors), fetching and live
  capture stay off until it is fixed (existing mirror files still parse, as
  with `enabled = false`), and the rest of the config and every other agent
  still load. A different case: if `config.toml` as a whole fails to parse
  when a job re-reads it, the job keeps the last good settings and shows
  "`<config path>` could not be read: `<reason>`. Codex cloud import keeps its
  last good settings.", followed by the reason those settings keep import off,
  if they do.
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
- **Off: this daemon does not sync sources.** The message names the two causes,
  a command run with `--no-sync` or an archive-only archive, and says that a
  daemon started with `agentsview serve` on a normal archive imports Codex
  cloud chats. The live tokens line says "Live tokens: off, because this
  daemon does not sync sources."
- **Waiting for first sync:** enabled, no pass has finished.
- **Importing, found N chats so far:** during a first backfill, until all four
  listings have finished a first pass. After that: **Importing N of M chats.**
- **Up to date as of T:** the last pass succeeded and no thread is waiting to be
  retried.
- **N chats could not be imported:** the last pass succeeded but threads are in
  the retry set. Shows the count, the oldest failure time, and the latest
  failure's message from the table below, followed by "AgentsView retries each
  one for 7 days; a change to the chat in OpenAI restarts the 7 days. The
  daemon log lists each chat. Sessions already imported are not affected."
- **Error:** the whole pass failed. Shows the message below and when the last
  successful pass finished.

Live token capture has its own line: "Live tokens: listening in on N running
chats.", "Live tokens: idle.", "Live tokens: off, because ..." naming the daemon
option or setting that keeps it off, or "Live tokens: stopped." with the
protocol-change message and "Then restart AgentsView to listen in again." When a
save of token records failed, the line adds "Token counts for N chats are not
saved yet. AgentsView keeps retrying; restarting it before a save works loses
them. The daemon log says why saving fails." The note stays when live tokens are
off, and pending records are retried on every discovery run whatever the gates
say. Cold resumes are logged per thread, not counted in the line: the count only
grew until restart, so a normal event looked like a growing fault. Each
session's capture coverage still shows the turns it missed.

Plan usage has its own line under the home's state, so a usage problem never
hides or replaces thread status. It shows "Plan usage: not checked yet", "Plan
usage as of T", "Usage isn't available for this account", or "Plan usage: "
followed by a usage error from the table below. After a restart, T is the stored
`data_as_of` of the newest answered report, shown with the year when it is not
the current year. A home skipped for another login problem shows that problem's
message instead of the signed-out one.

| Cause                                               | Message                                                                                                                                                                                                                                            |
| --------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| No token, or HTTP 401 from the thread host          | Signed out of ChatGPT in this Codex home. Run `codex login`, then AgentsView picks up the new login on its next sync.                                                                                                                              |
| HTTP 401 in `auto` mode                             | OpenAI did not accept the ChatGPT login in this Codex home's auth.json. With `cli_auth_credentials_store = "auto"`, Codex may keep the current login in the system keyring, which AgentsView does not read. (file fix)                             |
| A reply larger than the read limit                  | A reply from OpenAI was larger than AgentsView accepts.                                                                                                                                                                                            |
| Token refresh request from the thread host          | OpenAI asked for a fresh ChatGPT login in this Codex home. Open Codex or run `codex login`; AgentsView retries when the login changes, or after an hour.                                                                                           |
| HTTP 403 from the thread host                       | OpenAI refused this request. AgentsView retries automatically. If this continues, check that this ChatGPT account can use Codex.                                                                                                                   |
| Network failure                                     | Can't reach OpenAI. AgentsView retries automatically.                                                                                                                                                                                              |
| HTTP 429                                            | OpenAI is limiting requests from this account. AgentsView retries at T.                                                                                                                                                                            |
| HTTP 5xx                                            | OpenAI's service had an error. AgentsView retries automatically.                                                                                                                                                                                   |
| JSON-RPC error reply for one thread                 | OpenAI returned an error for the chat. It may have been deleted.                                                                                                                                                                                   |
| Handshake refused or reply not understood           | OpenAI's service answered in a way this version of AgentsView can't read. If config.toml sets endpoint under [agents.codex-cloud], check that address; otherwise update AgentsView. Sessions already imported are safe.                            |
| Keyring-backed home, no token in `auth.json`        | This Codex home keeps its ChatGPT login in the system keyring, which AgentsView does not read. (file fix)                                                                                                                                          |
| `ephemeral` credential store                        | This Codex home does not save its ChatGPT login, so AgentsView cannot use it. (file fix)                                                                                                                                                           |
| Codex home login settings or `auth.json` unreadable | One message per cause (a `config.toml` that does not parse, an unknown `cli_auth_credentials_store`, an `auth.json` that cannot be read or is not valid JSON), never quoting file content.                                                         |
| Local account state unreadable or unwritable        | AgentsView could not read or save its import state, so imports from this account are paused. The daemon log names the file and the error. Check that AgentsView can read and write its data directory; imports resume on the next sync after that. |
| Bad value in `[agents.codex-cloud]`                 | `<config path>` has an invalid value for `<key>` under `[agents.codex-cloud]`: `<reason>`. Codex cloud import is off until then.                                                                                                                   |
| HTTP 401 from plan usage                            | Plan usage: signed out of ChatGPT in this Codex home. Run `codex login`.                                                                                                                                                                           |
| Unknown key in `[agents.codex-cloud]`               | `<config path>` has an unknown setting `<key>` under `[agents.codex-cloud]`. Remove it or correct its name. Codex cloud import is off until then.                                                                                                  |

"(file fix)" stands for: "To let AgentsView use it, set
`cli_auth_credentials_store = "file"` in this home's config.toml and run
`codex login`. That stores the ChatGPT login in a plain file that any program
running as you can read." For a Codex home other than the first, every
`codex login` in a message names that home: `CODEX_HOME=<home> codex login`,
shell-quoted when needed, or on Windows an instruction to set `CODEX_HOME` to
the home and run `codex login`.

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
live capture, on one machine only. The builder checks how ClickHouse and a
hosted hub handle the same session from two machines and documents it; hub raw
projections are keyed per device (`internal/postgres/raw_projection_schema.go`),
so two machines uploading one thread may produce two projected sessions and
double-count its tokens.

## Testing

- A fake durable host on a local websocket server and a fake `query_v2` HTTPS
  server, built from synthetic fixtures. The fixtures contain no private
  names, paths, IDs, or tokens.

- Handshake: the token is sent only as the subprotocol, and handshake errors
  never contain it. The client subprotocol is `codex-client.agentsview`, never
  `codex-client.desktop`. The server selects `codex-app-server` (see Open
  questions); any other selected subprotocol is a protocol change. There is no
  retry under the desktop name.

- Allowlist: a non-allowlisted method or URL fails without being sent. A fake
  thread host and a fake usage endpoint that answer 302 get no second request,
  and the pass reports the handshake error.

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

- Listings: `thread/list` for the default listings is sent with no
  `sourceKinds`, and for the sub-agent listings with only the sub-agent kinds;
  a thread in both is fetched once.

- Check-again: a first import of a thread updated long before the fetch is
  fetched once and not again; a fetch within 2 minutes of the thread's
  `updatedAt` sets the flag, so a local clock up to 2 minutes ahead of the
  server still catches a second update in the same second.

- Token counters: `4603049.0` decodes as 4603049; `1.5` stops live capture for
  the account with a protocol-change error.

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

- Error codes: `-32601` with an `unsupported app-server method` message, and
  `-32600` with an `Invalid request:` message or an `experimentalApi` message,
  fail the pass as a protocol change and wait as for a 403; `-32602` on an
  anchored `thread/items/list` re-reads the turn without the anchor in the
  same pass, and only a second failure puts the thread in the retry set;
  overloaded and shutting-down replies end the pass with the account wait and
  no retry-set entry; other error replies to `thread/list` fail the pass with
  the 5xx wait; a `-32600` "no rollout found" reply to a fetcher thread
  request puts that thread in the retry set; an `environment_terminated` reply
  to the monitor's `thread/resume` closes the connection and is retried on the
  next run.

- Failures: a thread that keeps failing and stays at the top of the listing is
  not fetched again before its next-attempt time, backs off to the one-hour
  cap, does not hold back the listing watermark, and is fetched once it
  recovers.

- Deletion (blocked on the deleted-thread probe in Open questions): a thread the
  server reports as gone leaves the retry set, is not fetched again, keeps its
  stored session, and does not keep the "could not be imported" state up.

- Unreadable threads: a thread that fails for 7 days with an unchanged listing
  `updatedAt` leaves the retry set and the status count, keeps its stored
  session, and is not fetched again; a new `updatedAt` during those 7 days
  restarts the count, and a new `updatedAt` afterward selects it again. A
  thread with one item over the read limit at `limit: 1` ends the same way.

- Retry listing record: a thread in backoff whose listing record changes gets
  the new record in its retry entry, and the retry appends that record.

- Resume point: parallel tool calls that finish out of order all end with their
  final output. An item without a terminal status in a finished turn, or in a
  turn followed by a later turn, does not pin the resume point. A turn that
  ends between the turn-status call and the item call does not mark its items
  finished.

- Unchanged re-fetch: re-fetching a thread whose records did not change leaves
  its mirror file byte-identical and triggers no parse; a long turn with an
  early unfinished item does not grow the file on each pass.

- Durability: a crash between the mirror append and the state save repeats the
  fetch and leaves no gap and no duplicate session item.

- Mirror: an interrupted append followed by a restart and a successful append
  leaves every complete frame readable; torn frames contribute no records. A
  complete frame written after the saved offset, and already parsed by the
  provider, survives recovery, and the next parse yields the same session with
  no duplicate items. A normal append checks only the bytes after the stored
  offset; a missing or too-large offset triggers a full scan. On Windows, the
  first append to a new mirror file succeeds. A mirror file that is lost or
  shorter than the stored offset makes the next fetch read and write the whole
  thread again.

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
    - a root whose request fails after the 7-day point does not settle as final,
      even when an earlier request was answered; it gets a "could not check"
      outcome, keeps any `partial` row unfinal, stops retrying, and a later
      thread change reopens it;
    - a large backlog of pending roots sends at most 5 requests per pass;
    - nested sub-agent threads (child and grandchild) are all sent with their
      root, never as their own group, and a change in a grandchild refreshes the
      root;
    - a thread that turns out to be a descendant loses its plan usage values,
      keeps its coverage row, and shows "Counted under thread";
    - a root with a usable row is refreshed at the later of one day after its
      row's `data_as_of` and one day after its last attempt, including when
      `data_as_of` is earlier than the last attempt, while it changed in the
      past week, without scanning other roots;
    - a 403 from `query_v2` shows "Usage isn't available for this account", stops
      usage requests for 24 hours, and leaves thread sync status unchanged.

- Live token capture:

    - a thread listed as `idle` whose newest turn is `inProgress` and whose
      `updatedAt` is within the last 5 minutes is found and joined;
    - a thread whose newest turn is `inProgress` but whose `updatedAt` is 6
      minutes old gets no turn check and no `thread/resume`, including when the
      listing shows it as `active`;
    - a delegated dot task (`threadSource: "aeon_child"`, parent only in its
      `<codex_delegation>` tag) that is running and recent is joined;
    - `warm` keeps the connection until `turn/completed`, writes the
      turn-completion record, flushes, then leaves;
    - a thread already joined is not joined again by a later discovery run;
    - a sub-agent thread marked only by `threadSource` or `source` is not joined;
    - a `model/rerouted` notification changes the model on later token records;
    - turns before the first token record count as captured; a turn after the last
      token record counts as not captured, including one whose `turn/completed`
      was seen with no token update; a turn the monitor left after 6 hours, with
      token records but no turn-completion record and no later turn captured,
      counts as not captured;
    - after `turn/completed`, a discovery run that still sees that turn as
      `inProgress` does not join the thread;
    - usage events have a nil `MessageOrdinal`;
    - `cold` is logged and the connection closes at once; a turn stuck in
      `inProgress` causes one cold resume, and the thread is joined again only
      after a newer turn ID appears;
    - an unknown `resumeKind` stops capture for the account with an error;
    - server-initiated requests while joined are never answered; a token refresh
      request flushes the buffered records, closes the connection unanswered,
      and starts the home's refresh-requested wait, so no join follows until
      `auth.json` changes or one hour passes;
    - token records carry the model from the `thread/resume` response;
    - after a baseline reset, the session's output total still includes the
      increases from before and after the reset;
    - a thread still running after 6 hours is left and joined again on a later
      run;
    - a running sub-agent thread is not joined and gets no turn check;
    - discovery reads a second listing page while the first page holds only
      threads updated in the last 5 minutes, and stops after the first page that
      holds none;
    - `-32601` with an `unsupported app-server method` message on a discovery
      `thread/turns/list` stops capture for the account and does not set the
      sync job's wait;
    - an ordinary error reply to `thread/resume` is retried on the next run and
      does not stop capture; `-32601` with an `unsupported app-server method`
      message, or `-32600` with an `Invalid request:` or `experimentalApi`
      message, stops capture for the account without setting the sync job's wait
      with the protocol-change error; an overloaded reply sets the shared
      account wait;
    - a `-32601` on `thread/items/list` with any other message, such as "not
      supported yet", is an error for that thread only;
    - a thread joined before sync mirrored it gets a mirror file that starts with
      its metadata; a mirror file with no metadata is skipped;
    - after a decrease, the first record of the new baseline adds its full total,
      and the sum across baselines is exact;
    - a turn whose join ended (6-hour leave or dropped connection) before it
      completed, with no later capture, counts as not captured;
    - a 429 seen by sync stops live discovery requests for that account, and a 5xx
      or overloaded reply seen by discovery or the monitor delays sync;
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
  ID; state files are never captured. After a torn frame is truncated, the
  next capture replaces the file instead of extending it.

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

- Server requests: the fetcher and the monitor answer no server-initiated
  request; an approval request stays unanswered, and a token refresh request
  closes the connection unanswered. In the fetcher it ends the pass early: the
  watermarks stay, no thread enters the retry set, plan usage does not run,
  the status shows the fresh-login message, and the next pass uses another
  home with the same login key or, when there is none, waits until `auth.json`
  changes or one hour passes, then starts over.

- Turn view: every `thread/turns/list` call (fetch, discovery, and the check
  just before joining) carries `itemsView`.

- Later turns: a thread whose new turn started after the resume point gets that
  turn's items fetched in full.

- Sub-agent threads: a sub-agent thread in the default listing is mirrored and
  linked to its parent.

- First import: a thread whose last turn's `completedAt` is more than 12 minutes
  old is imported in one fetch, with no repeat fetch for the save-lag margin;
  a thread whose last turn ended 11 minutes ago is fetched again on each pass
  until that turn's `completedAt` is 12 minutes old (the next pass, at the
  usual interval), then not again.

- Archived during the save lag: a thread archived seconds after its turn ends,
  with a command item whose final output is not yet saved, keeps its resume
  point before that item until 10 minutes after the fetcher first saw it
  archived, and the later fetch stores the final output.

- Failed first import: a thread whose first fetch fails while the listing
  watermark advances is retried from the retry set, and the retry appends its
  listing record, so the session is parsed with its metadata.

- Listing order: a page whose `updatedAt` increases from one row to the next
  fails the pass as a protocol change instead of stopping early; no listing
  call carries `modelProviders`.

- Login key: two homes signed in as the same user, one whose ID token carries
  `chatgpt_account_user_id` and one whose older token does not, get one key
  and sync once.

- Price bands: an update whose `total` increase equals `last` with more than
  272,000 input tokens is priced at the band rate, including when it is the
  first record of a join; the first record of a join whose increase differs
  from `last` is priced at base rates; session output totals count each
  increase once.

- Coverage across a reset: turn A captured, turn B finishes while not joined,
  the counters reset, and turn C is captured; B counts as not captured.

- Coverage join IDs: join A records a token update and drops, join B sees only
  `turn/completed`; that turn counts as not captured.

- Endpoint overrides: an `https` override on a host outside `chatgpt.com` and
  `openai.com` is a config error and no request is sent.

- Oversized usage group: a root with 999 descendants (1,000 IDs with the root)
  is sent; a root with 1,000 descendants is never sent and settles with the
  "too many sub-agent threads" outcome, which a root with an earlier usable
  row shows beside that row.

- Hosted usage root: a grandchild's usage root resolves to the root's public ID
  on a hosted hub.

- Token canary: with a unique fake access token and ID token, a run of sync,
  plan usage, a warm join, a refresh request, and 401, 403, and 5xx failures
  leaves neither value in any written file (mirror, state, `account.json`),
  any log line, the status API, or `serve status` output.

- Reply size: a `thread/items/list` page larger than 32 KiB is read; a page over
  the configured read limit is retried once with `limit: 1`, and only a second
  oversized reply puts that thread in the retry set while the pass continues
  with the next thread; an oversized turn listing puts the thread in the retry
  set at once, without a retry.

- Refresh requested: during a home's refresh-requested wait, plan usage and live
  discovery skip that home, and a second home with the same login key is used.

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
  until the margin passes, and its final version is stored; an item that first
  appears in the store after its turn ended, with every earlier item terminal,
  is still fetched, including when a later turn has already started.

- Crash recovery: frames kept past a stale offset rebuild the dedup digests, so
  a plan usage row returning after a recovered outcome record is appended
  again.

- Late parent: a child mirrored before its parent moves under the parent's root
  when the parent arrives, with its own children (grandchildren of the parent)
  counted under the new root, and the root becomes usage-pending; the child's
  old descendant list, refresh entry, and pending mark are gone.

- Single parent: a thread with both `forkedFromId` and `parentThreadId` gets the
  sub-agent relationship.

- Pending mark: a running thread's fetch does not reset its root's usage
  next-attempt time; a running root with a usable row is re-queried after 5,
  10, 20 minutes and so on up to 6 hours, and the wait resets once no thread
  in its group is running, or after 24 quiet hours.

- Stale outcome: a usable row, then a 403 or a left-out response, then the same
  row again, ends with the row stored and shown.

- Outcome records: after a full resync, a left-out root, a settled root with no
  row, and an account-unavailable root show the same session details as
  before.

- Spawn parent: a thread whose `parentThreadId` is null but whose `source`
  carries a `thread_spawn` parent is grouped under that parent's root and gets
  the sub-agent relationship.

- Delegated parent: a thread with no `parentThreadId` and no spawn parent whose
  first `userMessage` starts with a `<codex_delegation>` tag gets that
  `source_thread_id` as its resolved parent and the sub-agent relationship; a
  later `functionCallOutput` with a different tag does not change it; while
  the parent is unlisted, the thread waits in that parent's waiting file and
  is its own usage root.

- Working directory: a stopped thread whose newest metadata has null
  `environments` keeps the working folder from an earlier record.

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
  same plan usage, outcome kind and time, coverage, and totals for the same
  data, through each backend's session usage reader and the hosted reports
  wrapper, and a plan usage change reaches each of them. A PostgreSQL schema
  without `session_plan_usage` fails `CheckSchemaCompat`.

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
  far" until all four listings have finished a first pass. A user who never
  configured the source sees no row.

## Documentation

- Add an evidence entry for this format, the handshake, and `query_v2` to
  `docs/internal/session-format-sources.md`, as `AGENTS.md` requires for new
  providers.
- Document the setting, what it does with the user's ChatGPT login, the
  private-endpoint caveat, what live token capture does on OpenAI's side and
  how to turn it off, why threads AgentsView never joined while running show
  plan usage instead of tokens, and these limits, in the user-facing docs:
    - a thread's first token record is dated when it was observed, even when
      earlier turns ran on an earlier day;
    - two model changes the monitor does not see (a change only
      `thread/settings/updated` reports, and a reroute before the join);
    - running sub-agent threads are not joined until the probe in Open questions
      is done;
    - enable the source, or at least live capture, on one machine only (see
      Several machines);
    - live capture joins a running thread only within 5 minutes of its last
      `updatedAt` change, so a turn that ends before a join, or that runs
      without changing `updatedAt` after the monitor missed that window, gets no
      live tokens;
    - a multi-call increase is priced at base rates, which can understate the cost
      of long-context calls inside it (see Live token capture);
    - a counter restart that the monitor does not see as a decrease undercounts
      the turns in between (see Live token capture);
    - a thread that fails for 7 days without changing stops being retried and is
      named in the daemon log.

## Open questions

1. **Turn listing view.** Answered 2026-10-13: the server accepts
   `itemsView: "notLoaded"` and returns turns with no items; it rejects
   `summary`. The fetcher and the monitor always send `notLoaded`.

1. **Listing order and providers.** Answered 2026-10-13: with
   `sortKey: updated_at`, descending, `updatedAt` never increased from row to
   row, within a page or across pages. The server rejects `modelProviders`
   with `-32602`, so it is never sent; the gap is the known limit under
   Incremental sync.

1. **Client subprotocol.** Answered 2026-10-13: the server accepts
   `codex-client.agentsview` and selects `codex-app-server`.

1. **How the server reports a deleted thread.** The user runs a probe that sends
   `thread/turns/list` and `thread/items/list`, the fetcher's only per-thread
   calls, for a thread they have deleted (for example after resetting a dot)
   and records the exact replies. Until then, no reply counts as a deletion,
   and the 7-day unreadable rule (Incremental sync) stops the retries.

1. **Joining sub-agent threads.** The resume probes covered dot threads only.
   Before the monitor joins sub-agent threads (as defined under Joining), the
   user runs the same warm and cold resume probes on a running and a stopped
   sub-agent thread. Until then those threads are not joined.

1. **Descendant threads.** The desktop app finds a thread's sub-agent threads
   with a local discovery step. No listed thread had a `parentThreadId`, so
   whether `parentThreadId` finds them all is unverified. The user runs a
   probe on a thread that used sub-agents, using the `parentThreadId` and
   `ancestorThreadId` filters of `thread/list` (experimental, so the probe,
   not the fetcher, sends the `experimentalApi` capability), and records
   whether every descendant carries `parentThreadId`. Until then, usage groups
   use the resolved parent (see Mapping) alone.

1. **Sub-agent threads in the listing.** On 2026-10-13 the server accepted the
   sub-agent listings and returned no threads from them. No probed thread was
   a sub-agent thread (delegated dot tasks are in the default listing), so it
   is unverified whether the default `thread/list` includes sub-agent threads.
   The user runs a read-only probe after a thread has used sub-agents: list
   with no `sourceKinds`, then with `sourceKinds` set to the sub-agent kinds,
   and records which listing returns the sub-agent threads. The next time the
   user archives a thread in their own app, a read-only listing before and
   after records whether its `updatedAt` changed; if it does not, the archived
   listing can miss it until the 24-hour stall rule stops its unfinished
   selection. Until then, sync reads both listings (see Incremental sync); if
   the default listing turns out to include them, the sub-agent listing can be
   dropped.

1. **Counter resets.** Whether the hosted token totals ever restart from zero,
   for example after a cold start. The user joins a thread before and after
   its environment ends and restarts, with a read-only probe, and compares the
   totals. If totals restart, the monitor needs a signal that separates
   counter generations before coverage can be trusted across joins; until then
   a decrease is the only signal, and the provider logs every decrease it
   sees.

1. **Saved-item lag.** The 10-minute margin before an ended turn's items count
   as finished is a guess. The user runs a read-only probe that reads a turn's
   items at intervals after it ends and records when the last item settles.
   Until then the margin is 10 minutes.

1. **Approval requests after AgentsView leaves.** First-reply-wins comes from
   open-source Codex; no probe has checked the hosted server. The user joins a
   thread that raises an approval prompt, closes the AgentsView connection
   while the prompt is pending, and records whether the user's own app still
   receives the prompt and can answer it, including an app that connects after
   the prompt was raised.

Only the user runs probes, because they use their ChatGPT login; whoever
executes the plan never reads the token. Test fixtures are synthetic: modeled on
the shapes the probes returned, with every value replaced.

## Risks

- The endpoints are private and undocumented. Any desktop app release may change
  the handshake, the protocol, or `query_v2`. Failures must surface as clear
  sync errors and must never damage stored sessions.
- OpenAI may treat third-party clients on these endpoints differently from their
  own apps.
- An item that stays unfinished early in a long turn, such as a background
  command, holds the resume point, so each pass re-reads the rest of that turn
  until the item finishes: the server has no "changed since" read
  (`after_updated_at_ordinal` is always unset in open-source Codex). Network
  cost within that one turn can grow with the square of its length. This is
  the accepted exception within one running turn (see Anti-goals); it ends
  when the item finishes or its turn ends and the save-lag margin passes.
- Re-fetching only unfinished items misses edits to items that already looked
  finished.
- Live token capture race: a turn that ends between the monitor's check and its
  `thread/resume` makes the call a cold resume. The tests showed what that
  does: the thread is loaded on OpenAI's side and starts its MCP servers;
  nothing runs, and the stored thread is unchanged. The probed thread had no
  active goal; in open-source Codex a resume also runs the thread's idle step,
  which can continue an active goal, so a cold resume of such a thread might
  start a turn. The join rule (listing `updatedAt` within the last 5 minutes)
  limits cold resumes to threads that changed moments before, and the
  monitor's one-cold-resume-per-turn rule bounds how often each can happen.
  The resume response reports the resume time as `updatedAt`; if the listing
  also moves the thread up, the user's thread list in OpenAI's apps reorders,
  and the fetcher fetches that thread once more and finds no change. The
  monitor detects it from `resumeKind: "cold"`, logs it, and leaves at once.
- A join rebinds the running session's client name and version to AgentsView
  (`set_app_server_client_info` in open-source Codex) and resets its MCP
  elicitation auto-deny. Later turns' initial context can mention the client.
  This is part of the accepted join exception; the sub-agent join probe also
  records whether a dot thread's later turns change after a join.
- If the hosted server sends an approval prompt only to connections present when
  it fires, a dot could wait on a prompt that the user's app never sees after
  AgentsView leaves. Open questions asks for the probe that settles it.
- OpenAI may change what `thread/resume` does on a running or stopped thread.
  The monitor checks `resumeKind` on every call and stops live capture for the
  account on any value other than `warm` or `cold`.

## Left to the builder

- Package and type names, file layout, and how the fetcher hands newly mirrored
  threads to the sync engine.
- Page sizes within protocol limits.
- The table for plan usage rows and how session details and cost views render
  them, within the rules under Plan usage.
