# Full-Sync Memory Bound Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for
> tracking.

**Goal:** Bound parsed-session memory during full sync while keeping median
wall-clock time within 10% of v0.42.0 on both bulk benchmark fixtures.

**Architecture:** Replace the unthrottled archive-pass admission object with a
dedicated byte-weighted budget. Keep each lease from immediately before parse
through the corresponding database write, and use the existing pressure signal
to flush partial batches. Select the internal capacity from measured 64 MiB, 128
MiB, and 256 MiB candidates; do not expose configuration or compatibility paths.

**Tech Stack:** Go 1.27, `golang.org/x/sync/semaphore`, SQLite/FTS5, Go
benchmarks, Linux user-systemd cgroup measurements, GitHub CLI.

## Global Constraints

- Use only generated synthetic archives for tracked tests and candidate
  profiling. Keep corpus paths, stable identifiers, observations, databases,
  profiles, and machine details out of Git and the pull request.
- Treat peak memory and median wall-clock time as co-equal. Reject a candidate
  when either benchmark fixture is more than 10% slower than v0.42.0.
- Keep parsing and database output unchanged. Do not add a user setting,
  environment-controlled production behavior, fallback, or historical parser.
- Preserve the end-of-pass scavenge and make cancellation release waiters and
  retained capacity.
- Use CGO and the `fts5` tag for Go tests. Do not poll GitHub Actions.

______________________________________________________________________

### Task 1: Specify Bounded Bulk Admission in Tests

**Files:**

- Modify: `internal/sync/parse_retention_test.go`

**Interfaces:**

- Consumes: `newBulkParseRetentionBudget`, `Engine.beginBulkRetentionPass`,
  `Engine.startWorkers`, and `Engine.collectAndBatch`.

- Produces: observable contracts for exclusive large-source admission, partial
  bulk flushes, one end scavenge, warm no-op behavior, and cancellation.

- [ ] **Step 1: Replace the unthrottled budget assertion**

    Replace `TestBulkParseRetentionBudgetNeverBlocks` with
    `TestBulkParseRetentionBudgetBoundsConcurrentSourceWeight`. Construct a 64
    MiB bulk budget, acquire one source whose weight consumes the capacity, and
    start a second acquisition in a goroutine. Assert that the second
    acquisition reports pressure and does not complete until the first lease is
    released.

- [ ] **Step 2: Make the full-pass test require a weighted budget**

    Rename `TestFullSyncPassIsUnthrottledAndScavengesOnce` to
    `TestFullSyncPassUsesBoundedRetentionAndScavengesOnce`. Assert that the
    installed bulk budget has a non-nil weighted semaphore and the configured
    bulk capacity, while retaining the existing parse-bearing and warm-pass
    scavenge assertions.

- [ ] **Step 3: Exercise admission-pressure flushing in both write modes**

    Convert `TestStartWorkersFlushesBelowBatchUnderAdmissionPressure` to a table
    over `syncWriteDefault` and `syncWriteBulk`. For the bulk case, install a
    bulk retention pass around the two-worker run. Keep the two synthetic 20 MiB
    files and require two writes: the first result must flush below `batchSize`
    before the second exclusive parse can start.

