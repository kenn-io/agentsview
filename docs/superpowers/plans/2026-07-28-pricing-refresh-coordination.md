# Pricing Refresh Coordination Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan directly in the current agent, task-by-task. Never use
> subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent scheduled and push-triggered pricing refreshes from fetching
and writing concurrently or losing scheduled writes across a resync database
swap.

**Approved spec/design:**
`docs/superpowers/specs/2026-07-28-pricing-refresh-coordination-design.md`

**Architecture:** Add a package-local registry in `internal/pricingrefresh` that
maps each `*db.DB` to a capacity-one channel gate and a reference count.
Ordinary refreshes wait for the database gate with context cancellation, while
forced scheduled refreshes attempt the gate without blocking and return
successfully when another refresh is already in flight. At the daemon layer, run
each scheduled refresh inside the sync engine's exclusive barrier, with a direct
fallback when `--no-sync` leaves the daemon without an engine.

**Tech Stack:** Go, `context`, `sync`, channels, the existing SQLite pricing
catalog, and Testify.

## Global Constraints

- Allow at most one current-catalog refresh per database at a time.
- Preserve the one-hour cooldown for ordinary `EnsureCurrent` calls.
- Make scheduled `RefreshCurrent` calls skip immediately on contention.
- Keep different `*db.DB` instances independent.
- Remove registry entries when their reference count reaches zero.
- Serialize the full scheduled refresh lifecycle with sync and resync swaps.
- Preserve a direct scheduled refresh path when syncing is disabled.
- Preserve existing fetch, fallback, attempt-metadata, error, and cancellation
  restoration behavior after gate acquisition.
- Add no dependencies, schema changes, cross-process coordination, or queued
  scheduled refresh.

______________________________________________________________________

### Task 1: Serialize current pricing refreshes per database

**Status:** Completed by `57a39ed70`.

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

- [x] **Step 1: Write the failing concurrent-refresh regression test**

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

- [x] **Step 2: Run the focused test and verify the red state**

    Run:

    ```bash
    go test ./internal/pricingrefresh \
      -run '^TestRefreshCurrentSkipsWhileEnsureCurrentInFlight$' -shuffle=on
    ```

    Expected: FAIL because the scheduled fetch runs while `EnsureCurrent` is
    blocked, so `refreshFetchCalls` is `1` and `scheduled-model` is persisted.

- [x] **Step 3: Add the reference-counted per-database gate**

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

- [x] **Step 4: Acquire the gate according to caller semantics**

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

- [x] **Step 5: Run the focused and package tests and verify the green state**

    Run:

    ```bash
    go test ./internal/pricingrefresh \
      -run '^TestRefreshCurrentSkipsWhileEnsureCurrentInFlight$' -shuffle=on
    go test ./internal/pricingrefresh -shuffle=on
    ```

    Expected: both commands pass. The regression test completes the scheduled call
    before releasing the blocked ordinary fetch, records zero scheduled fetches,
    and persists only `ensure-model`.

- [x] **Step 6: Format and run repository validation**

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

- [x] **Step 7: Commit the implementation**

    Review the diff, stage only the pricing refresh implementation and its
    regression test, then commit with a substantive rationale body:

    ```bash
    git add internal/pricingrefresh/refresh.go \
      internal/pricingrefresh/refresh_test.go
    git commit -m "fix(pricing): serialize catalog refreshes"
    ```

    Do not push, amend, rebase, resolve review threads, or post comments without
    explicit user authorization.

______________________________________________________________________

### Task 2: Serialize scheduled refreshes with resync swaps

**Files:**

- Modify: `cmd/agentsview/pricing_schedule.go:14-31`
- Modify: `cmd/agentsview/pricing_schedule_test.go:1-100`
- Modify: `cmd/agentsview/main.go:449-450`

**Interfaces:**

- Consumes: `(*sync.Engine).RunExclusive(func() error) error`,
  `(*sync.Engine).SwapResyncDatabase(string) error`, and
  `(*sync.Engine).ResetCachesAfterSwap() error`.

- Produces: package-local
  `pricingRefreshExclusiveRunner { RunExclusive(func() error) error }`, plus
  `startPeriodicPricingRefresh(context.Context, *db.DB, pricingRefreshExclusiveRunner)`
  and
  `runPeriodicPricingRefresh(context.Context, <-chan time.Time, *db.DB, pricingRefreshExclusiveRunner)`.

