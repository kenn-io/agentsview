# Idle Tracker Test Flake Design

## Problem

`TestIdleTrackerExternalRequestResetsIdle` gives the test goroutine only 15 ms
to wake and issue a request before a 40 ms idle deadline. A delayed Windows CI
runner can let the original deadline expire first, causing the request to be
rejected during draining and the test to fail without indicating a production
defect.

## Design

Rewrite the test around a controlled, blocking HTTP request. The wrapped request
will enter the handler before the idle loop starts, proving that the tracker has
recorded active external work. The handler will remain blocked beyond the idle
timeout, then return when the test closes a release channel.

While the handler is blocked, the test will snapshot `lastExternal` under the
tracker mutex. After the wrapped request completes, it will snapshot the value
again and require it to have advanced. Because the first snapshot follows
`beginExternal`, this deterministically proves that `endExternal` records the
request completion even if the test process is descheduled.

The idle callback will also report its firing time through the fixture channel.
The test will assert that the callback fires no earlier than one complete idle
timeout after the handler is released. This behavioral check exercises the real
`Wrap` middleware and verifies the consumer-visible outcome: completing an
external request starts a fresh idle period. The direct timestamp transition is
the deterministic regression guard; the callback timing assertion complements it
rather than relying on scheduler timing alone.

The request's HTTP status will also be asserted so an unexpected draining
transition cannot pass unnoticed. Request completion and idle callback waits
will use generous bounded deadlines so a failure cannot hang the suite or
introduce another narrow CI scheduling window. No production code will change.

## Validation

Run the focused test repeatedly to exercise scheduling variation, then run the
complete `internal/server` package tests. The relevant mutation is removing the
external-request timestamp reset from `endExternal`; the rewritten test must
fail because `lastExternal` will not advance between the in-handler and
post-request snapshots.
