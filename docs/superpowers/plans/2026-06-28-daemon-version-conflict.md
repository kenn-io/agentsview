# Daemon Version Conflict Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `agentsview serve` handle existing daemon version conflicts with
the same safe upgrade policy as daemon auto-start, plus an explicit `--replace`
override and clearer status output.

**Architecture:** Add one shared replacement classifier in the `cmd/agentsview`
package, then have foreground serve, background serve, and status rendering
consume it. Keep the final SQLite write-open daemon rejection as a backstop
instead of weakening `openWriteDB`.

**Tech Stack:** Go, Cobra CLI flags, existing kit daemon runtime records,
testify assertions, existing daemon runtime test helpers.

______________________________________________________________________

## File Structure

- Create `cmd/agentsview/serve_replacement.go`
    - Owns daemon replacement policy classification, user-facing conflict lines,
      and the small stop/report helper used by serve entry points.
- Create `cmd/agentsview/serve_replacement_test.go`
    - Unit tests for replacement decisions and output without starting the full
      server.
- Modify `cmd/agentsview/cli.go:123-167`
    - Adds the `--replace` flag and passes it to foreground/background serve.
- Modify `cmd/agentsview/main.go:88-118`
    - Adds foreground serve options and runs daemon replacement before
      `MarkDaemonStarting` and `mustOpenWriteDB`.
- Modify `cmd/agentsview/serve_background.go:112-175,272-350,500-515`
    - Reuses the shared classifier, supports `--replace`, preserves the current
      unconditional `no_sync` adoption, and strips parent-only flags from the
      child process args.
- Modify `cmd/agentsview/serve_lifecycle.go:22-63`
    - Reports incompatible but responding daemons as running and incompatible.
- Modify `cmd/agentsview/serve_background_test.go`
    - Covers background `--replace` and parent-only child arg stripping.
- Modify `cmd/agentsview/serve_lifecycle_test.go`
    - Covers `--replace` flag registration and incompatible status output.
- Modify `docs/commands.md:20-36,64-90`
    - Documents `--replace` in the serve flag table and lifecycle text.

## Task 1: Shared Replacement Classifier

**Files:**

- Create: `cmd/agentsview/serve_replacement.go`

- Create: `cmd/agentsview/serve_replacement_test.go`

- [ ] **Step 1: Write failing replacement decision tests**

Add tests covering the policy without stopping processes:

```go
func TestServeDaemonReplacementDecisionAutoReplacesOlderRelease(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "1.1.0")

	decision := decideServeDaemonReplacement(
		config.Config{DataDir: dir}, serveReplacementOptions{},
	)

	require.NotNil(t, decision.Runtime)
	assert.Equal(t, serveReplacementAuto, decision.Action)
	assert.Contains(t, decision.Reason, "older")
}

func TestServeDaemonReplacementDecisionRefusesDevBuild(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "dev")

	decision := decideServeDaemonReplacement(
		config.Config{DataDir: dir}, serveReplacementOptions{},
	)

	require.NotNil(t, decision.Runtime)
	assert.Equal(t, serveReplacementRefuse, decision.Action)
	assert.Contains(t, decision.Reason, "dev builds")
	assert.Contains(t, strings.Join(serveDaemonConflictLines(decision), "\n"),
		"--replace")
}

func TestServeDaemonReplacementDecisionReplaceOverridesForwardData(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeVersion("9.9.9"),
		withRuntimeMetadata(runtimeDataVersion,
			strconv.Itoa(db.CurrentDataVersion()+1)),
	))
	setTestVersion(t, "dev")

	decision := decideServeDaemonReplacement(
		config.Config{DataDir: dir},
		serveReplacementOptions{Replace: true},
	)

	assert.Equal(t, serveReplacementExplicit, decision.Action)
	require.Error(t, decision.CompatibilityErr)
}

func TestServeDaemonReplacementDecisionIgnoresReadOnlyDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeReadOnly(true),
		withRuntimeAPIVersion(0),
	))

	decision := decideServeDaemonReplacement(
		config.Config{DataDir: dir},
		serveReplacementOptions{Replace: true},
	)

	assert.Equal(t, serveReplacementNone, decision.Action)
	assert.Nil(t, decision.Runtime)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestServeDaemonReplacementDecision' -count=1
```