- [ ] **Step 4: Run the focused tests and record the expected failure**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
      -run '^(TestFullSyncPassUsesBoundedRetentionAndScavengesOnce|TestBulkParseRetentionBudgetBoundsConcurrentSourceWeight|TestStartWorkersFlushesBelowBatchUnderAdmissionPressure)$' \
      -count=1
    ```

    Expected: the new bulk-budget tests fail or time out under the current
    unthrottled implementation, while the default-mode table case still passes.

### Task 2: Implement the Dedicated Weighted Bulk Budget

**Files:**

- Modify: `internal/sync/parse_retention.go`
- Modify: `internal/sync/engine.go`
- Modify: `internal/sync/parse_retention_test.go`

**Interfaces:**

- Consumes: the existing weighted admission, pressure channel, collector flush,
  lease release, and end-of-pass scavenge machinery.

- Produces: a bounded internal budget for full sync, resync rebuild, and remote
  import processing.

- [ ] **Step 1: Add the internal bulk capacity**

    Add `defaultBulkParseRetentionBytes` beside `defaultParseRetentionBytes`.
    Start at 64 MiB for correctness tests; the host measurements in Task 4
    select the final value.

- [ ] **Step 2: Build bulk admission on the weighted implementation**

    Change `newBulkParseRetentionBudget` to accept a capacity and return
    `newParseRetentionBudget(capacity)` with a bulk-only flag that requests one
    scavenge for every successful parse acquisition. Remove the nil-semaphore
    fast path from `acquire`; every budget must calculate a weight, signal
    pressure when blocked, and return a releasing lease.

- [ ] **Step 3: Preserve bulk scavenge semantics**

    Update `noteKnownLargeSource` so the bulk-only flag sets `scavengePending` for
    any parse-bearing pass, while the default budget still requests a scavenge
    only for known sources at least 16 MiB. A warm pass must leave
    `scavengePending` false.

- [ ] **Step 4: Install the bounded bulk budget**

    Update `beginBulkRetentionPass` to construct the bulk budget with
    `defaultBulkParseRetentionBytes`. Rewrite its comments and the corresponding
    `Engine` field comment to describe byte-bounded admission and partial-batch
    pressure flushing rather than full parallelism.

- [ ] **Step 5: Run the focused tests green**

    Run:

    ```bash
    gofmt -w internal/sync/parse_retention.go \
      internal/sync/parse_retention_test.go internal/sync/engine.go
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
      -run '^(TestWarmNoopSyncAcquiresNoRetentionLeases|TestFullSyncPassUsesBoundedRetentionAndScavengesOnce|TestScopedSyncKeepsBoundedRetentionBudget|TestBulkParseRetentionBudgetBoundsConcurrentSourceWeight|TestBulkParseRetentionBudgetScavengesOncePerParseBearingPass|TestParseRetentionBudgetBoundsConcurrentSourceWeight|TestParseRetentionBudgetRunsOversizedSourceExclusively|TestCollectAndBatchRetainsParseLeaseThroughWrite|TestStartWorkersFlushesBelowBatchUnderAdmissionPressure|TestStartWorkersCancellationReleasesAdmissionWaiters)$' \
      -count=1
    ```

    Expected: all selected retention, batching, scavenge, and cancellation tests
    pass.

- [ ] **Step 6: Commit the correctness change**

    Stage only the three sync files and commit with a message explaining that full
    sync previously retained worker results plus a 100-session batch, and that
    weighted admission releases capacity only after write completion.

### Task 3: Check Broader Correctness Before Profiling

**Files:**

- Verify: `internal/sync/`

**Interfaces:**

- Consumes: all sync-engine behavior that shares the retention budget.

- Produces: a candidate safe enough for repeated local and remote profiling.

- [ ] **Step 1: Run the full sync package**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync -count=1
    ```

    Expected: the package passes without races, timeouts, or changed write counts.

- [ ] **Step 2: Run static checks**

    Run:

    ```bash
    make pricing-snapshot
    gofmt -w internal/sync/parse_retention.go \
      internal/sync/parse_retention_test.go internal/sync/engine.go
    go vet -tags fts5 ./internal/sync/...
    git diff --check
    ```

    Expected: all commands pass and no unrelated file changes appear.

### Task 4: Select Capacity With Paired Memory and Time Measurements

**Files:**

- Temporarily modify, then restore or finalize:
  `internal/sync/parse_retention.go`
- Record aggregate results in the pull request draft only after scrubbing.

**Interfaces:**

- Consumes: v0.42.0 and candidate builds at 64 MiB, 128 MiB, and 256 MiB.

- Produces: the lowest-memory capacity whose median wall time stays within 10%
  of v0.42.0 for both gated fixtures.

- [ ] **Step 1: Prepare isolated candidate builds**

    Build v0.42.0 and each capacity from separate disposable source copies. Do not
    install over a live binary, write to a live database, or capture private
    corpus data. Verify each binary or test executable identifies the intended
    revision and capacity before running it.

- [ ] **Step 2: Run the two synthetic benchmark paths locally**

    Run both benchmarks with fixed fixtures before using the remote machine:

    ```bash
    AGENTSVIEW_BENCH_SYNC_SESSIONS=120 \
    AGENTSVIEW_BENCH_SYNC_MESSAGES=5000 \
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
      -run '^$' \
      -bench '^(BenchmarkResyncBulkIngest|BenchmarkSyncAllColdArchive)$' \
      -benchtime=1x -count=1
    ```

    Expected: both paths complete and validate all 120 sessions.

