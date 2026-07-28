# Pricing Refresh Coordination Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent scheduled and push-triggered pricing refreshes from fetching
and writing concurrently for the same SQLite database.

**Architecture:** Add a package-local registry in `internal/pricingrefresh` that
maps each `*db.DB` to a capacity-one channel gate and a reference count.
Ordinary refreshes wait for the database gate with context cancellation, while
forced scheduled refreshes attempt the gate without blocking and return
successfully when another refresh is already in flight.

**Tech Stack:** Go, `context`, `sync`, channels, the existing SQLite pricing
catalog, and Testify.

## Global Constraints

- Allow at most one current-catalog refresh per database at a time.
- Preserve the one-hour cooldown for ordinary `EnsureCurrent` calls.
- Make scheduled `RefreshCurrent` calls skip immediately on contention.
- Keep different `*db.DB` instances independent.
- Remove registry entries when their reference count reaches zero.
- Preserve existing fetch, fallback, attempt-metadata, error, and cancellation
  restoration behavior after gate acquisition.
- Add no dependencies, schema changes, cross-process coordination, or queued
  scheduled refresh.

______________________________________________________________________

### Task 1: Serialize current pricing refreshes per database

**Files:**

- Modify: `internal/pricingrefresh/refresh.go:3-180`
- Modify: `internal/pricingrefresh/refresh_test.go:183-230`

**Interfaces:**

- Consumes:
  `ensureCurrent(context.Context, *db.DB, func(context.Context) ([]pricing.ModelPricing, error), time.Time) error`
  and
  `refreshCurrent(context.Context, *db.DB, func(context.Context) ([]pricing.ModelPricing, error), time.Time) error`.

- Produces: package-local `refreshGate`,
  `retainRefreshGate(*db.DB) *refreshGate`,
  `releaseRefreshGateReference(*db.DB, *refreshGate)`, and coordination at the
  start of `runCurrent`.

- [ ] **Step 1: Write the failing concurrent-refresh regression test**

    Add `sync/atomic` to `internal/pricingrefresh/refresh_test.go` and add this
    focused test after `TestRefreshCurrentFetchesDespiteRecentAttempt`:

    ```go
    func TestRefreshCurrentSkipsWhileEnsureCurrentInFlight(t *testing.T) {
        database := testDB(t)
        now := pricingTestNow()
        ensureFetchStarted := make(chan struct{})
        releaseEnsureFetch := make(chan struct{}, 1)
        ensureDone := make(chan error, 1)

        go func() {
            ensureDone <- ensureCurrent(context.Background(), database, func(
                context.Context,
            ) ([]pricing.ModelPricing, error) {
                close(ensureFetchStarted)
                <-releaseEnsureFetch
                return []pricing.ModelPricing{{
                    ModelPattern: "ensure-model",
                }}, nil
            }, now)
        }()
        defer func() {
            releaseEnsureFetch <- struct{}{}
        }()

        require.Eventually(t, func() bool {
            select {
            case <-ensureFetchStarted:
                return true
            default:
                return false
            }
        }, time.Second, time.Millisecond)

        var refreshFetchCalls atomic.Int32
        refreshDone := make(chan error, 1)
        go func() {
            refreshDone <- refreshCurrent(
                context.Background(), database, func(
                    context.Context,
                ) ([]pricing.ModelPricing, error) {
                    refreshFetchCalls.Add(1)
                    return []pricing.ModelPricing{{
                        ModelPattern: "scheduled-model",
                    }}, nil
                }, now.Add(time.Minute),
            )
        }()

        var refreshErr error
        require.Eventually(t, func() bool {
            select {
            case refreshErr = <-refreshDone:
                return true
            default:
                return false
            }
        }, time.Second, time.Millisecond)
        require.NoError(t, refreshErr)
        assert.Zero(t, refreshFetchCalls.Load())
        scheduledPrice, err := database.GetModelPricing("scheduled-model")
        require.NoError(t, err)
        assert.Nil(t, scheduledPrice)

        releaseEnsureFetch <- struct{}{}
        var ensureErr error
        require.Eventually(t, func() bool {
            select {
            case ensureErr = <-ensureDone:
                return true
            default:
                return false
            }
        }, time.Second, time.Millisecond)
        require.NoError(t, ensureErr)
        ensuredPrice, err := database.GetModelPricing("ensure-model")
        require.NoError(t, err)
        require.NotNil(t, ensuredPrice)
    }
    ```

    The real SQLite database and persisted models are the observable contract.
    This test fails if `RefreshCurrent` waits for the first fetch, invokes its
    own fetch, or writes a second catalog response.