Expected: fail with undefined `decideServeDaemonReplacement`,
`serveReplacementOptions`, and related types.

- [ ] **Step 3: Implement the classifier**

In `cmd/agentsview/serve_replacement.go`, add:

```go
type serveReplacementAction int

const (
	serveReplacementNone serveReplacementAction = iota
	serveReplacementUseExisting
	serveReplacementAuto
	serveReplacementExplicit
	serveReplacementRefuse
)

type serveReplacementOptions struct {
	Replace bool
}

type serveReplacementDecision struct {
	Action           serveReplacementAction
	Runtime          *DaemonRuntime
	CompatibilityErr error
	Reason           string
}
```

Then implement `decideServeDaemonReplacement(cfg, opts)`:

- Check `FindDaemonRuntime(cfg.DataDir, cfg.AuthToken)` first.
- Ignore read-only runtimes.
- Return `serveReplacementExplicit` when `opts.Replace` is true.
- Return `serveReplacementAuto` when `shouldUpgradeDaemonRuntime` is true.
- Return `serveReplacementUseExisting` only for compatible writable daemons that
  are safe to reuse as-is, such as the same binary version.
- Return `serveReplacementRefuse` when a compatible writable daemon has a
  different version that the current binary cannot prove is older, including
  dev builds, downgrades, and non-semver versions.
- If no compatible writable runtime is found, check
  `findIncompatibleWritableDaemonRuntime`.
- For incompatible writable runtimes, return explicit, auto, or refuse using
  `shouldUpgradeIncompatibleDaemonRuntime`.
- Return `serveReplacementNone` when no writable runtime exists.

Add a small refusal reason helper. Reasons should distinguish at least:

- current binary is a dev build;
- daemon API/data version is ahead;
- daemon version is newer than this binary;
- current binary is not newer than the daemon.

Add `serveDaemonConflictLines(decision)` and
`serveDaemonReplacementLines(decision)` helpers for consistent output.

- [ ] **Step 4: Run focused tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestServeDaemonReplacementDecision' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/agentsview/serve_replacement.go cmd/agentsview/serve_replacement_test.go
git commit -m "fix(serve): classify daemon replacement conflicts"
```

## Task 2: Foreground Serve Replacement

**Files:**

- Modify: `cmd/agentsview/cli.go:123-167`

- Modify: `cmd/agentsview/main.go:88-118`

- Modify: `cmd/agentsview/serve_replacement.go`

- Modify: `cmd/agentsview/serve_replacement_test.go`

- [ ] **Step 1: Write failing foreground helper tests**

Add tests around a helper that returns instead of calling `fatal`:

```go
func TestPrepareForegroundServeDaemonAutoReplacesOlderDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "1.1.0")

	var stoppedPID int
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stoppedPID = rt.Record.PID
		RemoveDaemonRuntime(dir)
		return nil
	})

	out := captureStdout(t, func() {
		cont, err := prepareForegroundServeDaemon(
			config.Config{DataDir: dir}, serveReplacementOptions{},
		)
		require.NoError(t, err)
		assert.True(t, cont)
	})

	assert.Equal(t, os.Getpid(), stoppedPID)
	assert.Contains(t, out, "Replacing agentsview daemon")
	assert.Contains(t, out, "version 1.0.0")
	assert.Nil(t, FindDaemonRuntime(dir))
}

func TestPrepareForegroundServeDaemonRefusesDevWithoutReplace(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "dev")
	forbidStopDaemonRuntimeForUpgrade(t, "dev build needs --replace")

	cont, err := prepareForegroundServeDaemon(
		config.Config{DataDir: dir}, serveReplacementOptions{},
	)

	assert.False(t, cont)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev builds")
	assert.Contains(t, err.Error(), "--replace")
}

