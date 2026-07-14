# Daemon-Owned Embedding ETA Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve embedding throughput and ETA across Settings page loads by
calculating one estimate in daemon memory and publishing it in build status.

**Architecture:** Add a small unexported EWMA estimator to the vector build
manager and update it from the existing progress callback under the manager
lock. Extend the additive status contract with readiness, rate, and remaining
milliseconds; the Svelte component becomes a pure renderer of those fields.

**Tech Stack:** Go, Huma/OpenAPI, Svelte 5, TypeScript, testify, Vitest

## Global Constraints

- Store estimator state only in daemon memory; do not change SQLite,
  `vectors.db`, migrations, or configuration.
- Use smoothing factor `0.3` and require two positive progress samples.
- Reset on build start, phase change, total change, or counter regression.
- Do not retain a browser-side compatibility estimator.
- Preserve elapsed-time display based on `started_at`.

______________________________________________________________________

### Task 1: In-memory build estimator and status contract

**Files:**

- Create: `internal/vector/eta.go`
- Create: `internal/vector/eta_test.go`
- Modify: `internal/vector/manager.go`
- Modify: `internal/vector/manager_test.go`

**Interfaces:**

- Produces:
  `buildETAEstimator.sample(phase string, done, total int64, observedAt time.Time) buildETAEstimate`

- Produces: `BuildStatus.EstimateReady bool`,
  `BuildStatus.RatePerSecond float64`, and `BuildStatus.ETAMilliseconds int64`

- Consumes: `Manager.now()` as a monotonic observation clock and the existing
  `BuildProgress` callback stream

- [ ] **Step 1: Write failing estimator tests**

    Add table-driven/testify coverage for baseline plus two positive samples,
    stalled-time inclusion, phase and denominator resets, counter regression,
    unknown totals, and `reset`. A representative ready assertion is:

    ```go
    est.sample("embedding", 0, 1000, base)
    est.sample("embedding", 100, 1000, base.Add(2*time.Second))
    got := est.sample("embedding", 200, 1000, base.Add(4*time.Second))
    require.True(t, got.Ready)
    assert.InDelta(t, 50, got.RatePerSecond, 0.001)
    assert.Equal(t, 16*time.Second, got.Remaining)
    ```

- [ ] **Step 2: Run the focused tests and confirm RED**

    Run:
    `CGO_ENABLED=1 go test -tags fts5 ./internal/vector -run 'TestBuildETA|TestManagerStatusPublishesETA'`

    Expected: compilation fails because the estimator and status fields do not
    exist.

- [ ] **Step 3: Implement the estimator**

    Define focused private types in `eta.go`:

    ```go
    type buildETAEstimate struct {
        Ready         bool
        RatePerSecond float64
        Remaining     time.Duration
    }

    type buildETAEstimator struct {
        phase           string
        lastDone        int64
        lastTotal       int64
        lastObservedAt  time.Time
        ewmaRatePerNano float64
        positiveSamples int
        initialized     bool
    }
    ```

    Reset and establish a baseline on identity changes. Fold positive deltas with
    `0.3*instantaneous + 0.7*previous`, keep zero-delta observations out of the
    baseline, require two samples, and reject non-positive/non-finite output.

- [ ] **Step 4: Wire estimates into manager status**

    Add the three JSON fields with `omitempty` to `BuildStatus`. Reset the private
    estimator in `begin` and `finish`; in `reportProgress`, sample with
    `m.now()` and copy either a ready estimate or zero values into status.

- [ ] **Step 5: Add manager-level regression coverage**

    Use a controllable `m.now` and direct progress reports after `m.begin()` to
    prove a ready estimate appears in `Status()` and that `m.finish(...)` clears
    it. Assert model/dimension and existing lifecycle fields remain intact.

- [ ] **Step 6: Run focused and package tests**

    Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/vector`

    Expected: PASS.

### Task 2: Generated API and server-owned UI rendering

**Files:**

- Modify: `frontend/src/lib/api/generated/models/VectorBuildStatus.ts`
- Modify: `frontend/src/lib/components/settings/EmbeddingsSettings.svelte`
- Modify: `frontend/src/lib/components/settings/EmbeddingsSettings.test.ts`
- Delete: `frontend/src/lib/utils/etaEstimator.ts`
- Delete: `frontend/src/lib/utils/etaEstimator.test.ts`

**Interfaces:**

- Consumes: optional generated fields `estimate_ready`, `rate_per_second`, and
  `eta_milliseconds`

- Produces: immediate Settings rendering from the first warmed daemon response

- [ ] **Step 1: Rewrite component coverage to expect server estimates**

    Make the initial mocked running status ready:

    ```ts
    runningStatus(200, {
      estimate_ready: true,
      rate_per_second: 50,
      eta_milliseconds: 16_000,
    })
    ```

    Assert the first settled render contains `50 chunks/s` and `ETA 16s`.
    Separately assert a response with `estimate_ready: false` renders the
    existing estimating message. Remove tests for client-side build-key resets.

- [ ] **Step 2: Run the focused component test and confirm RED**

    Run:
    `npm test -- --run src/lib/components/settings/EmbeddingsSettings.test.ts`
    from `frontend/`.

    Expected: type/render failures because the generated status and component do
    not consume the server fields yet.

- [ ] **Step 3: Regenerate the API client**

    Run: `npm run generate:api` from `frontend/`. Keep only changes caused by the
    additive `BuildStatus` fields; inspect and exclude unrelated generator
    drift.

- [ ] **Step 4: Remove the browser estimator**

    Delete the estimator import, instance, state, sampling function, and reset
    calls. Derive display values directly:

    ```ts
    const estimateReady = $derived(status?.estimate_ready ?? false);
    const ratePerSecond = $derived(status?.rate_per_second ?? null);
    const etaMs = $derived(status?.eta_milliseconds ?? null);
    ```

    Retain `elapsedMs` updates from `started_at` and existing polling behavior.
    Delete the now-unused estimator source and unit test.

- [ ] **Step 5: Run frontend verification**

    Run from `frontend/`:

    ```bash
    npm test -- --run src/lib/components/settings/EmbeddingsSettings.test.ts
    npm run check
    ```

    Expected: PASS.

### Task 3: Repository verification and delivery commit

**Files:**

- Modify only files named in Tasks 1 and 2 plus this plan.

**Interfaces:**

- Consumes: complete backend status contract and frontend renderer

- Produces: a verified feature-branch commit ready to push when authorized

- [ ] **Step 1: Format and inspect generated drift**

    Run `go fmt ./...`, then inspect `git status --short`, `git diff --stat`, and
    `git diff --check`. Confirm there are no database, migration, config, or
    unrelated generated changes.

- [ ] **Step 2: Run backend verification**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/vector ./internal/server
    go vet ./...
    ```

    Expected: PASS.

- [ ] **Step 3: Run frontend verification**

    Run from `frontend/`:

    ```bash
    npm test -- --run src/lib/components/settings/EmbeddingsSettings.test.ts
    npm run check
    ```

    Expected: PASS.

- [ ] **Step 4: Commit the implementation**

    Stage only the scoped files and commit with a conventional subject describing
    daemon-owned embedding ETA. Do not bypass hooks, amend the design commit,
    push, or open a pull request without explicit authorization.