- [ ] **Step 2: Run the focused test and verify the red state**

    Run:

    ```bash
    go test ./internal/pricingrefresh \
      -run '^TestRefreshCurrentSkipsWhileEnsureCurrentInFlight$' -shuffle=on
    ```

    Expected: FAIL because the scheduled fetch runs while `EnsureCurrent` is
    blocked, so `refreshFetchCalls` is `1` and `scheduled-model` is persisted.

- [ ] **Step 3: Add the reference-counted per-database gate**

    Add `sync` to `internal/pricingrefresh/refresh.go`, then define the registry
    beside the refresh constants:

    ```go
    type refreshGate struct {
        slot chan struct{}
        refs int
    }

    var currentRefreshGates = struct {
        sync.Mutex
        byDatabase map[*db.DB]*refreshGate
    }{
        byDatabase: make(map[*db.DB]*refreshGate),
    }

    func retainRefreshGate(database *db.DB) *refreshGate {
        currentRefreshGates.Lock()
        defer currentRefreshGates.Unlock()

        gate := currentRefreshGates.byDatabase[database]
        if gate == nil {
            gate = &refreshGate{slot: make(chan struct{}, 1)}
            gate.slot <- struct{}{}
            currentRefreshGates.byDatabase[database] = gate
        }
        gate.refs++
        return gate
    }

    func releaseRefreshGateReference(database *db.DB, gate *refreshGate) {
        currentRefreshGates.Lock()
        defer currentRefreshGates.Unlock()

        gate.refs--
        if gate.refs == 0 {
            delete(currentRefreshGates.byDatabase, database)
        }
    }
    ```

    The reference count changes only while the registry mutex is held. It is
    incremented before acquisition so a holder, waiter, and new caller cannot
    split across different gates for the same database.

- [ ] **Step 4: Acquire the gate according to caller semantics**

    At the start of `runCurrent`, before reading refresh metadata, retain the
    database gate. For forced scheduled calls, try to receive the gate token
    without blocking. For ordinary calls, wait for the token or context
    cancellation:

    ```go
    gate := retainRefreshGate(database)
    if force {
        select {
        case <-gate.slot:
        default:
            releaseRefreshGateReference(database, gate)
            return nil
        }
    } else {
        select {
        case <-gate.slot:
        case <-ctx.Done():
            releaseRefreshGateReference(database, gate)
            return ctx.Err()
        }
    }
    defer func() {
        gate.slot <- struct{}{}
        releaseRefreshGateReference(database, gate)
    }()
    ```

    Return the token before decrementing the holder's reference. That ordering
    lets a newly retained caller acquire the same gate before the registry can
    delete its entry. Leave the remainder of `runCurrent` unchanged so the
    established lifecycle runs only after acquisition.

- [ ] **Step 5: Run the focused and package tests and verify the green state**

    Run:

    ```bash
    go test ./internal/pricingrefresh \
      -run '^TestRefreshCurrentSkipsWhileEnsureCurrentInFlight$' -shuffle=on
    go test ./internal/pricingrefresh -shuffle=on
    ```

    Expected: both commands pass. The regression test completes the scheduled call
    before releasing the blocked ordinary fetch, records zero scheduled fetches,
    and persists only `ensure-model`.

- [ ] **Step 6: Format and run repository validation**

    Run:

    ```bash
    go fmt ./...
    go vet ./...
    make lint-ci
    make test-short
    make test
    ```

    Expected: formatting leaves only the intended Go diff, and vet, lint, the
    short suite, and the full Go suite all pass.

- [ ] **Step 7: Commit the implementation**

    Review the diff, stage only the pricing refresh implementation and its
    regression test, then commit with a substantive rationale body:

    ```bash
    git add internal/pricingrefresh/refresh.go \
      internal/pricingrefresh/refresh_test.go
    git commit -m "fix(pricing): serialize catalog refreshes"
    ```

    Do not push, amend, rebase, resolve review threads, or post comments without
    explicit user authorization.
