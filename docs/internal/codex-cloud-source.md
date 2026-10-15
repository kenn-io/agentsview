# Codex Cloud source design

The `codex-cloud` source imports threads that live only on OpenAI's hosted Codex
app-server: ChatGPT dot tasks, Codex web tasks, and Codex cloud tasks. The UI
calls them chats. Each thread becomes a searchable session with its full trace,
token counts for the turns AgentsView heard while they ran, and the plan usage
OpenAI reports for it.

This document explains how the source works and why. Setup and user-facing
limits live in [Configuration: Codex Cloud](../configuration.md#codex-cloud) and
[Token usage](../token-usage.md). The wire formats and the evidence for them
live in the `codex-cloud` entry of
[session-format-sources.md](session-format-sources.md).

## Where the data lives

None of these threads exist on the user's computer. A dot that used a personal
computer leaves only its working folder (`~/Documents/Codex/<date>/task-N`),
which holds what the work produced, not what the agent did.

OpenAI's desktop app reads them from a remote app-server at
`wss://codex-cloud-backend.chatgpt.com/`, which speaks the same JSON-RPC
protocol as a local `codex app-server`. Three facts shape the whole design:

- **Stored history has no token counts.** OpenAI sends token counts only as live
  `thread/tokenUsage/updated` notifications to a client connected to the
  running thread.
- **The per-thread token report is closed to plan accounts.** The
  `thread_usage/query` endpoint a local Codex uses answers HTTP 403 for a Pro
  account. The endpoint behind the desktop app's Top chats list,
  `thread_usage/query_v2`, does answer, but only with shares of the plan's
  weekly and five-hour limits and credits, never tokens or dollars.
- **Nothing is public.** OpenAI documents none of the three endpoints AgentsView
  uses. Every rule here rests on read-only probes the account owner ran, plus
  the open-source app-server in `codex-rs/app-server`.

## Boundaries

- **Read-only, with one exception.** The fetcher sends only `initialize`,
  `initialized`, `thread/list`, `thread/turns/list`, and `thread/items/list`.
  The live token monitor adds `thread/resume`, accepted only with `threadId`
  and `excludeTurns: true`. An allowlist in the client fails any other method
  before it is sent. Plan usage sends only to the configured `usage_endpoint`.
- **No answers to the server.** Neither client answers a server-initiated
  request. In open-source Codex the first reply to an approval prompt wins, so
  even an error reply from AgentsView could beat the user's own app.
- **No token refresh.** Refreshing would rotate the refresh token the Codex CLI
  depends on. A refresh request from the server closes the connection
  unanswered and starts a wait for that Codex home.
- **The token stays in memory.** AgentsView reads `auth.json`, decodes the ID
  token in memory, and redacts the access token from every error. A fetch
  holds the token until it ends; a live join keeps its login for the join's
  life, up to 6 hours, to redact errors and recheck its home. The websocket
  carries it as the `openai-bearer.<token>` subprotocol; a plain
  `Authorization` header is rejected with 403.
- **Honest identity.** AgentsView names itself in `User-Agent`, `originator`,
  `clientInfo`, and the subprotocol `codex-client.agentsview`. If OpenAI ever
  refuses that, AgentsView reports a protocol change. It never falls back to
  the desktop app's identity.
- **Endpoint overrides stay on OpenAI.** A non-loopback `endpoint` or
  `usage_endpoint` must name `chatgpt.com`, `openai.com`, or a subdomain, over
  `wss` or `https`. Plain `ws` and `http` work only on loopback. Neither
  client follows redirects, because a redirect would carry the token to a URL
  the allowlist never checked.
- **Cost must not look free.** A session with no token counts never shows zero
  tokens or zero cost on any surface.

## Components

```text
 OpenAI app-server (wss)       query_v2 (https)
   |            |                    |
 sync job    live discovery        plan usage step
 (5 min)     (30 s) -> monitor     (runs after each sync pass)
   |            |                    |
   +------------+--------------------+
                |
   {dataDir}/sources/codex-cloud/<accountKey>/<threadId>.jsonl.zstd
                |
   codex-cloud provider (normal sync engine)
                |
   SQLite archive -> PostgreSQL, ClickHouse, DuckDB mirror, hosted hub
```

| Part             | Code                                                                    |
| ---------------- | ----------------------------------------------------------------------- |
| Sync pass        | `internal/codexcloud/pass.go`, `fetch.go`, `listing.go`, `selection.go` |
| Live capture     | `internal/codexcloud/discovery.go`, `monitor.go`                        |
| Plan usage       | `internal/codexcloud/usage_step.go`, `usage_client.go`, `roots.go`      |
| Logins and waits | `internal/codexcloud/auth.go`, `waits.go`, `classify.go`                |
| Status text      | `internal/codexcloud/status.go`, `/api/v1/codex-cloud/status`           |
| Mirror files     | `internal/codexcloud/mirror/`, `internal/zstdframe/`                    |
| Parser           | `internal/parser/codex_cloud*.go`                                       |

Both jobs are `poller.Job` values with the scheduler's jitter. When a joined
turn completes, the monitor calls the sync job's `TriggerNow` so the trace is
fetched again without waiting for the next interval; `poller.ErrTriggerPending`
counts as success.

The sync engine never talks to OpenAI. It parses mirror files like any other
file-backed agent, so fingerprints, full resync, archive content policy, raw
capture, and replica pushes need nothing special. A full resync rebuilds SQLite
from the mirror and fetches nothing.

## Accounts and logins

AgentsView syncs every ChatGPT login found in these Codex homes, in order:
`CODEX_HOME` from the daemon's own environment or `~/.codex`, then each
`[agents.codex] homes` entry. A background daemon rarely inherits the user's
shell `CODEX_HOME`, so the shell value does not count.

Each login is keyed by `tokens.account_id` plus the ID token's ChatGPT user ID.
The account ID alone names a workspace, and two users of one Business workspace
must stay separate. Homes with the same key sync once, through the first home; a
401 there tries the next home on the same key before reporting a sign-out. State
and mirror files live in a directory named after the key, which never holds
token material.

AgentsView reads `auth.json` only. Codex ignores that file in `keyring` and
`ephemeral` modes, so those homes get a status row instead. In `auto` mode
AgentsView uses `auth.json` when it holds a token; a missing token, or a 401,
shows the same row, because Codex may keep the current login in the keyring. The
row names the fix (`cli_auth_credentials_store = "file"`) and its cost: the
login then sits in a plain file any program running as the user can read.
AgentsView does not read the system keyring.

## Failure handling

The scheduler's backoff applies to a whole job, so one account's failure would
slow every account. Instead each account keeps one persisted next-attempt time
for the thread host, shared by sync and live discovery, and the jobs return no
error to the scheduler for one account's failure.

| Failure                                                       | Wait                                                             |
| ------------------------------------------------------------- | ---------------------------------------------------------------- |
| Signed out (no token, 401)                                    | Per Codex home, in memory: until `auth.json` changes or one hour |
| Server asked for a fresh login                                | Same as signed out; every job skips that home                    |
| 403                                                           | One hour, doubling to 24 hours                                   |
| 429                                                           | `Retry-After`, else 5 minutes doubling to one hour               |
| Network failure or 5xx                                        | One minute, doubling to one hour                                 |
| Protocol change, oversized reply, unexpected handshake status | Same as 403                                                      |

A success resets the account's wait. Live discovery's successful listing also
clears a network, 5xx, or 429 wait and triggers a sync run. Per-thread failures
use the retry set instead (see Incremental sync).

Every handshake and call times out after 2 minutes. A thread that times out goes
to the retry set and ends the pass, because the next thread would likely time
out too. A handshake redirect or any 4xx other than 401, 403, and 429 is a
protocol change.

JSON-RPC errors are classified by message, not code, because the open-source
server uses `-32600` both for an unreadable request and for a missing thread:

1. `-32600` with `data.errorType: "environment_terminated"` is an error for that
   thread.
1. `-32001`, or `-32600` with `data.reason: "serverShuttingDown"`, is treated
   like a 5xx for the account.
1. `-32601` starting `unsupported app-server method`, `-32600` starting
   `Invalid request:` or naming `experimentalApi`, and `-32602` on anything
   but an anchored item read, are a protocol change. Sync waits as for 403.
   The monitor stops live capture for the account and leaves the sync wait
   alone.
1. `-32602` on an anchored `thread/items/list` makes the fetcher re-read that
   turn without the anchor in the same pass.
1. Any other error on `initialize` or `thread/list` fails the pass like a 5xx.
   On a per-thread call it is an error for that thread: the fetcher retries
   the thread later, and the monitor drops that join.

A reply larger than the 64 MiB read limit closes the connection. The fetcher
reconnects and fetches that thread again with one item per page, then records a
per-thread failure if that also fails. An oversized turn listing is not retried,
because its turns carry no items. One huge page never fails the account.

## Fetcher state

State lives beside the mirror files, outside SQLite, written atomically:

- `<threadId>.state.json` per thread: the stored `updatedAt`, resume point,
  verified mirror offset, resolved parent, per-turn status and first-seen
  times, record digests for dedup, `createdAt`, the last plan usage record's
  digest and kind, the newest plan usage row and when it was fetched, and the
  usage root the thread counts under.
- `<rootId>.descendants.json` per usage root: its known descendants.
- `<parentId>.waiting.json` per unmirrored parent: children waiting for it.
- `account.json` per account: listing watermarks, waits, plan usage pauses, and
  the working sets (retry set, check-again flags, unfinished threads, pending
  and refresh roots).

The working sets hold only threads with outstanding work, so they grow with
recent activity, not with the archive. Retired threads (see Incremental sync)
are the exception: each stays listed until the thread changes again. All writes
for one account go through one lock shared by the sync job and the monitor.

Lost state is recoverable. A state file that does not decode is moved to
`<name>.corrupt` and the thread or account syncs as new. A lost per-thread file
leaves offset zero, so the next append treats every complete frame as kept and
rebuilds the digests from it. A mirror file that is missing or shorter than its
stored offset resets the thread's resume point, so the next pass fetches the
whole thread again. A live-token or plan usage write checks for this first,
because its append would recreate the file and hide the loss. The write then
starts the new file with the saved listing, which the parser needs. If the full
fetch fails, nothing is written. Until the mirror holds the thread again, sync
keeps the archived copy as it is. It skips a parse with fewer messages than the
archive, or one with no token counts where the archive has some. Server history
carries no token counts, so after a refetch the archived counts stay until new
live counts arrive. Sync always replaces an archived copy from an older parser
version, so a parser change still takes effect. A descendants or waiting file
that does not decode is rebuilt from the saved thread files. A descendants list
collects the threads counted under that root. A waiting list collects the
threads whose parent is the waiting thread. The damaged file stays in place
until the rebuilt list is saved, so a failed rebuild is tried again on the next
load.

A missing or corrupt `account.json` with thread files still present makes
AgentsView rebuild the plan usage queue from the thread files. The queue is the
set of pending and refresh roots described in Scheduling; a usage root is the
thread a group of sub-agent threads counts toward (see Usage roots). The rebuild
reads only local files and fetches nothing. For each root:

- A root whose newest plan usage record settled it after its group's newest
  change stays settled.
- A root whose newest row is complete and was fetched after that change goes
  back on the refresh schedule.
- Every other root is pending again, keeping its row if it has one.

Each root keeps its group's stored change time, so a root that changed more than
7 days ago settles on its first answer. A root whose files cannot be read is
logged and left out until its chat changes again.

The same rebuild restores the chats that a pass fetches again although their
listing has not changed. A chat with a running turn, or whose newest turn ended
within the save-lag margin, is unfinished again. A chat whose `updatedAt` is
within 2 minutes is marked to check again. Without this, a chat waiting for late
items would keep an incomplete transcript until it changes again.

## Incremental sync

The protocol has no "items changed since" query. So for each thread the fetcher
keeps a **resume point**: the last item of the longest run of finished items
from the thread's start. Each pass re-reads everything after it.

An item counts as finished when its turn has ended and the save-lag margin has
passed, when a later turn started and the margin passed for that start, when the
thread was archived at least 10 minutes ago, or when `completedAtMs` is set and
any `status` is terminal. The margin exists because saved items lag the live
work by minutes. It is 10 minutes after the turn's end, judged from OpenAI's
`completedAt` with a 2-minute clock allowance, or from when the fetcher first
saw the end. The resume point stops at the first unfinished item, because Codex
runs tool calls in parallel and a later command can finish first.

Each pass reads four listings: the default listing and the sub-agent listing,
each for active and archived threads. The default listing sends no
`sourceKinds`, because setting every kind returned nothing for these threads.
Each listing walks `updated_at` descending and stops after the first page that
holds a thread older than its watermark. `updatedAt` has whole-second
resolution, so the walk finishes that page. A page whose order ever increases is
a protocol change, not an early stop.

One selection per account then unions:

- threads whose `updatedAt` changed;
- **unfinished** threads: listed `active`, a turn last seen `inProgress`,
  unfinished items, or a margin not yet passed. Listing status alone is
  unreliable, because a thread listed `idle` can have a running turn. A turn
  stuck in progress with no change for 24 hours stops this selection;
- **check-again** threads: a fetch that started within 2 minutes of the thread's
  `updatedAt` could have missed a second update in the same second, so the
  thread is fetched again until a later fetch finds no change;
- due entries of the **retry set**, after every other thread, because a thread
  OpenAI never answers ends the pass.

For each selected thread the fetcher reads turn statuses first
(`thread/turns/list`, `itemsView: notLoaded`, newest first, stopping at the
resume point's turn), then items from the resume point with an item anchor, then
each later turn in full. Reading statuses first means a turn that ends between
the calls is not yet treated as ended. The hosted server defaults to
`itemsView: full`, so the fetcher always sends `notLoaded`.

Only records that differ from the newest stored record for the same key are
appended, so an unchanged re-fetch leaves the file byte-identical and causes no
parse. The fetcher fsyncs, then saves the new offset and resume point. A crash
before the save repeats the fetch and never skips records. A listing's watermark
advances once every changed thread from it is durable or in the retry set, so a
failing thread never holds the watermark back.

A failing thread retries with backoff from 5 minutes to one hour. After 7 days
of failures with no new `updatedAt`, it is **retired**: retries stop, the thread
ID and last failure go to the log, and stored data stays. A new `updatedAt`
brings it back. This also covers deleted threads, since nobody has yet seen how
the server reports one; until that reply is known, no error counts as a
deletion. AgentsView never scans for deletions, and sessions stay either way.

Work per pass follows what changed: one listing call per listing for a quiet
account, and about k items for a thread that grew by k items. The exception is
an unfinished item early in a long running turn, such as a background command.
It pins the resume point, so each pass re-reads the rest of that turn until the
item finishes.

## Mirror format

Each thread has one file, `<accountKey>/<threadId>.jsonl.zstd`. Every append is
one complete zstd frame of JSONL records with a content checksum and no header
frame. Records carry an envelope naming their kind and fetch time: thread
metadata, turns, items as returned, plan usage rows and outcomes, token usage
records, and turn completions. When the monitor reaches a thread before sync
does, it writes a `thread_resume` metadata record without `updatedAt` and
`recencyAt`, which a resume sets to the resume time. The parser uses it only
until a listing record arrives.

Before appending, the writer checks only the bytes after the stored offset.
Complete frames there come from an append whose state save crashed, so the
writer keeps them and rebuilds digests from them. An incomplete last frame is a
torn append and is truncated. Readers decode only complete frames. Frame
handling is shared with the DeepSeek harness format through
`internal/zstdframe`; that format's header frame and torn-frame behavior are
not.

Mirror files declare `RawCaptureAppendOne`, so raw capture ships them to a
hosted hub and replaces a file whose prefix a torn-frame repair changed. State
files are never captured. The agent sets `RemoteHostSyncExcluded`, not
`RemoteSyncExcluded`: remote host sync must never copy the mirror or its state,
while raw ingest must still accept the mirror.

## Parsing and mapping

The provider parses a changed mirror file in full. The engine's incremental path
cannot replace a stored item or append usage events, and only threads whose file
changed are parsed. A running thread is parsed again whenever a pass appends to
it, and the monitor's one-minute flush bounds how often token records add a
parse.

Each item comes from its newest record for `(turnId, item.id)`, so re-fetched
items replace earlier ones. Item types map to the nearest existing message or
tool category; unknown types become a generic tool call with their raw type
name, never dropped.

- **Origin.** `codex_work_cca` or `threadSource: aeon_child` is a dot,
  `codex_work_web` is Codex web, `codex_cloud` is Codex cloud. It goes in
  `Entrypoint`. `SessionKind` stays empty so dot tasks are not classified as
  automated workers.
- **Working directory.** The first `environments[].cwd` of the newest listing
  record that has one, else `cwd`. Resume responses abbreviate the home
  directory, so the monitor's metadata record is not used.
- **Model.** Sessions have no model field, so the thread's model and reasoning
  effort go on each assistant and thinking message.
- **Parent.** The resolved parent is `parentThreadId`, else the `thread_spawn`
  parent in `source`, else the `source_thread_id` in a `<codex_delegation>`
  tag at the start of the first `userMessage`. Delegated dot tasks name their
  parent only that way, and the parent is the dot's own conversation, which is
  not listable. The resolved parent wins over `forkedFromId`, because usage
  roots follow it.

## Live token capture

Every 30 seconds a discovery job lists recent non-archived threads and checks
the newest turn of each thread updated in the last 5 minutes. It ignores listing
status, which can be stale. The monitor joins a thread only when that turn is
`inProgress` and the thread changed in the last 5 minutes, checks again just
before joining, and sends `thread/resume` with `excludeTurns: true`.

The 5-minute rule exists because a resume of a stopped thread is a cold resume:
OpenAI loads the thread, starts its MCP servers, and stamps it with the resume
time. In open-source Codex a resume also runs the thread's idle step, which can
continue an active goal. The window keeps the monitor to threads that were
running moments ago.

The resume reply's `resumeKind` decides what happens:

- `warm`: the thread is running, and the monitor stays.
- `cold`: the monitor logs it and leaves at once. It does not join that thread
  again until the turn listing shows a newer turn, so a stuck turn causes at
  most one cold resume.
- anything else: a protocol change. Live capture stops for the account until a
  restart or until discovery sees live capture turned off.

Each joined thread gets its own websocket, because the server does not support
`thread/unsubscribe`; closing the connection is the only way to leave. The cap
is 10 joins per account. Sub-agent threads are not joined until a probe shows a
join is warm and harmless for them. Delegated dot tasks are in the default
listing and are joined normally.

While joined, the monitor reads only `thread/tokenUsage/updated`,
`turn/completed`, and `model/rerouted`. On `turn/completed` every update for
that turn has already arrived, because one websocket delivers in order. The
monitor writes a turn-completion record, flushes, and leaves. It also leaves
after 6 hours, on a refresh request, or when live capture turns off.

Each joined thread holds a daemon work lease until its final save, so a detached
daemon does not exit on idle while it is listening in, for up to the 6-hour join
limit. A daemon that is shutting down does not start listening in on new chats.

Records are buffered and appended at most once a minute, on turn completion, and
on leaving. Totals are cumulative, so a crash loses only the buffer and the next
update recovers the counts. If saving fails, records wait in memory and the
status line warns. Past 256 waiting records, compaction shrinks a thread's queue
to the records that still carry information, about three per turn, which keeps
the queue bounded. A run is a stretch of consecutive token records in one join,
turn, and model whose totals only grow. Compaction drops most of a run but keeps
its peak (the record with the largest last-call context) and the record before
it, so the peak call is still priced as one request. The other calls in the run
merge into one increase that is priced at base rates. During a save outage, a
long turn's long-context calls other than its peak are therefore underpriced. A
restart loses the waiting records.

### From totals to usage events

Each record holds `total` (running totals since the thread started), `last` (the
latest model call), the context window, and the current model. Counters arrive
as JSON floats such as `4603049.0`; a true fraction is a protocol change.

- **Increases.** Each event is the field-by-field increase in `total` since the
  previous record. A decrease starts a new baseline and never makes a negative
  event. It is not logged, because every reparse would log every past reset
  again. The dedup key combines thread ID, baseline, and observed total, so a
  reparse reproduces the same events.
- **One call or many.** When the increase equals `last` exactly, it is one model
  call and becomes a `codex-cloud-request` event, so per-request price bands
  apply. Any other increase spans several calls and becomes a thread-level
  `session` event priced at base rates, because a band chosen from a sum would
  be wrong. This can understate long-context cost.
- **Token fields.** OpenAI's input includes cached reads and cache writes, so
  the mapping matches the local Codex provider: uncached input is input minus
  both, reasoning stays inside output.
- **Session fields.** The engine skips `session` events when it derives session
  token fields, so the provider sets total output and peak context itself, as
  Cline does.
- **Dates.** The first record of a join covers every earlier turn and is dated
  when it was observed, even if those turns ran on another day.

### Coverage

The provider counts finished turns that no capture covers. A turn is covered
when the same baseline has a token record for a later turn, or a token record
for that turn followed by its turn-completion record from the same join. A
turn-completion record alone does not count: for threads with
`historyMode: "legacy"`, a join that lands after the last model call sees
`turn/completed` with no counts. Coverage goes in the per-session plan usage
table, so every backend can show "N turns not captured" and count partly
captured sessions.

## Plan usage

`query_v2` reports a share of the weekly and five-hour limits and credits, split
by model, reasoning effort, and speed. AgentsView stores and shows exactly that
and never converts it to tokens or dollars.

### Usage roots

The desktop app counts sub-agent threads toward the thread that started them,
and AgentsView does the same. A thread is a usage root unless it has a known
ancestor through its resolved parent. A request sends each root with its
`created_at` and all known descendants in `descendant_thread_ids`. A child whose
parent is not mirrored waits in the parent's waiting file and counts as its own
root. When the parent arrives, the child and its descendants move under the
parent's root, and each gets a "counted under" record. Building a group reads
only the root's descendant file, so the work follows what changed.

### Scheduling

Plan usage runs after the thread step, with its own pauses, and never changes
thread sync status. A pass skips it when the thread step hit a local state error
or a refresh request.

- **Pending roots** have no usable row yet, or had a change. Each has its own
  next attempt, backing off from 5 minutes to 6 hours. A root that already has
  a usable row and keeps changing (a running thread is fetched every pass)
  re-enters with a wait that doubles while any thread in its group is running,
  up to 6 hours, so it is not asked about every pass. The wait resets once the
  group stops running or after 24 quiet hours.
- **Refresh roots** have a usable (`available`) row. Each refreshes at the later
  of one day after the row's `data_as_of` and one day after its last attempt.
  Seven days after the root's last change it leaves the refresh set without
  another request, and its last row stays as it is.
- **Change time.** A root's last change is the newest change among the threads
  in its group. A thread's change is the later of its listing `updatedAt` and
  the last time a fetch saved new items for it, including items recovered
  after a crash. A refetch that finds nothing new does not move it, so a
  finished thread the listing still shows as `active` keeps its schedule.
- **Settling.** Usage can change for 7 days after a root's last change. A
  pending root answered after that settles: a `partial` or `unavailable` row
  is stored as final; a root left out stores its last row again as final, or
  gets a "settled with no row" record when it never had one. Only HTTP 200
  answers settle. Three failed requests in a row after the 7 days stop with a
  "could not check" record instead. Any later change to the root or a
  descendant starts over.
- **Budget.** Each pass sends at most 5 requests, pending roots first. Each
  request holds at most 100 groups and 1,000 thread IDs, the desktop app's
  limits. A root whose group alone exceeds 1,000 IDs is never sent and settles
  with a "too many sub-agent threads" record.
- **Account answers.** 429 pauses plan usage for the account. 403 or 404 means
  OpenAI does not offer this report to the account: AgentsView stops asking
  for 24 hours and shows no error. A 401 tries the next home on the same login
  key, and pauses plan usage only when every home has refused. A 429, a 403 or
  404, or no usable home stops the pass's remaining usage requests. Replies
  are capped at 16 MiB.

### Records and storage

Every outcome is a record in the mirror file, so a full resync or a hub can
rebuild what session details show: response rows, "left out", "settled with no
row", "account unavailable", "too many sub-agent threads", "could not check",
and "counted under". A row is appended only when it differs from the last plan
usage record of any kind in that file.

The provider resolves ordering from record positions at parse time and writes
one row per session to `session_plan_usage`: the newest row's figures, whether
it is final, and the newest outcome only when that outcome is newer than the
row. A newer "counted under" record clears the figures and keeps only the root
reference. Backends never compare times to decide this. The same row carries
live-capture coverage.

The table is pushed to PostgreSQL, CockroachDB, and ClickHouse like other
per-session data and is part of the DuckDB mirror. On a hosted hub, sessions use
raw projection IDs, and the hosted ID mapper resolves references only through
link edges. The raw projection therefore writes a usage-root link, so a "counted
under" reference resolves at any depth, not just to the direct parent.

### Totals

Sessions with no usage events are left out of token and cost totals in every
backend. Those with a `session_plan_usage` row are counted as "billed as plan
usage". A thread with neither token records nor any plan usage record still gets
a row, with no figures, no outcome, and no coverage, which session details show
as "Plan usage: not checked yet". A chat waiting for its first plan usage answer
is therefore counted even when pauses, the per-pass request budget, or homes
that refuse plan usage keep that answer from arriving. The count is computed at
query time from `session_plan_usage`, with the same filters as the totals,
because the usage cache has no record of plan usage. A plan-only session still
produces activity facts, so per-model views show its message count, but those
columns read "Plan usage", not zero. The source does not set
`NoPerMessageTokenData`, whose "no token data" notice would contradict that
count.

## Configuration and gates

Settings live in `[agents.codex-cloud]`: `enabled` (default `false`),
`endpoint`, `usage_endpoint`, and `live_tokens` (default `true`). The source is
off by default because it uses private endpoints as the user's ChatGPT account.
Once enabled, live capture is on unless `live_tokens = false`.

The shared `[agents.<id>]` reader skips this table, which has its own parser. An
unknown key or a bad value there disables only this source and shows in its
status; the rest of the config loads. `disabled_agents` wins over `enabled`. In
a daemon without source sync (`--no-sync`, or an archive-only archive) both jobs
still run, contact nothing, and report the source as off.

Poller jobs register only at daemon start, and the daemon reloads settings only
on a save through the settings API. Both jobs therefore re-read
`[agents.codex-cloud]`, `disabled_agents`, and `[agents.codex] homes` from
`config.toml` at the start of each run, so a hand edit takes effect without a
restart. If `config.toml` stops parsing, the jobs keep the last good settings
and say so.

## Status

Poller state lives in memory, and AgentsView had no general sync-error display,
so this source has its own status endpoint, shown in the UI's sync status area
and in `agentsview serve status`. It appears only once the user has configured
the source. Each login gets one row with up to three lines: import state, live
tokens, and plan usage. A plan usage problem never hides the import state. Every
failure message names a next step, and messages for a non-default Codex home
name the exact `CODEX_HOME=<home> codex login` command, or its PowerShell form
on Windows. Raw details go to `debug.log` in the data directory, with the token
redacted.

## Several machines

Session IDs come from the thread ID, so every machine produces the same ID for a
thread. PostgreSQL keeps the first machine's copy and skips the others as
ownership conflicts. ClickHouse keeps the last push. A hosted hub keys raw
projections per device, so two machines can project one thread twice and double
its tokens. Both machines would also join the same running threads. The user
docs recommend enabling the source on one machine.

## Rejected alternatives

- **Token estimates from trace text.** The trace leaves out system prompts, tool
  definitions, and the history each turn rereads, and these models have no
  public tokenizer. An estimate would be far off and look authoritative.
- **`account/usage/read` or `thread_usage/query`.** The hosted server rejects
  the first, and the second answers 403 for plan accounts.
- **Codex OpenTelemetry export.** It covers only local processes and leaves out
  content by default.
- **Engine-level incremental parsing.** The append path cannot replace items or
  append usage events. Full parses of changed threads are bounded by what
  changed; a benchmark on the largest synthetic fixture tracks the cost.
- **Storing plan usage as usage events or Copilot `ai_credits`.** Rows are
  running totals, and a percentage of one week's limit does not add across
  weeks. One latest row per session is the honest shape.

## Limits and unverified assumptions

- **Private endpoints.** Any OpenAI release can change the handshake, the
  protocol, or `query_v2`. AgentsView reports a protocol change and keeps
  stored sessions.
- **Joining is visible to OpenAI.** In open-source Codex a join records the
  joining client in the running session, and later turns' context can mention
  it; the hosted server likely does the same. A cold resume can move the
  thread up in the user's list.
- **Untested resume cases.** Whether a cold resume of a thread with an active
  goal starts a turn, and whether an approval prompt raised while AgentsView
  is joined still reaches the user's app afterwards.
- **Missed turns.** A turn that ends before a join, or while all 10 slots are
  full, gets no live tokens. The 5-minute window depends on the local clock.
- **Counter restarts.** A reset is detected only as a decrease. A restart that
  grows past the old total before the next join undercounts. No probe has seen
  a reset.
- **Model changes.** A change reported only by `thread/settings/updated`, which
  needs the experimental API, and a reroute before the join both go to the
  earlier model.
- **Edits to finished items** are missed, because the protocol cannot list items
  changed since a point.
- **Other model providers.** Listings never send `modelProviders` (the server
  rejects it), and every probed thread had `modelProvider: "local"`.
- **Open probes.** The deleted-thread reply, sub-agent joins, whether
  `parentThreadId` finds every descendant, whether the default listing
  includes sub-agent threads (both listings are read until then), whether
  archiving changes `updatedAt`, and the real save lag behind the 10-minute
  margin. Probes use the account owner's login, so only the owner runs them.
