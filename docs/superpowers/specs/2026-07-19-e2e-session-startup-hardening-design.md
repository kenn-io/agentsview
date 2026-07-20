# E2E Session Startup Hardening Design

## Problem

The main-branch E2E job intermittently failed in WebKit while waiting for the
first session row. The shared `SessionsPage.goto()` helper uses a fixed
five-second visibility timeout even though CI permits longer test execution and
can experience transient runner contention. The same build loaded sessions in
later WebKit tests, and the exact failing tree passed the targeted test twenty
times in an isolated Linux Playwright environment.

The failure also occurred before the runtime-stability test reached its
assertions. As a result, browser errors captured by `RuntimeErrorMonitor` were
not included in the failure output, leaving a real rendering failure and a slow
startup harder to distinguish.

## Design

Keep the five-second session startup timeout for local runs so developer
feedback remains fast. When `CI=true`, allow up to fifteen seconds for the first
session row to become visible. This changes only the Playwright page object;
application request handling and rendering behavior remain unchanged.

The runtime-stability spec will preserve its original startup error while also
including any browser errors captured before startup failed. Empty diagnostics
will not replace or obscure the locator timeout.

The timeout remains scoped to initial session readiness. Global Playwright
timeouts and retry counts will not change, so unrelated hangs and deterministic
failures retain their current behavior.

## Testing

Add a Playwright regression that delays the initial sessions response beyond
five seconds in WebKit under CI settings, then asserts that
`SessionsPage.goto()` still reaches a visible session row. The delay exercises
the real HTTP and rendered-DOM boundary rather than asserting a timeout constant
or mocking the page object.

Run the regression before implementation to confirm that the existing
five-second guard fails, then run it after implementation to confirm the
CI-specific wait succeeds. Run the runtime-stability WebKit test repeatedly to
ensure the diagnostic handling and longer guard do not introduce instability.

## Tradeoffs

A genuinely broken CI startup may take up to ten seconds longer to fail. That
cost is limited to CI and only the initial session readiness check. Raising all
expectation timeouts or enabling retries would cover more transient failures,
but would also hide unrelated regressions and make the suite slower to report
deterministic failures.