- [ ] **Step 3: Alternate baseline and candidate runs on the 16 GB machine**

    For each capacity and for both 120x5,000 and 120x10,000 fixtures, run at least
    five measured baseline runs and five measured candidate runs in alternating
    order. Put each run in an isolated user-owned cgroup. Capture elapsed time,
    `memory.peak`, sampled anonymous memory, and sampled file cache. Keep raw
    outputs and scratch databases on the machine.

- [ ] **Step 4: Apply the selection rule**

    Compute the median wall time and median/maximum cgroup peak for each
    revision-fixture pair. Reject a capacity when either candidate median is
    more than 10% slower than the matching v0.42.0 median. Among survivors,
    select the capacity with the lowest peak. If none survives, stop and revise
    the design rather than weakening the gate.

- [ ] **Step 5: Finalize and reverify the selected constant**

    Set `defaultBulkParseRetentionBytes` to the selected value, remove every
    temporary profiling edit, rerun the Task 2 focused tests, and inspect the
    diff. Commit the capacity decision with aggregate memory and timing evidence
    in the commit body.

### Task 5: Run the Complete Authorized Benchmark

**Files:**

- No tracked benchmark data or machine configuration changes.

**Interfaces:**

- Consumes: the final candidate and the existing operator-authorized benchmark
  workload on the generic 16 GB machine.

- Produces: the acceptance result for the below-8-GiB complete-workload target.

- [ ] **Step 1: Run through the existing polled host harness**

    Use the existing authorized benchmark mechanism. Do not add an inbound
    webhook, self-hosted GitHub runner, credential, service change, or permanent
    host installation. Ask the operator for the one necessary action if the
    current SSH account cannot start the private workload.

- [ ] **Step 2: Verify the final operational gate**

    Require the complete workload to stay below 8 GiB cgroup peak and complete
    successfully. Compare wall time to the last valid baseline. Keep all raw
    corpus-linked measurements private; retain only aggregate peak and elapsed
    values for review.

### Task 6: Final Verification, Scrub, Push, and Pull Request

**Files:**

- Verify every file changed on `fix-full-sync-memory`.
- Create: one public pull request against `main`.

**Interfaces:**

- Consumes: the approved design, implementation commits, unit evidence, paired
  benchmark evidence, and complete-workload evidence.

- Produces: one sanitized reviewable pull request; no merge.

- [ ] **Step 1: Run repository verification**

    Run:

    ```bash
    make pricing-snapshot
    gofmt -w internal/sync/parse_retention.go \
      internal/sync/parse_retention_test.go internal/sync/engine.go
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync -count=1
    go vet -tags fts5 ./internal/sync/...
    git diff --check origin/main...HEAD
    ```

    Expected: every command passes on the final commit candidate.

- [ ] **Step 2: Review and scrub everything leaving private context**

    Inspect the full commit range, diff, commit messages, new blob contents, and
    proposed pull-request title/body. Reject private hostnames, absolute paths,
    corpus identifiers, source paths, profiles, credentials, and stable session
    identifiers. Refer only to a generic 16 GB machine and aggregate synthetic
    or complete-workload measurements.

- [ ] **Step 3: Commit any final documentation synchronization**

    If the selected capacity or evidence changes the design document, update it so
    it describes the final implementation, format it, scrub it, and create a new
    commit. Do not amend, squash, or rebase existing commits.

- [ ] **Step 4: Push and open one pull request**

    Push `fix-full-sync-memory`, then open a pull request against `main`. Keep the
    title and body synchronized with the current diff. Explain the retained
    pipeline, weighted admission, measured memory reduction, and wall-clock
    result in plain language. Do not include Test Plan, Validation, or
    Verification sections, do not post a PR comment, and do not merge.

- [ ] **Step 5: Report the exact handoff**

    Provide the PR link, selected capacity, focused/full test commands and
    outcomes, paired median wall times, memory peaks, and complete-workload
    result. Clearly distinguish verified results from any remaining operational
    limitation.
