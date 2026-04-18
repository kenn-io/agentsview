# Live Refresh via SSE

Date: 2026-04-17

## Problem

Usage, Analytics, and Sessions-list views feel stale. Usage and Analytics
refresh on a 5-minute timer; the Sessions list has no auto-refresh at all. Users
watching an agent run must click refresh repeatedly to see cost, activity, and
newly-arrived sessions update.

## Goals

- Usage, Analytics, and Sessions-list update within a few seconds of the sync
  engine persisting new data, without the user clicking anything.
- Build one reusable mechanism, not three ad-hoc solutions.
- Degrade cleanly when the mechanism is unavailable (PG serve mode, dropped
  connection).

## Non-goals

- No changes to the existing per-session watch (`/api/v1/sessions/{id}/watch`).
  It already works and covers a different case (live message streaming inside
  one open session).
- No cross-machine push. PG serve mode falls back to polling; we do not hook
  PostgreSQL `LISTEN`/`NOTIFY` into the serve path.
- No event replay, resume cursors, or guaranteed delivery. Events are advisory:
  "refetch now". Losing one is harmless because the next sync cycle fires
  another, and the safety-net poll catches the final state.

## Architecture

A single global Server-Sent Events channel, `GET /api/v1/events`, that emits a
`data_changed` event each time the sync engine completes a cycle that inserted
or updated session or message rows. Three frontend stores — `usage`,
`analytics`, `sessions` — subscribe on mount and refetch on event, with a short
trailing-edge debounce so a burst of events collapses to one refetch of the
final state. Each view keeps its existing timed poll as a low-frequency safety
net.

```
sync engine  ──emit──>  broadcaster  ──fan out──>  /api/v1/events SSE
                                                        |
                                                        v
                               EventSource in frontend events store
                                                        |
                       ┌────────────────┬───────────────┤
                       v                v               v
                 usage store     analytics store   sessions store
                  (refetch)       (refetch)         (refetch)
```

The broadcaster is an in-process fan-out: the engine calls `Emit(scope)` on it
synchronously, and the broadcaster sends the event on each connected SSE
client's per-client channel. No persistence, no queueing beyond a small
per-client buffer that drops oldest on overflow (a slow client shouldn't block
the engine).

## Event contract

Example frame:

```
event: data_changed
data: {"scope":"messages"}

```

- `scope` is advisory; permitted values are `"messages"`, `"sessions"`, and
  `"sync"`. Subscribers MAY filter on it; today, all three refetch
  unconditionally. Keeping the field present from day one avoids a breaking
  change when a subscriber later wants to be pickier.
- Heartbeat: an `event: heartbeat` frame roughly every 30 seconds, matching the
  existing `handleWatchSession` pattern (`internal/server/events.go:244`). Keeps
  intermediaries from closing idle connections and gives the client a liveness
  signal.
- No `id:` field, no Last-Event-ID handling. Reconnection replays nothing.

## Authentication

The new endpoint lives under `/api/v1/`, so the existing `authMiddleware`
(`internal/server/auth.go:42`) applies. Because the browser's native
`EventSource` cannot set request headers, remote-auth clients pass the bearer
token as a `?token=<value>` query parameter — the same pattern already used by
`watchSession` in `frontend/src/lib/api/client.ts:417-421`. The frontend
`watchEvents` helper MUST mirror this: append `?token=` from `getAuthToken()`
when present.

One wiring change is required. Today `authMiddleware` only accepts the `?token=`
fallback when the request path ends with `/watch`
(`internal/server/auth.go:95`); any other path in `require_auth` mode returns
401 if the token is not in the `Authorization` header. The implementation must
make the new endpoint eligible for the query-param fallback, using one of:

- Extend the middleware allowlist in `auth.go:95` to also accept query-param
  tokens on the events path (e.g.,
  `HasSuffix(path, "/watch") || path == "/api/v1/events"`). Keeps the URL short
  and makes the rule explicit.
- Name the route with the existing suffix, e.g., `/api/v1/events/watch`, so no
  middleware change is needed. Trades URL aesthetics for zero auth-layer
  changes.

