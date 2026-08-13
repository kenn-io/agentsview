# Sync Lifecycle Observability

## Problem

The daemon can hold the shared sync lock for a long startup reconciliation after
the HTTP server begins listening. A foreground `agentsview sync` request then
waits without a clear indication of whether it is queued, actively processing
local data, or contacting a remote host. The current logs expose some failures
and generic HTTP access entries, but not operation boundaries, lock waits,
durations, or successful completion.

## Goals

- Make startup reconciliation visible from start through completion.
- Make scheduled reconciliation visible from start through completion.
- Make coordinated remote syncs visible at request and per-host boundaries.
- Include durations, relevant counts, cancellation, and errors in lifecycle
  entries.
- Preserve sync ordering, locking, data behavior, and API response schemas.
- Add focused tests that verify emitted lifecycle messages through real helper
  behavior rather than source-text assertions.

## Non-goals

- Changing the sync scheduler or lock ownership semantics.
- Adding a new status API or changing the JSON shape of `/sync/status`.
- Rebuilding search indexes differently or changing reconciliation scope.
- Adding production-only diagnostics that expose credentials, paths, or remote
  payloads.

## Design

Use structured-enough `log.Printf` lifecycle entries at the existing operation
boundaries:

1. Startup gap reconciliation logs its start, configured root count, and a
   terminal entry with elapsed time and outcome.
1. Scheduled reconciliation logs its start and terminal result with elapsed
   time. The terminal result distinguishes completion, failure, and
   cancellation.
1. A coordinated remote-sync request logs its start and terminal result,
   including whether local work and a full pass were requested.
1. Each remote host logs start and terminal result, including elapsed time,
   session counts, failures, and cancellation/error state.

The messages use host identities already present in configuration but never
include tokens, URLs, request bodies, or source paths. Deferred functions or
small lifecycle helpers ensure terminal logs are emitted on success, error, and
cancellation paths without changing control flow.

## Testing

Add table-driven tests around the lifecycle helpers and existing orchestration
functions. Tests will capture the standard logger, invoke real callbacks with
successful and failing outcomes, and assert the stable event names and outcome
fields. Duration values will use a deterministic injected clock or a bounded
format assertion so tests do not depend on wall-clock timing.

Run the focused Go tests, then `go fmt ./...` and `go vet ./...` as required by
the repository instructions. No live daemon, production archive, or installed
binary will be used for verification.
