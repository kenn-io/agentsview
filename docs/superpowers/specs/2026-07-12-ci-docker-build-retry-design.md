# CI Docker Build Retry Design

## Problem

The integration job builds an SSH test image from Docker Hub after the
PostgreSQL tests complete. A transient Docker Hub authentication or registry
failure currently fails the entire job immediately, even when a subsequent
attempt would succeed.

## Design

Add a small shell helper that runs an arbitrary command up to a fixed number of
attempts. It will return immediately when the command succeeds, wait with a
short linear backoff between failures, and return the final command's exit
status after the last attempt.

The integration workflow will invoke the SSH image build through this helper
with three total attempts. Retries will remain bounded so persistent Dockerfile,
dependency, or registry failures still fail CI promptly.

## Testing

A behavioral shell test will run the helper against controlled fake commands. It
will verify that a command which fails twice and then succeeds is attempted
three times, that arguments are forwarded unchanged, and that a command which
never succeeds stops after the configured limit and preserves its exit status.
The existing shell-script CI job will run this test.

## Tradeoffs

Retrying every Docker build failure may add a short delay for deterministic
build errors. Restricting retries to specific error text would be brittle
because Docker and registry failure messages vary, so bounded unconditional
retries provide the simpler and more reliable policy.