Either is acceptable; the plan should pick one and document it. Whichever path
is chosen, the Bearer-header route keeps working for non-browser clients.

Local loopback requests without a token continue to work in non-`require_auth`
mode, as they do for every other `/api/v1/` route.

## Backend

### `internal/sync/engine.go`

- Add an `Emitter` interface with one method: `Emit(scope string)`. The engine
  holds an optional `Emitter`; `nil` means no-op.
- For `SyncAll`, `SyncAllSince`, and `ResyncAll`, emit once after the call when
  `stats.Synced > 0`. These already return `SyncStats` whose `Synced` counter
  reports how many sessions were written (`internal/sync/progress.go:40-51`).
- `SyncSingleSession` currently returns only `error`, so it has no built-in
  "rows changed" signal. Either extend its return type (e.g.,
  `(changed bool, err error)`) or emit unconditionally on nil error — the
  fallback path only runs when the session monitor already detected a file
  change, so spurious events are rare. The implementation plan picks one; either
  is acceptable.
- Emission is fire-and-forget; the engine does not block on slow listeners.

### `internal/server/events.go`

- Add `handleEvents(w, r)` registered at `/api/v1/events`.
- Use the existing SSE helper: `stream, err := NewSSEStream(w)`
  (`internal/server/sse.go:22`) to set headers, then `stream.Send(event, data)`
  or `stream.SendJSON(event, obj)` for each frame.
- On connect: register a per-client buffered channel (e.g., cap 8) with a
  broadcaster owned by the server. On disconnect (request context done): remove
  the channel.
- Stream loop: `select` on the client channel, the 30s heartbeat ticker, and
  `r.Context().Done()`. The request context is wired to the server's base
  context via `WithBaseContext`, so SSE handlers exit promptly on graceful
  shutdown — follow the same pattern as `handleWatchSession`
  (`internal/server/events.go:235-248`).
- PG serve mode (`s.engine == nil`): return `503 Service Unavailable` with
  `Retry-After: 300`. Do not hold the connection open. `Retry-After` is advisory
  — browsers ignore it for `EventSource` reconnect timing and will retry at
  their default interval regardless — but setting it remains correct HTTP for
  any non-browser consumer.

### `internal/server/server.go` and wiring

- The broadcaster is created once at startup and must be visible to both sides:
  the engine (as an `Emitter` for sync events) and the server (as a subscription
  source for the handler). Because `server.New` already receives a
  pre-constructed `*sync.Engine` as a parameter, several wiring shapes are
  acceptable — pick whichever fits cleanest:
  - Construct the broadcaster in `cmd/agentsview/main.go` before the engine and
    hand it to both: the engine sees it as an `Emitter`, the server sees it as a
    subscription source (name the server option whatever reads best, e.g.,
    `WithBroadcaster`).
  - Add an `Engine.SetEmitter(e Emitter)` setter and call it after the engine is
    constructed but before the server is started.
  - Any equivalent arrangement that keeps the engine's `Emitter` field set
    before the first sync runs and gives the server handler a way to subscribe.
- Register the route with `HandleFunc`, not `s.withTimeout`, because SSE
  connections are long-lived — the existing `/api/v1/sessions/{id}/watch`
  registration at `internal/server/server.go:171-173` sets this precedent and
  explains why in a comment.

### Broadcaster

