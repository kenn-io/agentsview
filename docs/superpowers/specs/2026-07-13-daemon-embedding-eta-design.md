# Daemon-Owned Embedding ETA Design

## Context

The embeddings settings UI currently estimates throughput and remaining time in
each browser. Its exponentially weighted moving average (EWMA) starts with the
first status response seen by that page, so opening or reloading Settings resets
the estimate to an `Estimating` warm-up even when the daemon has been building
for hours. Multiple browsers also calculate independent answers from different
poll samples.

The build manager already receives every progress callback and keeps the current
build status in daemon memory. It is therefore the authoritative place to track
the estimate. An estimate has no archival value and must not be written to the
main database or vector index.

## Considered Approaches

1. **Keep the estimator in each browser.** This is the smallest change, but it
   preserves the reload reset and inconsistent estimates that prompted this
   work.
1. **Keep a bounded progress history in the daemon and calculate rates when
   status is requested.** This survives browser reloads, but retains more
   state and adds computation without improving the result.
1. **Maintain one EWMA in the in-memory build manager.** This uses the complete
   progress stream, keeps status reads cheap, and gives every client the same
   answer. This is the selected approach.

## Design

The vector build manager will own an unexported estimator alongside its existing
in-memory `BuildStatus`. The estimator uses a monotonic clock and the current
progress counter to fold each positive progress delta into an EWMA with the same
0.3 smoothing factor as the existing UI estimator. Two positive samples are
required before an estimate is ready.

The estimator resets when a build starts, the build phase changes, the total
changes, or the completed counter moves backwards. Polls and callbacks with no
new progress do not advance its baseline; when work resumes, the stalled time is
therefore reflected in the next observed rate. Finishing or failing a build
clears the active estimate.

`BuildStatus` will expose additive JSON fields for estimate readiness, smoothed
units per second, and estimated remaining milliseconds. These values are
snapshots maintained under the manager's existing status lock. They are never
stored in SQLite, `vectors.db`, configuration, or another file, and naturally
disappear when the daemon exits.

The settings component will render the server-provided status fields and remove
its local estimator. Elapsed time remains derived from `started_at`, because it
is wall-clock display data rather than a progress-rate measurement. Once the
daemon has warmed its estimator, a newly opened browser shows the ETA on its
first status response.

## Error and Lifecycle Semantics

- Scanning and embedding are independent phases and never share a rate.
- Unknown totals and insufficient positive samples report `Estimating` rather
  than a fabricated zero or infinite ETA.
- A zero or non-finite calculated rate is not published as ready.
- A completed or failed build reports no active ETA, while retaining the
  existing result and error behavior.
- Restarting the daemon loses estimator state by design. Any new build warms the
  estimator again from its own progress callbacks.

## Testing

Backend tests will drive progress observations with a controllable monotonic
clock and verify EWMA calculation, warm-up, stalled intervals, phase and
denominator resets, counter regression, and finish/failure cleanup. Manager
status tests will prove the estimate is present in the status snapshot without
database interaction.

Frontend component tests will provide ready and warming estimates in the first
status response and assert that the UI renders them immediately. The obsolete
browser estimator and its unit tests will be removed rather than retained as a
fallback.

## Non-goals

- Persisting ETA history across daemon restarts.
- Predicting scanning duration before an embedding denominator exists.
- Changing embedding scheduling, concurrency, batching, or vector data.
- Adding a second compatibility estimator in the frontend.
