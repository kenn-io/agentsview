# Pricing Refresh Coordination Design

## Problem

The scheduled daemon refresh calls `pricingrefresh.RefreshCurrent`, while
PostgreSQL push paths call `pricingrefresh.EnsureCurrent`. Both operations can
fetch concurrently for the same `*db.DB`. SQLite serializes each write, but it
does not serialize the network fetch and subsequent catalog upsert as one
operation. A slower, older response can therefore overwrite data written by a
newer response.

## Goals

- Allow at most one current-catalog refresh per database at a time.
- Let ordinary `EnsureCurrent` callers wait for an in-flight refresh while
  respecting context cancellation.
- Make scheduled `RefreshCurrent` calls skip immediately when another refresh is
  already running.
- Keep different database instances independent.
- Release coordination state when no caller is using it.

## Non-goals

- Serialize unrelated database writes.
- Change the one-hour cooldown for ordinary refresh triggers.
- Queue a missed scheduled refresh behind the in-flight operation.
- Coordinate refreshes across processes.

## Design

`internal/pricingrefresh` will own a small registry keyed by `*db.DB`. Each
entry contains a capacity-one channel used as a gate and a reference count for
callers that hold or are attempting to acquire that gate.

Registry access is protected by one package-level mutex. Retaining a gate
increments its reference count before acquisition begins. Releasing or skipping
decrements the count; the registry deletes the entry when the count reaches
zero. This prevents closed or test databases from being retained indefinitely,
while ensuring that waiters and new callers cannot split onto different gates
for the same database.

`EnsureCurrent` acquires the gate with a `select` over the gate and
`ctx.Done()`. Once acquired, it runs the existing cooldown-aware lifecycle.
Cancellation while waiting returns `ctx.Err()` without fetching or changing
refresh metadata.

`RefreshCurrent` attempts a non-blocking acquisition. If the gate is already
held, it returns successfully without fetching. This is the scheduler-facing
behavior: an in-flight refresh already satisfies the need for current catalog
work, and no second response can race it.

The existing fetch, attempt-metadata, fallback, and cancellation-restoration
logic remains unchanged inside the acquired gate.

## Error and shutdown behavior

Fetch and database errors continue to propagate from the operation that acquired
the gate. The gate is released with `defer` on every acquired path. Scheduled
contention is not an error and is not logged. Waiting callers can leave promptly
when their context is cancelled.

## Test strategy

A focused test will block an `EnsureCurrent` fetch after it acquires the gate,
then invoke `RefreshCurrent` for the same database. The scheduled call must
return without invoking its fetch or persisting its model. After the first fetch
is released, its model must be persisted and both calls must finish cleanly.

This test fails if coordination is removed, if the scheduled path waits instead
of skipping, or if it performs a second fetch. Existing tests continue to cover
cooldown behavior, cancellation metadata restoration, and scheduled daily
execution.
