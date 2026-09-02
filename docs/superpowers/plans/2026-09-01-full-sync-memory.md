# Full-Sync Memory Bound Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan task-by-task.

**Goal:** Bound parsed-session memory during full sync while keeping wall-clock
time within 10% of v0.42.0 on a production-scale workload.

**Architecture:** Bound active and queued parses with a 128 MiB weighted
semaphore. Transfer completed archive results to a separately bounded 512 MiB
pending-write stage and release parse admission at that ownership boundary.
Preserve the existing 100-session transaction batch when the pending byte bound
allows it. Clear completed pointer-bearing collector slices before reuse.

**Tech Stack:** Go 1.27, `golang.org/x/sync/semaphore`, SQLite/FTS5, Go
benchmarks, Linux cgroup measurements, GitHub CLI.

## Global constraints

- Use generated synthetic archives for tracked tests. Keep corpus paths, stable
  identifiers, observations, databases, profiles, and machine details out of
  Git and the pull request.
- Treat process memory and wall-clock time as co-equal. Reject a candidate that
  is more than 10% slower than v0.42.0.
- Record process heap, resident set size (RSS), anonymous memory, and file cache
  separately. Do not treat reclaimable cache as a Go heap leak.
- Keep parsing and database output unchanged. Do not add a user setting,
  environment-controlled behavior, fallback, or historical parser.
- Preserve the end-of-pass scavenge and make cancellation release waiters and
  retained capacity.
- Use CGO and the `fts5` tag for Go tests. Do not poll GitHub Actions.

______________________________________________________________________

## Task 1: Reproduce and locate retention

**Files:**

- Inspect: `internal/sync/engine.go`
- Inspect: `internal/sync/parse_retention.go`
- Modify: `internal/sync/parse_retention_test.go`

1. Reproduce high heap during a synthetic archive-scale pass.
1. Profile the pipeline boundaries: parser result, queued result, prepared
   database rows, database write, and end-of-pass scavenge.
1. Add a behavior test that keeps the collector alive after a write and proves a
   flushed parsed payload becomes collectible.
1. Confirm whether memory remains live after the existing final scavenge.

Expected finding: concurrent parse results and a pending 100-session write batch
are retained at the same time, prepared rows amplify that payload, and stale
collector backing-array pointers extend completed-batch lifetimes. The final
scavenge returns the released heap, so the symptom is transient retention rather
than a permanent allocator leak.

## Task 2: Add byte-aware bulk admission

**Files:**

- Modify: `internal/sync/parse_retention.go`
- Modify: `internal/sync/engine.go`
- Modify: `internal/sync/parse_retention_test.go`

1. Replace the unthrottled bulk budget with a weighted semaphore.
1. Estimate retained bytes with the existing fixed allowance plus four times
   source size.
1. Make an oversized source acquire the active capacity exclusively.
1. Count an unknown-size source conservatively rather than admitting it with a
   zero estimate.
1. Preserve one end-of-pass scavenge for each parse-bearing bulk pass, and skip
   it for a warm no-op pass.
1. Verify blocked acquisition and cancellation behavior with focused tests.

## Task 3: Separate parse and pending-write ownership

**Files:**

- Modify: `internal/sync/parse_retention.go`
- Modify: `internal/sync/engine.go`
- Modify: `internal/sync/parse_retention_test.go`

1. Add an internal pending-result capacity separate from the active parse
   capacity.
1. In an archive-scale pass, release the parse lease when the collector
   transfers a result into the pending database batch, independent of the
   database write mode.
1. Flush the existing batch before adding a result that would exceed the pending
   bound. Flush after adding a result that reaches the bound.
1. Keep incremental mode's lease-through-write and admission-pressure behavior
   unchanged.
1. Clear pending writes, leases, and cache writes after every completed or
   cancelled flush.
1. Add behavior tests that prove bulk admission remains available while a
   database write is blocked, pending parsed bytes split batches at the limit,
   and parser pressure does not fragment bulk transactions.

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
  -run '^(TestArchiveCollectorReleasesParseLeaseBeforeWrite|TestBulkCollectorBoundsPendingParsedBytesBetweenWrites|TestStartWorkersKeepsBulkBatchingIndependentOfParseAdmission|TestBulkCollectorReleasesFlushedParsedBatch)$' \
  -count=1
```

## Task 4: Select the capacities with memory and time measurements

1. Build v0.42.0 and each candidate in disposable source copies. Do not install
   over a live binary or point branch code at a live archive.
1. Screen candidates with the generated cold-archive and resync benchmarks.
1. Run the surviving architecture against an isolated production-scale clone on
   a generic 16 GB machine. Keep raw data and diagnostics private.
1. Compare a candidate with bracketing v0.42.0 runs to reduce sensitivity to
   host I/O variation.
1. Reject the shared lease-through-write design if parser backpressure creates
   small database transactions or exceeds the wall-clock gate.
1. Select the separate active and pending capacities that minimize memory while
   preserving normal transaction batching.
1. Complete and seal a fresh archive with the final candidate. Compare heap,
   worker RSS, worker anonymous memory, target anonymous memory, transaction
   count, and elapsed time.

Selection: 128 MiB active parse capacity and 512 MiB pending-result capacity.
The production-scale candidate changed wall time by +0.49% against the midpoint
of bracketing baselines while lowering peak worker anonymous memory by 19.3%.

## Task 5: Check broader correctness

Run:

```bash
gofmt -w internal/sync/engine.go internal/sync/parse_retention.go \
  internal/sync/parse_retention_test.go
CGO_ENABLED=1 go test -tags fts5 ./internal/sync -count=1
CGO_ENABLED=1 go test -race -tags fts5 ./internal/sync \
  -run '^(TestBulkParseRetentionBudgetCountsUnknownSourceAtPendingLimit|TestArchiveCollectorReleasesParseLeaseBeforeWrite|TestBulkCollectorBoundsPendingParsedBytesBetweenWrites|TestStartWorkersKeepsBulkBatchingIndependentOfParseAdmission|TestBulkCollectorReleasesFlushedParsedBatch)$' \
  -count=1
CGO_ENABLED=1 go vet -tags fts5 ./...
make pricing-snapshot
make lint-ci
git diff --check
```

Review the final diff for changed parse or archive semantics, leaked leases,
unbounded pending data, cancellation deadlocks, transaction fragmentation, and
unrelated changes.

## Task 6: Scrub and deliver

1. Format the design and plan with the repository's Markdown formatter.
1. Inspect the full branch range, commit metadata, paths, patches, and every
   introduced blob. Scan the final commit message, pull request title, and
   pull request body with the private-data denylist and structural heuristics.
1. Commit only the synchronized design, implementation, and behavior tests. Do
   not amend, squash, or rebase existing commits.
1. Push `fix-full-sync-memory` and open one pull request against `main`.
   Describe the final architecture and aggregate evidence only. Do not add a
   test-plan section, post a pull request comment, or merge.
1. Remove temporary diagnostics, scratch archives created for this
   investigation, and the temporary sudo grant. Leave the benchmark worker
   quarantined.
1. Report the pull request link, selected capacities, test outcomes, aggregate
   memory and wall-clock result, and the separate file-cache limitation.
