# Daily Pricing Catalog Refresh Design

## Background

Agentsview seeds its embedded model-pricing catalog and fetches the live LiteLLM
catalog when the writable daemon starts. A long-running daemon does not fetch
the catalog again, so models added upstream after startup remain unpriced in
dashboard and session-usage responses until the daemon restarts.

## Goal

While the writable daemon is running, attempt to refresh the live pricing
catalog at least once every 24 hours without adding request latency or unbounded
background work.

## Non-goals

- Change model-name matching or token accounting.
- Refresh pricing in read-only PostgreSQL or DuckDB servers.
- Add a user-configurable refresh interval.
- Make every usage request synchronously fetch pricing.

## Design

Keep the existing startup behavior: install the embedded fallback synchronously
and fetch LiteLLM once in the background. In addition, start one daemon-owned
periodic loop after startup. The loop waits 24 hours between ticks and invokes
the existing context-aware pricing refresh lifecycle.

The periodic loop will:

- own one ticker and no archive-sized state;
- run refresh attempts serially so catalog fetches cannot overlap;
- stop the ticker and return when the daemon context is cancelled;
- log a failed attempt and continue waiting for the next tick; and
- reuse the existing refresh cooldown, so a more recent successful or failed
  refresh trigger can make the daily tick a no-op without leaving the catalog
  older than the required interval.

The scheduler will live beside the existing daemon pricing lifecycle. Its loop
will accept a tick source and refresh callback internally so tests can exercise
the scheduling behavior without sleeping or testing `time.Ticker` itself. The
production wrapper supplies a 24-hour ticker and calls the existing
context-aware catalog refresh.

## Error and shutdown behavior

A network or database error is non-fatal to the daemon. It is logged once for
that attempt, and the loop remains active for the next scheduled attempt.
Cancellation stops the loop promptly and prevents future attempts.

## Test strategy

A focused Go test will drive the scheduler through a controlled tick channel. It
will prove that a tick invokes the refresh callback, a transient refresh failure
does not terminate the loop, a later tick invokes the callback again, and
context cancellation stops the scheduler. The test protects the consumer-visible
scheduling contract rather than inspecting source text or testing
standard-library ticker behavior.

## Acceptance criteria

- A continuously running writable daemon attempts a live pricing refresh at
  least every 24 hours.
- Startup still performs its immediate background refresh.
- Transient refresh failures do not stop later daily attempts.
- Daemon shutdown stops the periodic refresh loop.
- The scheduler uses constant memory and never overlaps refresh attempts.
