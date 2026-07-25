# Deterministic Daemon Startup Wait Test Implementation Plan

> **For agentic workers:** Follow this plan task by task using test-driven development. Do not broaden the production behavior or timeout policy.

**Goal:** Remove the Windows timing race from `TestEnsureBackgroundServeChecksTooNewDatabaseAfterStartupWait` by synchronizing the test with the exact point where `ensureBackgroundServe` begins waiting for another daemon startup.

**Architecture:** Introduce one package-level function seam that defaults to `WaitForDaemonStartupContext`. The production path still delegates directly to the existing implementation. The test temporarily wraps that seam, signals a channel when the wait begins, then publishes the simulated daemon runtime and releases the external-start lock.

**Tech Stack:** Go 1.26, testify, existing daemon lifecycle test helpers.

## Global Constraints

- Preserve all daemon startup behavior, timeouts, and error precedence.
- Do not replace synchronization with a longer sleep or retry.
- Restore the package-level seam with `t.Cleanup`.
- Keep the test assertion diagnostic by printing the unexpected error.
- Run the repository-required Go formatting, vetting, focused tests, and short suite.

---

### Task 1: Make the startup-wait handoff deterministic

**Files:**

- Modify: `cmd/agentsview/serve_background_test.go`
- Modify: `cmd/agentsview/serve_background.go`

**Step 1: Write the failing test change**

In `TestEnsureBackgroundServeChecksTooNewDatabaseAfterStartupWait`, replace the publish delay with a channel synchronized through a not-yet-defined `waitForDaemonStartupForEnsure` seam:

```go
waitStarted := make(chan struct{})
oldWaitForDaemonStartup := waitForDaemonStartupForEnsure
waitForDaemonStartupForEnsure = func(
	ctx context.Context,
	dataDir string,
	timeout time.Duration,
	authToken ...string,
) bool {
	close(waitStarted)
	return oldWaitForDaemonStartup(
		ctx, dataDir, timeout, authToken...,
	)
}
t.Cleanup(func() {
	waitForDaemonStartupForEnsure = oldWaitForDaemonStartup
})

errCh := make(chan error, 1)
go func() {
	<-waitStarted
	_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		oldHost, oldPort,
		withRuntimeVersion("1.0.0"),
		withRuntimeAPIVersion(0),
	))
	unlockStart()
	errCh <- err
}()
```

Keep the existing runtime-record contents and cleanup behavior. Change the type check to include the actual error:

```go
assert.True(
	t,
	db.IsDataVersionTooNew(err),
	"expected data-version-too-new error, got %v",
	err,
)
```

**Step 2: Verify the test does not compile yet**

Run:

```bash
env CC=/usr/bin/gcc CXX=/usr/bin/g++ CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run '^TestEnsureBackgroundServeChecksTooNewDatabaseAfterStartupWait$' -count=1
```

Expected: FAIL with `undefined: waitForDaemonStartupForEnsure`.

**Step 3: Add the production seam**

Beside the existing `startServeBackgroundProcessForEnsure` seam in `serve_background.go`, add:

```go
var waitForDaemonStartupForEnsure = WaitForDaemonStartupContext
```

Replace only the direct `WaitForDaemonStartupContext` call inside `ensureBackgroundServe` with `waitForDaemonStartupForEnsure`. Do not change any arguments or surrounding control flow.

**Step 4: Format and run the focused test repeatedly**

Run:

```bash
gofmt -w cmd/agentsview/serve_background.go cmd/agentsview/serve_background_test.go
env CC=/usr/bin/gcc CXX=/usr/bin/g++ CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run '^TestEnsureBackgroundServeChecksTooNewDatabaseAfterStartupWait$' -count=50
```

Expected: PASS on all 50 runs.

**Step 5: Run package and repository verification**

Run:

```bash
env CC=/usr/bin/gcc CXX=/usr/bin/g++ CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -count=1
go fmt ./...
go vet ./...
make test-short
```

Expected: all commands PASS. If the restricted execution environment prevents loopback listeners or normal user-state writes, rerun the affected command with normal host capabilities and record the reason.

**Step 6: Review and commit**

Inspect:

```bash
git diff --check
git diff --stat
git diff
git status --short
```

Confirm that no sleep remains in this test and the only production change is the new delegate seam. Commit:

```bash
git add cmd/agentsview/serve_background.go cmd/agentsview/serve_background_test.go
git commit -m "fix(test): synchronize daemon startup wait"
```