func TestPrepareForegroundServeDaemonReplaceStopsWritableDevConflict(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "dev")

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = true
		RemoveDaemonRuntime(dir)
		return nil
	})

	out := captureStdout(t, func() {
		cont, err := prepareForegroundServeDaemon(
			config.Config{DataDir: dir},
			serveReplacementOptions{Replace: true},
		)
		require.NoError(t, err)
		assert.True(t, cont)
	})

	assert.True(t, stopped)
	assert.Contains(t, out, "Replacing agentsview daemon")
	assert.Nil(t, FindDaemonRuntime(dir))
}

func TestPrepareForegroundServeDaemonReplaceLeavesReadOnlyDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeReadOnly(true),
		withRuntimeAPIVersion(0),
	))
	forbidStopDaemonRuntimeForUpgrade(t, "read-only daemon must not be stopped")

	cont, err := prepareForegroundServeDaemon(
		config.Config{DataDir: dir},
		serveReplacementOptions{Replace: true},
	)

	require.NoError(t, err)
	assert.True(t, cont)
	rt, compatErr := FindIncompatibleDaemonRuntime(dir)
	require.NotNil(t, rt)
	require.Error(t, compatErr)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestPrepareForegroundServeDaemon' -count=1
```

Expected: fail with undefined `prepareForegroundServeDaemon`.

- [ ] **Step 3: Implement foreground helper**

Add `prepareForegroundServeDaemon(cfg, opts) (bool, error)`:

- `serveReplacementNone`: return `true, nil`.
- `serveReplacementUseExisting`: print `agentsview already running at ...` and
  return `false, nil`.
- `serveReplacementAuto` or `serveReplacementExplicit`: print replacement lines,
  call `stopDaemonRuntimeForUpgrade(cfg, decision.Runtime)`, and return
  `false, err` if stopping or identity confirmation fails; otherwise return
  `true, nil`.
- `serveReplacementRefuse`: return `false, errors.New(strings.Join(...))`.

Do not call `MarkDaemonStarting` inside this helper.

- [ ] **Step 4: Wire CLI and foreground runServe**

In `newServeCommand`, add:

```go
var replace bool
cmd.Flags().BoolVar(
	&replace,
	"replace",
	false,
	"Replace a running local daemon before starting",
)
```

Change foreground serve to pass options:

```go
runServe(mustLoadConfig(cmd), serveOptions{ReplaceDaemon: replace})
```

Change `runServe` to accept `serveOptions` and call
`prepareForegroundServeDaemon` after auth-token generation and before
`MarkDaemonStarting`. If `cont` is false, return. If `err` is non-nil, call
`fatal("%v", err)`.

- [ ] **Step 5: Run focused tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestPrepareForegroundServeDaemon|TestServeCommand' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/agentsview/cli.go cmd/agentsview/main.go cmd/agentsview/serve_replacement.go cmd/agentsview/serve_replacement_test.go
git commit -m "fix(serve): replace older daemons before foreground start"
```

## Task 3: Background Serve Replacement and Child Args

**Files:**

- Modify: `cmd/agentsview/cli.go:123-167`

- Modify: `cmd/agentsview/serve_background.go:112-175,272-350,500-515`

- Modify: `cmd/agentsview/serve_background_test.go`

- Modify: `cmd/agentsview/serve_replacement.go`

- [ ] **Step 1: Write failing background tests**

Add tests for explicit replacement and parent-only child args:

```go
func TestServeBackgroundChildArgsRemovesReplaceFlag(t *testing.T) {
	got := serveBackgroundChildArgs([]string{
		"serve", "--background", "--replace", "--port", "0",
	})
	assert.Equal(t, []string{"serve", "--port", "0"}, got)
}

func TestRunServeBackgroundReplaceOverridesDevRefusal(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "dev")

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = true
		RemoveDaemonRuntime(dir)
		return nil
	})

	newHost, newPort := testPingServer(t)
	oldStart := startServeBackgroundProcessForRun
	startServeBackgroundProcessForRun = func(
		_ config.Config, arguments []string,
	) (*exec.Cmd, string, error) {
		assert.NotContains(t, arguments, "--replace")
		_, err := WriteDaemonRuntime(dir, newHost, newPort, "dev", false)
		require.NoError(t, err)
		cmd := exec.Command("sleep", "2")
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, "test.log", nil
	}
	t.Cleanup(func() { startServeBackgroundProcessForRun = oldStart })

	runServeBackground(
		config.Config{DataDir: dir},
		[]string{"serve", "--background", "--replace"},
		serveReplacementOptions{Replace: true},
	)

	assert.True(t, stopped)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestServeBackgroundChildArgsRemovesReplaceFlag|TestRunServeBackgroundReplaceOverridesDevRefusal' -count=1
```

