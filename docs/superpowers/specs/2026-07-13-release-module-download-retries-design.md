# Release Module Download Retries Design

## Problem

The tagged CLI release workflow lets `go build` fetch modules implicitly. A
transient module-proxy transport failure can therefore abort one matrix build
after the frontend and pricing preparation have completed. Because the release
and PyPI jobs require every build, a single short-lived download failure blocks
all CLI archives and package publication.

The 0.38.1 Linux amd64 build demonstrated this failure mode when
`proxy.golang.org` returned an HTTP/2 internal error while downloading a
transitive module. Retrying the job succeeded without any source or
configuration change, confirming that the failure was external and transient.

## Design

Add an explicit module-prefetch step to both CLI release build jobs before
compilation. The step will run `go mod download` through the repository's
existing `scripts/retry.sh` helper with three total attempts and a short linear
backoff.

Prefetching separates network availability from compilation. Completed module
downloads remain cached between attempts, while a later deterministic compiler
or packaging failure still fails immediately without retrying the full build.
Applying the same preparation to the Linux and cross-platform matrices keeps
release behavior consistent across architectures and operating systems.

The Linux build will continue to copy the `go-sqlite3` amalgamation header after
prefetching. Its existing targeted download becomes unnecessary because the
general prefetch has already populated the module cache.

## Failure Handling

The retry policy is bounded at three attempts. Each failed attempt remains
visible in the job log, and exhaustion preserves the final `go mod download`
exit status. Persistent dependency, checksum, authentication, or proxy failures
therefore continue to block publication rather than being hidden.

The workflow will not retry `go build`, archive creation, checksum generation,
release uploads, or PyPI publication. Those operations have different failure
semantics and are outside this incident's root cause.

## Testing

The existing behavioral shell test for `scripts/retry.sh` already exercises
eventual recovery, retry exhaustion, argument forwarding, linear delays, and
final exit-status preservation with controlled subprocesses. The change will run
that test, validate the modified workflow with `actionlint` when available, and
run the repository's normal workflow-adjacent checks.

No source-grep assertion will be added for the YAML wiring. Such a test would
only mirror the workflow text rather than exercise release behavior.

## Tradeoffs

An explicit prefetch adds one fast command when the module cache is already warm
and up to a bounded delay when the dependency service is unavailable. This is
preferable to retrying complete builds, which would repeat compilation for
deterministic failures, or falling back directly to source repositories, which
would change dependency transport and supply-chain behavior without providing a
bounded retry policy.
