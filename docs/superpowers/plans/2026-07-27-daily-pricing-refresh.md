# Daily Pricing Catalog Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the writable agentsview daemon attempt to refresh its live model
pricing catalog every 24 hours for as long as it remains running.

**Architecture:** Keep the existing immediate startup seed and refresh. Add a
single daemon-owned scheduler with a 24-hour ticker, a context-cancellable loop,
and the existing context-aware pricing refresh lifecycle. Separate the loop from
ticker construction so a deterministic test can inject ticks without sleeping.

**Tech Stack:** Go, `context`, `time`, the existing `internal/pricingrefresh`
package, and Testify.

## Global Constraints

- Keep startup's immediate background pricing refresh unchanged.
- Use one constant-memory ticker loop and never overlap refresh attempts.
- Stop promptly when the daemon context is cancelled.
- Log transient refresh failures and continue with the next scheduled attempt.
- Do not add configuration, dependencies, compatibility paths, or backend schema
  changes.

______________________________________________________________________

### Task 1: Schedule daily daemon pricing refreshes

**Files:**

- Create: `cmd/agentsview/pricing_schedule.go`
- Create: `cmd/agentsview/pricing_schedule_test.go`
- Modify: `cmd/agentsview/main.go:441-450`

**Interfaces:**

- Consumes: `pricingrefresh.EnsureCurrent(context.Context, *db.DB) error` and
  the writable daemon's lifetime context and SQLite database.

- Produces: `startPeriodicPricingRefresh(context.Context, *db.DB)` for startup
  wiring and
  `runPeriodicPricingRefresh(context.Context, <-chan time.Time, func(context.Context) error)`
  as the deterministic scheduling unit.

- [ ] **Step 1: Write the failing scheduler behavior test**

    Create `cmd/agentsview/pricing_schedule_test.go` with a controlled tick
    channel. The refresh callback returns an error on the first call and
    succeeds on the second. Assert with Testify that both ticks invoke the
    callback, then cancel the context and require the loop to return:

    ```go
    func TestRunPeriodicPricingRefreshContinuesAfterFailureAndStopsOnCancel(
        t *testing.T,
    ) {
        ctx, cancel := context.WithCancel(context.Background())
        ticks := make(chan time.Time, 2)
        done := make(chan struct{})
        var attempts atomic.Int32

        go func() {
            runPeriodicPricingRefresh(ctx, ticks, func(context.Context) error {
                if attempts.Add(1) == 1 {
                    return errors.New("temporary pricing failure")
                }
                return nil
            })
            close(done)
        }()

        ticks <- time.Time{}
        require.Eventually(t, func() bool {
            return attempts.Load() == 1
        }, time.Second, time.Millisecond)
        ticks <- time.Time{}
        require.Eventually(t, func() bool {
            return attempts.Load() == 2
        }, time.Second, time.Millisecond)

        cancel()
        require.Eventually(t, func() bool {
            select {
            case <-done:
                return true
            default:
                return false
            }
        }, time.Second, time.Millisecond)
    }
    ```

    This test fails if the scheduler is absent, exits after a transient error, or
    ignores cancellation. The controlled callback is the nondeterministic
    network boundary; the scheduler itself remains the real subject.

- [ ] **Step 2: Run the focused test and verify the red state**

    Run:

    ```bash
    go test ./cmd/agentsview -run TestRunPeriodicPricingRefreshContinuesAfterFailureAndStopsOnCancel -count=1
    ```

    Expected: compilation fails because `runPeriodicPricingRefresh` is not yet
    defined.

- [ ] **Step 3: Implement the minimal periodic scheduler**

    Create `cmd/agentsview/pricing_schedule.go` with:

    ```go
    const periodicPricingRefreshInterval = 24 * time.Hour

    func startPeriodicPricingRefresh(ctx context.Context, database *db.DB) {
        ticker := time.NewTicker(periodicPricingRefreshInterval)
        defer ticker.Stop()
        runPeriodicPricingRefresh(ctx, ticker.C, func(ctx context.Context) error {
            return pricingrefresh.EnsureCurrent(ctx, database)
        })
    }

    func runPeriodicPricingRefresh(
        ctx context.Context,
        ticks <-chan time.Time,
        refresh func(context.Context) error,
    ) {
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticks:
                if err := refresh(ctx); err != nil && ctx.Err() == nil {
                    log.Printf("pricing refresh: %v", err)
                }
            }
        }
    }
    ```

    In `cmd/agentsview/main.go`, immediately after `seedPricing(database)`, start
    the scheduler with:

    ```go
    go startPeriodicPricingRefresh(ctx, database)
    ```

- [ ] **Step 4: Run the focused test and package tests**

    Run:

    ```bash
    go test ./cmd/agentsview -run TestRunPeriodicPricingRefreshContinuesAfterFailureAndStopsOnCancel -count=1
    go test ./cmd/agentsview -count=1
    ```

    Expected: both commands pass with no race, timeout, or leaked scheduler.

- [ ] **Step 5: Format and run repository-required Go validation**

    Run:

    ```bash
    go fmt ./...
    go vet ./...
    make test-short
    ```

    Expected: formatting produces no unintended diff, vet passes, and the short
    test suite passes.

- [ ] **Step 6: Commit the implementation**

    Stage only the scheduler, its test, and startup wiring, then commit:

    ```bash
    git add cmd/agentsview/pricing_schedule.go \
      cmd/agentsview/pricing_schedule_test.go cmd/agentsview/main.go
    git commit -m "fix(pricing): refresh catalog daily"
    ```

- [ ] **Step 7: Push and open the pull request**

    Push `feat/daily-pricing-refresh` to `origin` and open a PR whose title and
    summary explain that long-running daemons otherwise leave newly released
    models unpriced until restart. Do not merge the PR or poll its checks.