Expected: fail because `--replace` is not stripped and `runServeBackground` does
not accept replacement options.

- [ ] **Step 3: Wire background replacement**

Change signatures:

```go
func runServeBackgroundCommand(cmd *cobra.Command, replace bool)
func runServeBackground(cfg config.Config, args []string, opts serveReplacementOptions)
```

Keep `ensureBackgroundServe` with no explicit replace option. Other CLI commands
must not gain a `--replace` path.

In `runServeBackground`, replace both compatible and incompatible branches with
the shared decision helper. Before stopping for auto or explicit replacement,
call `adoptDaemonRuntimeLaunchOptions(&cfg, decision.Runtime)` to preserve the
current unconditional `no_sync` behavior.

For `serveReplacementUseExisting`, print the current
`agentsview already running at ...` message and return.

For `serveReplacementRefuse`, use the shared conflict lines and keep the fatal
prefix specific to background serve.

Add `isParentOnlyServeFlagArg` or extend `serveBackgroundChildArgs` so
`--replace`, `--replace=true`, `-replace`, and `-replace=true` are removed along
with `--background` and `-background`.

- [ ] **Step 4: Run background tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestServeBackground|TestRunServeBackground|TestEnsureBackgroundServe' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/agentsview/cli.go cmd/agentsview/serve_background.go cmd/agentsview/serve_background_test.go cmd/agentsview/serve_replacement.go
git commit -m "fix(serve): support explicit background daemon replacement"
```

## Task 4: Incompatible Status Output

**Files:**

- Modify: `cmd/agentsview/serve_lifecycle.go:22-63`

- Modify: `cmd/agentsview/serve_lifecycle_test.go`

- Modify: `cmd/agentsview/serve_replacement.go`

- [ ] **Step 1: Write failing status tests**

Add helper and command-level coverage:

```go
func TestServeStatusLinesIncompatibleDaemon(t *testing.T) {
	rt := &DaemonRuntime{
		Record: daemon.RuntimeRecord{PID: 4242, Version: "0.1.0"},
		Host:   "127.0.0.1",
		Port:   8080,
		API:    0,
		Data:   db.CurrentDataVersion(),
	}
	compatErr := daemonRuntimeCompatibilityError(rt)

	out := strings.Join(serveIncompatibleStatusLines(rt, compatErr), "\n")

	assert.Contains(t, out, "agentsview running at http://127.0.0.1:8080")
	assert.Contains(t, out, "pid:")
	assert.Contains(t, out, "version: 0.1.0")
	assert.Contains(t, out, "incompatible")
	assert.Contains(t, out, "API version")
	assert.Contains(t, out, "binary:")
	assert.Contains(t, out, "serve --replace")
}

func TestRunServeStatusReportsIncompatibleRespondingDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeVersion("0.1.0"),
		withRuntimeAPIVersion(0),
	))

	out := captureStdout(t, func() {
		runServeStatus(config.Config{DataDir: dir})
	})

	assert.Contains(t, out, "incompatible")
	assert.NotContains(t, out, "not responding")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestServeStatus.*Incompatible|TestRunServeStatusReportsIncompatible' -count=1
```

Expected: fail with undefined status helper or current "not responding" output.

- [ ] **Step 3: Implement status rendering**

In `runServeStatus`, after `FindDaemonRuntime` returns nil and before
`liveDaemonRecords`, call:

```go
if rt, err := FindIncompatibleDaemonRuntime(cfg.DataDir, cfg.AuthToken); err != nil && rt != nil {
	for _, line := range serveIncompatibleStatusLines(rt, err) {
		fmt.Println(line)
	}
	return
}
```

Implement `serveIncompatibleStatusLines` using the same detail helpers as
conflict output. Include URL, PID, daemon version, binary version, API/data
versions, compatibility error, and `agentsview serve --replace` /
`agentsview serve stop` guidance.

- [ ] **Step 4: Run focused status tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestServeStatus|TestRunServeStatus' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/agentsview/serve_lifecycle.go cmd/agentsview/serve_lifecycle_test.go cmd/agentsview/serve_replacement.go
git commit -m "fix(serve): report incompatible daemon status"
```