A small type owned by the server (new file `internal/server/broadcaster.go` or
co-located in `events.go` — implementer's call):

- Thread-safe subscribe/unsubscribe returning a receive-only channel.
- `Emit(scope)` copies the event to every subscriber's channel, non-blocking: if
  a subscriber's buffer is full, skip it. A slow client loses events; the
  safety-net poll still covers them.
- Shutdown is driven by request-context cancellation, not by a broadcaster-level
  Close call. `WithBaseContext` (see `internal/server/server.go`) propagates the
  server's base context to every incoming request, so cancelling it causes each
  SSE handler's `<-r.Context().Done()` branch to fire, which runs the handler's
  `defer unsub()` and returns the subscription's channel to the broadcaster.
  Once all handlers have exited, the broadcaster has no subscribers and no
  further work to do. A dedicated `Close()` API is not needed.

## Frontend

### `frontend/src/lib/api/client.ts`

- Add `watchEvents(onEvent: (e: DataChangedEvent) => void): EventSource`
  mirroring the existing `watchSession`. Returns the raw `EventSource` so
  callers can call `.close()` when done. Higher-level unsubscribe ownership
  lives in `events.svelte.ts`, which wraps this helper.
- Uses native `EventSource`, which auto-reconnects on transient errors at the
  browser's default interval (~3s).
- On a persistent non-2xx response (e.g., 503 in PG serve mode), `watchEvents`
  trips a circuit breaker after `WATCH_EVENTS_MAX_CONSECUTIVE_ERRORS` onerror
  firings without an intervening successful connection or event delivery, then
  calls `es.close()` so the browser stops reconnecting. Both `open` (a
  successful (re)connect) and a delivered `data_changed` event reset the
  counter, so transient network blips on a healthy stream do not trip the
  breaker. Subscribers fall back to their safety-net poll.

### `frontend/src/lib/stores/events.svelte.ts` (new)

- Owns a single `EventSource` shared across subscribers (ref-counted).
- Opens the connection on the first `subscribe`; closes on the last
  `unsubscribe`.
- Exposes `subscribe(fn): () => void` where `fn` receives `{scope}`.
- Tracks connection state (`idle` | `open` | `error`) for optional UI use later;
  not required for the first cut.

### Subscribers

`usage.svelte.ts`, `analytics.svelte.ts`, `sessions.svelte.ts` each:

- In `onMount` of the corresponding page component (or wherever the store's
  initial fetch is triggered), call `events.subscribe(debouncedRefetch)`.
- Call the returned unsubscribe in `onDestroy`.
- `debouncedRefetch` uses a ~300ms trailing-edge debounce: the timer resets on
  each event, and the refetch fires once after the burst settles. This ensures
  the fetch sees the final state, not a mid-burst snapshot.

### Safety-net polling

- Keep existing 5-minute `setInterval` on Usage and Analytics pages.
- Add a 5-minute interval on the Sessions list for parity. If that's too noisy,
  it can be dropped later; the SSE path is the primary mechanism.

## PG serve mode

- The sync engine is not running in PG serve mode; the server is a read-only
  view onto PostgreSQL.
- `/api/v1/events` returns 503 with `Retry-After: 300`.
- The frontend's safety-net poll is the sole refresh path in this mode. No
  additional code changes needed — the polling is already there for Usage and
  Analytics, and is being added for the Sessions list as part of this work.

## Error handling

- Engine → broadcaster: non-blocking send. Dropped events are acceptable.
- Broadcaster → client: non-blocking send; slow client loses events.
- Client → server: `EventSource` auto-reconnect handles transient network
  errors. No custom backoff.
- Refetch failures in subscribers: log, leave existing data in place, wait for
  the next event or poll. Same behavior as today's timer-driven refetch.

## Testing

- **Backend unit** — broadcaster subscribe/unsubscribe semantics; `Emit` is
  non-blocking when a subscriber is slow; no emission from `SyncAll`-family
  paths when `stats.Synced == 0`.
- **Backend integration** — start a test server, connect to `/api/v1/events`,
  trigger a sync that changes rows, assert a `data_changed` frame arrives.
  Assert 503 when engine is nil.
- **Auth coverage** — with `require_auth: true`, assert that
  `GET /api/v1/events?token=<valid>` opens the stream, that a missing or invalid
  token returns 401, and that the `Authorization: Bearer <valid>` header path
  also works.
- **Frontend unit** — `events` store ref-counts subscribers correctly, opens one
  EventSource, closes when the last subscriber leaves. Debounce collapses rapid
  events into one refetch call.
- **Manual** — open each of Usage, Analytics, Sessions list; run an agent; see
  values change within a few seconds without clicking refresh.

## Open questions

None blocking implementation. Possible follow-ups once this lands:

- Expose connection state in the UI (small "live" indicator vs "polling only").
- Narrow refetches by `scope` so Analytics doesn't recompute when only Usage
  data moved.
- Replace the Sessions-list safety-net poll with a lighter `/sync/status` check
  that only triggers a full refetch when `lastSync` changed.
