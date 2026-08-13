# Sync Lifecycle Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose the full lifecycle of startup, scheduled, and
daemon-coordinated remote sync work in logs so a foreground sync can be
diagnosed while it is queued or running, without changing sync behavior or API
contracts.

**Architecture:** Add lifecycle entries at the existing orchestration
boundaries. Startup and scheduled reconciliation remain owned by the daemon
process and retain their current lock and ordering. Remote request logging wraps
`runRemoteSyncRequest`, while per-host logging wraps each existing transport
invocation. Tests capture the standard logger while exercising the existing
orchestration helpers with fake engines or isolated fixtures.

**Tech Stack:** Go, standard `log` package, `testing` with existing Testify
assertions, isolated SQLite test fixtures.

## Global Constraints

- Preserve lock ownership, sync ordering, reconciliation scope, API response
  types, and data behavior.
- Never log credentials, URLs, request bodies, source paths, or remote payloads.
- Use test-first changes: add focused behavior tests, confirm they fail for the
  missing lifecycle entries, then implement the smallest production change.
- Do not run branch code against the live daemon, production archive, or
  installed binary.
- After Go changes, run `go fmt ./...` and `go vet ./...`, plus focused package
  tests.

______________________________________________________________________

## Task 1: Add startup and scheduled lifecycle coverage

**Files:** `cmd/agentsview/main.go`, `cmd/agentsview/periodic_sync_test.go`, and
the relevant startup test file if needed.

- [ ] Add a failing test around the existing scheduled reconciliation helper.
  Capture the standard logger, run a successful pass and a failing provider
  pass through the real `runScheduledSyncPass` orchestration, and assert
  stable `scheduled reconciliation ...` start/finish event names, duration
  fields, and `outcome=completed`/`outcome=failed` fields. Keep assertions
  independent of exact elapsed values.
- [ ] Add a failing test for the startup gap callback, using the existing
  startup wiring or a narrowly extracted callback helper. Exercise both an
  empty/no-op root set and a failed reconciliation path, and assert
  start/finish entries include the root count, duration, and terminal outcome.
- [ ] Implement the lifecycle logging around the existing startup callback and
  scheduled pass. Use deferred terminal logging or a small local lifecycle
  helper so cancellation and errors still produce a terminal event. Keep the
  existing retry queue and `RecordStartupReconciled` behavior unchanged.
- [ ] Run the focused `cmd/agentsview` tests and inspect the diff for accidental
  changes to scheduling or lock behavior.

## Task 2: Add coordinated remote request lifecycle coverage

**Files:** `internal/server/huma_routes_sync.go`,
`internal/server/huma_routes_sync_internal_test.go`.

- [ ] Add a failing test that invokes `runRemoteSyncRequest` with the existing
  isolated sync fixture and canceled/successful paths, captures standard logs,
  and verifies request start/finish events include `include_local`, `full`,
  duration, and a terminal outcome/error classification.
- [ ] Add a failing test for `runRemoteSyncHostsOwned` that exercises successful
  and failing host callbacks through the existing transport seams. Assert one
  start and one terminal event per host, with host identity, duration, session
  counters, and cancellation/error outcome; assert sensitive URL/token values
  do not appear.
- [ ] Implement request lifecycle logging with a deferred terminal entry and
  per-host lifecycle logging around the existing SSH/HTTP switch. Preserve all
  current error sanitization, pending-cleanup returns, aggregation, and
  response construction.
- [ ] Run the focused `internal/server` sync tests, including the existing
  cancellation and cleanup cases.

## Task 3: Repository-wide verification and review

**Files:** all changed files from Tasks 1–2.

- [ ] Run the focused tests again with the final implementation, then
  `go fmt ./...`, `go vet ./...`, and the repository’s applicable lint/check
  commands with an isolated cache if shared worktrees hold a lint lock.
- [ ] Review `git diff` and `git diff --check`; confirm logs contain only
  approved operational fields and no private data. Re-read the design spec
  against the implementation.
- [ ] Run the private-data scrub required before publishing. Resolve any finding
  by removing or anonymizing the data rather than weakening the scan.
- [ ] Commit the implementation with a focused conventional commit, preserving
  the earlier documentation and parser-test repair commits.

## Task 4: Push and open the pull request

- [ ] Fetch the current `origin/main` ref and verify the branch/base
  relationship without rebasing or rewriting existing commits.
- [ ] Push the current feature branch to `origin` with upstream tracking.
- [ ] Open a pull request with a summary-only description that states the
  lifecycle logging now emitted, why it is needed, and the unchanged
  lock/API/data behavior. Do not include private project names, hostnames,
  paths, credentials, command transcripts, or a testing checklist.
- [ ] Read back the created PR and report its URL, commit, and verification
  evidence. Do not merge it or poll CI.