- [ ] **Step 1: Write the failing staged-swap scheduler regression**

    Import Testify's `assert` package and the sync package as `agentsync` in
    `cmd/agentsview/pricing_schedule_test.go`. Update the existing direct-path
    test to pass a nil runner:

    ```go
    runPeriodicPricingRefresh(ctx, ticks, database, nil)
    ```

    Add this regression after
    `TestRunPeriodicPricingRefreshFetchesAfterRecentAttempt`:

    ```go
    func TestRunPeriodicPricingRefreshWaitsForResyncSwap(t *testing.T) {
        database := dbtest.OpenTestDB(t)
        engine := agentsync.NewEngine(database, agentsync.EngineConfig{})
        t.Cleanup(engine.Close)
        dbtest.EnsureTestDBAt(t, engine.ResyncTempPath())

        swapEntered := make(chan struct{})
        releaseSwap := make(chan struct{}, 1)
        swapDone := make(chan error, 1)
        go func() {
            swapDone <- engine.RunExclusive(func() error {
                close(swapEntered)
                <-releaseSwap
                if err := engine.SwapResyncDatabase(
                    engine.ResyncTempPath(),
                ); err != nil {
                    return err
                }
                return engine.ResetCachesAfterSwap()
            })
        }()
        defer func() {
            select {
            case releaseSwap <- struct{}{}:
            default:
            }
        }()
        require.Eventually(t, func() bool {
            select {
            case <-swapEntered:
                return true
            default:
                return false
            }
        }, time.Second, time.Millisecond)

        requests := make(chan *http.Request, 1)
        originalTransport := http.DefaultTransport
        http.DefaultTransport = pricingCatalogTransport{requests: requests}
        t.Cleanup(func() {
            http.DefaultTransport = originalTransport
        })

        ctx, cancel := context.WithCancel(context.Background())
        ticks := make(chan time.Time, 1)
        refreshDone := make(chan struct{})
        go func() {
            runPeriodicPricingRefresh(ctx, ticks, database, engine)
            close(refreshDone)
        }()
        t.Cleanup(func() {
            cancel()
            require.Eventually(t, func() bool {
                select {
                case <-refreshDone:
                    return true
                default:
                    return false
                }
            }, time.Second, time.Millisecond)
        })

        ticks <- time.Now()
        assert.Never(t, func() bool {
            return len(requests) > 0
        }, 50*time.Millisecond, time.Millisecond)

        releaseSwap <- struct{}{}
        var swapErr error
        require.Eventually(t, func() bool {
            select {
            case swapErr = <-swapDone:
                return true
            default:
                return false
            }
        }, time.Second, time.Millisecond)
        require.NoError(t, swapErr)
        require.Eventually(t, func() bool {
            price, err := database.GetModelPricing("scheduled-model")
            return err == nil && price != nil
        }, time.Second, time.Millisecond)
    }
    ```

    The staged replacement is a real current-schema SQLite archive and the engine
    performs the actual swap. Removing the scheduler's `RunExclusive` call makes
    the request occur before release and leaves `scheduled-model` absent from
    the swapped archive.

- [ ] **Step 2: Run the focused test and verify the red state**

    Run:

    ```bash
    go test ./cmd/agentsview \
      -run '^TestRunPeriodicPricingRefreshWaitsForResyncSwap$' -shuffle=on
    ```

    Expected: compilation fails because `runPeriodicPricingRefresh` does not yet
    accept the exclusive runner required by the regression.

- [ ] **Step 3: Wrap scheduled refreshes in the optional exclusive runner**

    Add the narrow interface to `cmd/agentsview/pricing_schedule.go`:

    ```go
    type pricingRefreshExclusiveRunner interface {
        RunExclusive(func() error) error
    }
    ```

    Add a `runner pricingRefreshExclusiveRunner` parameter to
    `startPeriodicPricingRefresh` and `runPeriodicPricingRefresh`, pass it
    through, and replace the refresh callback with:

    ```go
    runPricingRefreshLoop(ctx, ticks, func(ctx context.Context) error {
        refresh := func() error {
            return pricingrefresh.RefreshCurrent(ctx, database)
        }
        if runner == nil {
            return refresh()
        }
        return runner.RunExclusive(refresh)
    })
    ```

    This places the engine barrier outside the pricing package gate and covers the
    entire metadata-read, fetch, and catalog-write lifecycle.

- [ ] **Step 4: Wire the daemon engine without a typed-nil interface**

    In `cmd/agentsview/main.go`, replace the scheduler startup with an explicit
    interface assignment so a nil `*sync.Engine` does not become a non-nil
    interface value:

    ```go
    var pricingRefreshRunner pricingRefreshExclusiveRunner
    if engine != nil {
        pricingRefreshRunner = engine
    }
    go startPeriodicPricingRefresh(ctx, database, pricingRefreshRunner)
    ```

    Syncing daemons now share the resync barrier; `--no-sync` daemons pass a true
    nil interface and retain the direct refresh path covered by the existing
    scheduler test.

- [ ] **Step 5: Run focused scheduler tests in the green state**

    Run:

    ```bash
    go test ./cmd/agentsview \
      -run 'TestRunPeriodicPricingRefresh' -shuffle=on
    go test -race ./cmd/agentsview \
      -run 'TestRunPeriodicPricingRefresh' -shuffle=on
    ```

    Expected: both commands pass. The swap regression makes no request while the
    engine barrier is held, then persists `scheduled-model` after the swap; the
    existing test still fetches through the nil-runner path.

- [ ] **Step 6: Format and run repository validation**

    Run:

    ```bash
    go fmt ./...
    go vet ./...
    make lint-ci
    make test-short
    make test
    ```

    Expected: formatting changes only the intended Go files, and vet, lint, the
    short suite, and the full Go suite all pass.

- [ ] **Step 7: Commit and push the fix**

    Review the complete diff, stage only the scheduler, startup wiring, and
    scheduler tests, then commit:

    ```bash
    git add cmd/agentsview/main.go cmd/agentsview/pricing_schedule.go \
      cmd/agentsview/pricing_schedule_test.go
    git commit -m "fix(pricing): coordinate refresh with resync"
    git push origin HEAD:feat/daily-pricing-refresh
    ```

    The user explicitly authorized this push to PR #1285. Do not amend, rebase,
    resolve review threads, post comments, or merge the pull request.