## Task 5: CLI Docs and Final Verification

**Files:**

- Modify: `docs/commands.md:20-36,64-90`

- Optional modify: `README.md:38-68` only if the quickstart text needs a short
  status/replacement note after seeing final CLI output.

- Test: `cmd/agentsview/serve_replacement_test.go`

- [ ] **Step 1: Document `--replace`**

In `docs/commands.md`, add the flag row:

```markdown
| `--replace` | `false` | Replace a running local daemon before starting |
```

Add one short paragraph near the background/status text:

```markdown
When a newer release binary finds an older writable daemon, `serve` may replace
it automatically. Dev builds, downgrades, and forward API/data mismatches require
an explicit `agentsview serve --replace`; `serve status` prints the running
daemon version and the matching command when replacement is needed.
```

- [ ] **Step 2: Format Markdown**

Run:

```bash
mdformat docs/commands.md
```

Expected: command exits 0.

- [ ] **Step 3: Add and run the write-open backstop test**

Add a test proving replacement stops one discoverable writable daemon and the
existing direct-write rejection catches another live writable daemon:

```go
func TestPrepareForegroundServeDaemonLeavesOpenWriteDBBackstop(t *testing.T) {
	dir := runtimeTestDir(t)
	firstHost, firstPort := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		firstHost, firstPort, withRuntimeVersion("1.0.0"),
	))

	secondPID := startSleepProcess(t)
	secondHost, secondPort := testPingServer(t)
	_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		secondHost, secondPort,
		withRuntimePID(secondPID),
		withRuntimeVersion("1.0.0"),
	))
	require.NoError(t, err)

	setTestVersion(t, "1.1.0")
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		removeRuntimeRecordFile(rt.Record)
		return nil
	})

	cont, err := prepareForegroundServeDaemon(
		config.Config{DataDir: dir}, serveReplacementOptions{},
	)
	require.NoError(t, err)
	assert.True(t, cont)

	err = rejectLiveWritableDaemonBeforeDirectWrite(config.Config{DataDir: dir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to write directly")
}
```

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestPrepareForegroundServeDaemonLeavesOpenWriteDBBackstop' -count=1
```

Expected: PASS.

- [ ] **Step 4: Run focused Go tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestServeDaemonReplacement|TestPrepareForegroundServeDaemon|TestServeBackground|TestRunServeBackground|TestEnsureBackgroundServe|TestServeStatus|TestRunServeStatus' -count=1
```

Expected: PASS.

- [ ] **Step 5: Run required Go formatting and vet**

Run:

```bash
go fmt ./...
go vet ./...
```

Expected: both exit 0.

- [ ] **Step 6: Run broader package tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit docs and any final test adjustments**

```bash
git add docs/commands.md README.md cmd/agentsview
git commit -m "docs: document daemon replacement flag"
```

Only stage `README.md` if it changed.

## Final Checklist

- [ ] `agentsview serve --help` lists `--replace`.
- [ ] Foreground `serve` auto-replaces an older release daemon.
- [ ] Foreground `serve` refuses dev/downgrade/forward-version conflicts without
  `--replace`.
- [ ] `serve --replace` stops only confirmed writable daemons.
- [ ] Read-only daemons are never stopped by replacement.
- [ ] `serve --background --replace` works and does not pass `--replace` to the
  child process.
- [ ] `serve status` reports incompatible responding daemons as incompatible,
  not "not responding".
- [ ] Existing `openWriteDB` daemon rejection remains intact as the final
  single-writer backstop.
- [ ] Final verification passes before implementation handoff: `go fmt ./...`,
  `go vet ./...`, and
  `CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -count=1`.
