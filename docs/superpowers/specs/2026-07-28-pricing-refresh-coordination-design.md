# Pricing Refresh Coordination Design

## Problem

The scheduled daemon refresh calls `pricingrefresh.RefreshCurrent`, while
PostgreSQL push paths call `pricingrefresh.EnsureCurrent`. Both operations can
fetch concurrently for the same `*db.DB`. SQLite serializes each write, but it
does not serialize the network fetch and subsequent catalog upsert as one
operation. A slower, older response can therefore overwrite data written by a
newer response.

The scheduled refresh also writes directly through `*db.DB`, outside the sync
engine's exclusive barrier. A resync can copy pricing into its replacement
archive, then allow the scheduled refresh to update the old archive before the
replacement is swapped into place. Both the catalog update and its attempt
marker are then lost with the old archive.

## Goals

- Allow at most one current-catalog refresh per database at a time.
- Let ordinary `EnsureCurrent` callers wait for an in-flight refresh while
  respecting context cancellation.
- Make scheduled `RefreshCurrent` calls skip immediately when another refresh is
  already running.
- Serialize the full scheduled refresh lifecycle with resync database swaps.
- Keep different database instances independent.
- Release coordination state when no caller is using it.

## Non-goals

- Serialize unrelated database writes.
- Change the one-hour cooldown for ordinary refresh triggers.
- Queue a missed scheduled refresh behind the in-flight operation.
- Coordinate refreshes across processes.
- Change resync construction, pricing-copy, or database-swap behavior.

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

The daemon scheduler will also receive the daemon's sync engine through a narrow
interface exposing `RunExclusive(func() error) error`. When syncing is enabled,
each tick runs the complete `RefreshCurrent` call inside `RunExclusive`, using
the same mutex as sync and resync swaps. The engine lock is acquired before the
pricing gate; no existing path holds the pricing gate while acquiring the engine
lock. This prevents refresh writes from landing in the old archive after pricing
has been copied into a staged replacement.

When `--no-sync` is active, the daemon has no sync engine and cannot perform a
resync swap. The scheduler therefore retains a direct `RefreshCurrent` path for
a nil exclusive runner instead of constructing an otherwise-unused engine.

## Error and shutdown behavior

Fetch and database errors continue to propagate from the operation that acquired
the gate. The gate is released with `defer` on every acquired path. Scheduled
contention is not an error and is not logged. Waiting callers can leave promptly
when their context is cancelled.

Errors returned by `RunExclusive` propagate through the existing scheduler
logging path. Context cancellation remains owned by `RefreshCurrent`; waiting
for the sync engine barrier is bounded by the daemon's existing sync and resync
shutdown behavior.

## Test strategy

A focused test will block an `EnsureCurrent` fetch after it acquires the gate,
then invoke `RefreshCurrent` for the same database. The scheduled call must
return without invoking its fetch or persisting its model. After the first fetch
is released, its model must be persisted and both calls must finish cleanly.

This test fails if coordination is removed, if the scheduled path waits instead
of skipping, or if it performs a second fetch. Existing tests continue to cover
cooldown behavior, cancellation metadata restoration, and scheduled daily
execution.

A scheduler regression test will create a valid staged replacement archive, hold
the real sync engine barrier before swapping it, and trigger a scheduled
refresh. The refresh must not fetch while the barrier is held. After the test
allows the swap to complete, the refresh must run against the reopened archive
and its model must be present there. Passing a nil runner in the existing
scheduler test preserves coverage for the `--no-sync` direct path.
