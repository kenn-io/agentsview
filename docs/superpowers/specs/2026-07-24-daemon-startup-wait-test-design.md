# Deterministic Daemon Startup Wait Test Design

## Problem

`TestEnsureBackgroundServeChecksTooNewDatabaseAfterStartupWait` coordinates
two goroutines with `time.Sleep(2 * startProbeTick())`. The delay does not prove
that `ensureBackgroundServe` has entered `WaitForDaemonStartupContext` before
the simulated daemon publishes its runtime record and releases the external
startup lock. On a sufficiently different scheduler, the launch path can
observe the handoff in another state and return an unrelated error.

The test then reports only `Should be true` because it checks
`db.IsDataVersionTooNew(err)` without including the unexpected error.

## Design

Add a package-level `waitForDaemonStartupForEnsure` function variable whose
production value is `WaitForDaemonStartupContext`, matching the existing
process-start seams in `serve_background.go`.

The test will temporarily wrap that seam. The wrapper closes a channel when
the startup wait is entered and then delegates to the real implementation.
The runtime-publishing goroutine will block on that channel instead of sleeping
for an assumed scheduling interval. Production timing and timeout values remain
unchanged.

Use a testify error assertion that includes the returned error when the
data-version classification is wrong. This keeps any future failure
diagnostic without changing the contract under test.

## Validation

- Run the focused test repeatedly with the repository's required FTS5 setup.
- Run the `cmd/agentsview` test package.
- Run `go fmt ./...` and `go vet ./...`.
- Run the repository-supported broader test target before publishing.

## Out of Scope

- Increasing startup deadlines or probe intervals.
- Changing daemon lifecycle behavior.
- Reworking other sleep-based lifecycle tests without evidence that they share
  this failure.
