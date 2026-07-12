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

The idle callback will report its firing time through the fixture channel. The
test will assert that the callback fires no earlier than one complete idle
timeout after the handler is released. This exercises the real `Wrap` middleware
and verifies the consumer-visible behavior: completing an external request
starts a fresh idle period.

The request's HTTP status will also be asserted so an unexpected draining
transition cannot pass unnoticed. No production code will change.

## Validation

Run the focused test repeatedly to exercise scheduling variation, then run the
complete `internal/server` package tests. The relevant mutation is removing the
external-request timestamp reset from `endExternal`; the rewritten test must
fail because the idle callback would fire immediately after the blocked request
ends instead of after a fresh timeout.
