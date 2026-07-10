# Daemon Lifecycle CLI Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a config-driven, idempotent
`agentsview daemon start|status|stop|restart` interface while retaining the
existing `serve` behavior and adding the explicitly scoped `serve restart`
bridge.

**Architecture:** Keep `serve` as the only server startup implementation. Add
writable-only runtime discovery and lifecycle orchestration around the existing
kit daemon records, background launch lock, daemon start lock, readiness probes,
and PID-safe termination. Canonical daemon commands use fresh configuration and
return structured errors; compatibility `serve` paths retain their existing
runtime-option preservation and broad status/stop scope.

**Tech Stack:** Go, Cobra, `go.kenn.io/kit/daemon`, `gofrs/flock`, testify,
Markdown/Zensical documentation.

______________________________________________________________________

## File map

- Create `cmd/agentsview/daemon.go`: canonical Cobra command and config-driven
  lifecycle orchestration.
- Create `cmd/agentsview/daemon_test.go`: command, status, start, stop, restart,
  contention, and error-path coverage.
- Modify `cmd/agentsview/cli.go`: register `daemon` and the `serve restart`
  bridge.
- Modify `cmd/agentsview/daemon_runtime.go`: tri-state create-time comparison
  and writable-only runtime listing with surfaced I/O errors.
- Modify `cmd/agentsview/daemon_runtime_test.go`: identity-state and writable
  discovery tests.
- Modify `cmd/agentsview/serve_background.go`: reusable background-start
  primitive and a config-only replacement policy.
- Modify `cmd/agentsview/serve_background_test.go`: config-only replacement,
  read-only coexistence, and launch-result tests.
- Modify `cmd/agentsview/serve_lifecycle.go`: share safe stop primitives without
  changing legacy `serve stop` semantics.
- Modify `cmd/agentsview/serve_lifecycle_test.go`: all-target prevalidation and
  stale-record recovery tests.
- Modify user-facing hint sites under `cmd/agentsview/` only where the writable
  daemon is intended.
- Modify `README.md`, `docs/index.md`, `docs/quickstart.md`, `docs/commands.md`,
  `docs/configuration.md`, and `docs/remote-access.md`: make `daemon ...`
  canonical and document compatibility limitations.
- Delete `docs/superpowers/specs/2026-07-10-daemon-lifecycle-cli-design.md` and
  this plan after implementation/review, as explicitly requested.

## Required workflows

- Use `@superpowers:test-driven-development` before implementation code.
- Use `@testing-without-tautologies` for every new or changed test.
- Use `@kenn:isolate-prod` before building or running the daemon; all live
  checks must set a temporary `AGENTSVIEW_DATA_DIR`.
- Use `@kenn:commit` for every commit and obey the repository rule to commit
  every turn that changes tracked files.
- After implementation validation, use the explicitly requested `@roborev-fix`
  workflow until open failing reviews are resolved.
- Before pushing a public PR, use `@kenn:scrub-private-data`, then
  `@kenn:commit-push-pr`. Do not merge the PR.

### Task 1: Make runtime identity and writable discovery explicit

**Files:**

- Modify: `cmd/agentsview/daemon_runtime.go`

- Test: `cmd/agentsview/daemon_runtime_test.go`

- [ ] **Step 1: Write failing tri-state identity tests**

Add table-driven testify coverage for exact match, proven mismatch, missing
value, malformed value, and unavailable live lookup. Model the contract with:

```go
type processCreateTimeState int

const (
	processCreateTimeUnknown processCreateTimeState = iota
	processCreateTimeMatch
	processCreateTimeMismatch
)
```

The unavailable-lookup case can call the pure comparator with `liveOK=false`; do
not depend on a platform-specific failing process lookup.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestCompareProcessCreateTime|TestWritableDaemonRecords' -count=1
```

Expected: FAIL because the comparator and writable listing do not exist.

- [ ] **Step 3: Implement tri-state comparison**

Add a pure comparison helper and make existing boolean identity checks wrappers:

```go
func compareProcessCreateTime(recorded string, live int64, liveOK bool) processCreateTimeState {
	recordedMillis, err := strconv.ParseInt(recorded, 10, 64)
	if err != nil || recordedMillis <= 0 || !liveOK || live <= 0 {
		return processCreateTimeUnknown
	}
	if recordedMillis == live {
		return processCreateTimeMatch
	}
	return processCreateTimeMismatch
}

func processCreateTimeStateForPID(pid int, recorded string) processCreateTimeState {
	live, ok := processCreateTimeMillis(pid)
	return compareProcessCreateTime(recorded, live, ok)
}
```

Change mismatched-record cleanup to delete only on `processCreateTimeMismatch`;
missing, malformed, zero, negative, and OS-unavailable values remain unknown.

- [ ] **Step 4: Add writable-only discovery with errors**

Introduce a helper shaped like:

```go
func writableDaemonRecords(dataDir string) ([]daemon.RuntimeRecord, error)
```

It must migrate legacy records, clean dead records, surface runtime-store list
failures, filter non-AgentsView/read-only/dead records, remove only proven
PID-reuse records, and return every remaining live writable record. Preserve
`SourcePath` for manual recovery output.

- [ ] **Step 5: Verify GREEN and legacy behavior**

Run the focused command above plus:

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestLiveDaemonRecords|TestProcessIdentity|TestRuntimeRecord' -count=1
```

Expected: PASS; legacy broad discovery tests remain unchanged.

- [ ] **Step 6: Commit the runtime primitive**

Commit with `@kenn:commit`, for example:

```text
refactor(cli): distinguish writable daemon runtime identity
```

### Task 2: Extract a config-only background-start policy

**Files:**

- Modify: `cmd/agentsview/serve_background.go`

- Test: `cmd/agentsview/serve_background_test.go`

- [ ] **Step 1: Write failing policy tests**

Cover these observable cases:

- Existing `serve --background` automatic replacement still preserves an old
  runtime's `NoSync` option.

- Canonical/config-only start replaces the same older daemon without adding
  `--no-sync` to child arguments.

- A read-only runtime does not satisfy writable start; a writable child is
  spawned.

- The shared launch function returns whether it used an existing daemon or
  started a child, and returns startup errors with the log path instead of
  terminating the process internally.

- [ ] **Step 2: Run focused tests and verify RED**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestRunServeBackground.*ConfigOnly|TestBackgroundStartResult|TestRunServeBackground.*ReadOnly' -count=1
```

Expected: FAIL because the config-only policy/result API is absent.

- [ ] **Step 3: Add policy and result types**

Extend the internal launch options without changing zero-value compatibility
behavior:

```go
type backgroundLaunchPolicy struct {
	ConfigOnly bool
	Operation  string
}

type backgroundLaunchResult struct {
	Runtime *DaemonRuntime
	Started bool
	LogPath string
}
```

Extract an error-returning lower-level launch function from
`runServeBackground`. Keep the existing wrapper responsible for legacy `fatal`
behavior so current callers and output remain stable.

- [ ] **Step 4: Prevent runtime-option adoption for config-only starts**

Guard every `adoptDaemonRuntimeLaunchOptions` call used by the shared start
path:

```go
if !policy.ConfigOnly {
	adoptDaemonRuntimeLaunchOptions(&cfg, decision.Runtime)
}
```

Config-only child arguments start from `[]string{"serve"}` and must never append
runtime-derived `--no-sync`.

- [ ] **Step 5: Verify focused and existing background tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestRunServeBackground|TestEnsureBackgroundServe|TestServeBackground' -count=1
```

Expected: PASS, including compatibility replacement tests.

- [ ] **Step 6: Commit the reusable launch policy**

Use `@kenn:commit`, for example:

```text
refactor(cli): support config-only daemon launches
```

### Task 3: Add canonical daemon command, start, and status

**Files:**

- Create: `cmd/agentsview/daemon.go`

- Create: `cmd/agentsview/daemon_test.go`

- Modify: `cmd/agentsview/cli.go`

- [ ] **Step 1: Write failing command-surface tests**

Test root help and `daemon --help` for `start`, `status`, `stop`, and `restart`;
reject positional arguments and serve flags such as `--host` and `--no-sync`.
Assert `daemon` is in the core group.

- [ ] **Step 2: Verify command tests fail**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestDaemonCommand|TestRootHelpShows.*Daemon' -count=1
```

Expected: FAIL because `daemon` is unknown.

- [ ] **Step 3: Register the Cobra tree**

Add `newDaemonCommand()` to `newRootCommand()`. Each child uses `cobra.NoArgs`,
has no serve-specific flags, and returns errors through `RunE`.

- [ ] **Step 4: Implement config-driven start**

Resolve/create the data directory, take the background launch lock before
writable config loading, and invoke the config-only background-start primitive
with child args `[]string{"serve"}`. If lock contention resolves to a published
writable daemon, report it and exit zero. A persistent start lock returns
PID/log manual recovery details without spawning.

Spell out every bounded contention outcome:

- If the lock owner publishes a writable runtime, report it and exit zero.

- If the launch lock remains held and the daemon start lock/snapshot exists,
  return startup-in-progress with PID/log recovery details.

- If the launch lock remains held without a start-lock snapshot, return a
  retryable launch-in-progress error that identifies the launch lock; do not
  load or write config.

- If the launch lock clears without a writable runtime or active start lock,
  return a startup-failed error. Do not treat it as success and do not start a
  second launch in the same invocation.

- [ ] **Step 5: Write and implement status behavior**

Test and implement:

- stopped and read-only-only both report no writable daemon;
- one writable daemon renders URL/PID/version/uptime;
- startup state renders PID/elapsed/phase/log;
- incompatible writable state remains visible;
- multiple writable records are all listed with a single-writer warning;
- config/runtime-store failures return nonzero, while every inspected state
  returns zero.

Use `writableDaemonRecords` rather than first-record discovery. Reuse rendering
helpers where their wording still fits.

- [ ] **Step 6: Verify start/status tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestDaemon(Command|Start|Status)|TestRootHelpShows.*Daemon' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit canonical start/status**

Use `@kenn:commit`, for example:

```text
feat(cli): add daemon start and status
```

### Task 4: Implement all-or-nothing daemon stop and restart

**Files:**

- Modify: `cmd/agentsview/daemon.go`

- Modify: `cmd/agentsview/serve_lifecycle.go`

- Test: `cmd/agentsview/daemon_test.go`

- Test: `cmd/agentsview/serve_lifecycle_test.go`

- [ ] **Step 1: Write failing stop tests**

Cover idempotent stopped state, mixed writable/read-only records, multiple
writable targets, launch-lock contention, start lock held alongside confirmed
records, persistent start-lock fallback with startup PID/log output, proven
PID-reuse cleanup, legacy/malformed/unavailable identity recovery, and
all-target prevalidation before signalling any process. Also force configuration
loading to fail and assert that no target is signalled.

Use injected stop functions/channels and actual runtime fixtures; do not assert
implementation source text.

- [ ] **Step 2: Run stop tests and verify RED**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestDaemonStop' -count=1
```

Expected: FAIL because canonical stop is absent.

- [ ] **Step 3: Extract safe stop helpers**

Add an error-returning helper that prevalidates all records before stopping any:

```go
func stopConfirmedWritableDaemons(cfg config.Config, records []daemon.RuntimeRecord) (int, error)
```

On an unconfirmed record, include PID and `SourcePath` plus manual
verification/termination guidance. Do not change `runServeStop`; it retains
broad, best-effort legacy behavior.

- [ ] **Step 4: Implement canonical stop**

Hold the launch lock across discovery and stop. Check `IsDaemonStarting`
unconditionally before records. On a held start lock, include startup snapshot
details and persistent-publication-failure guidance, signal nothing, and return
nonzero. Otherwise discover/clean writable records, prevalidate all, stop them,
clean orphaned Caddy children, and report idempotent results.

- [ ] **Step 5: Write failing restart tests**

Cover running and stopped states, distinct `restarted` versus
`started (was not running)` output, current config replacing old runtime
settings, read-only processes left alive, database/config validation before
stop, lock held across the stop/start gap, persistent start-lock refusal,
unconfirmed-record refusal, and child startup failure with log path.

- [ ] **Step 6: Implement restart**

Restart owns the launch lock for the full operation, loads and validates fresh
configuration, checks the start lock, runs `db.CheckDataVersion` before
stopping, discovers/cleans/prevalidates writable records, stops them, and calls
the config-only start primitive without reacquiring the lock. It never adopts
old `NoSync` and never signals a startup-snapshot PID.

- [ ] **Step 7: Verify lifecycle tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestDaemon(Stop|Restart)|TestStopConfirmedWritable' -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit canonical stop/restart**

Use `@kenn:commit`, for example:

```text
feat(cli): add safe daemon stop and restart
```

### Task 5: Add the scoped `serve restart` bridge and canonical hints

**Files:**

- Modify: `cmd/agentsview/cli.go`

- Modify: `cmd/agentsview/main.go`

- Modify: `cmd/agentsview/write_lock.go`

- Modify: `cmd/agentsview/serve_replacement.go`

- Modify: `cmd/agentsview/serve_background.go`

- Modify: `cmd/agentsview/transport.go`

- Test: `cmd/agentsview/daemon_test.go`

- Test: affected existing `*_test.go` files

- [ ] **Step 1: Write failing bridge/help tests**

Assert `serve restart` exists, takes no serve flags, delegates to writable
config-driven restart, and leaves read-only runtimes untouched. Help must state
that its scope differs from broad legacy `serve stop`.

- [ ] **Step 2: Implement the bridge**

Register a `serve restart` command that calls the same restart function as
`daemon restart`. Do not implement it as `serve stop` plus start.

- [ ] **Step 3: Update writable-daemon recovery hints**

Prefer `agentsview daemon stop`, `agentsview daemon restart`, or
`agentsview daemon status` where the message concerns the writable SQLite
daemon. Keep `serve stop` wording where broad legacy server cleanup is
intentionally described, and keep update internals on `serve --background` where
runtime flags must be preserved.

- [ ] **Step 4: Verify compatibility tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -run 'TestServe(CommandHasLifecycleSubcommands|Restart)|TestServeBackground|TestServeReplacement|TestWriteLock' -count=1
```

Expected: PASS; existing `serve --background`, `serve status`, and broad
`serve stop` behavior is unchanged.

- [ ] **Step 5: Commit bridge and guidance**

Use `@kenn:commit`, for example:

```text
feat(cli): add serve restart compatibility bridge
```

### Task 6: Update README and Zensical documentation

**Files:**

- Modify: `README.md`

- Modify: `docs/index.md`

- Modify: `docs/quickstart.md`

- Modify: `docs/commands.md`

- Modify: `docs/configuration.md`

- Modify: `docs/remote-access.md`

- [ ] **Step 1: Update canonical examples**

Change primary managed-daemon examples to:

```bash
agentsview daemon start
agentsview daemon status
agentsview daemon restart
agentsview daemon stop
```

Describe bare `serve` as foreground operation.

- [ ] **Step 2: Document compatibility boundaries**

Retain visible documentation for `serve --background`, `serve status`, and
`serve stop`. Explain that `serve --background` remains necessary for flag-only
`--no-sync` and one-off unauthenticated non-loopback `--host`; `serve restart`
is writable-only and intentionally differs from broad legacy `serve stop`.

- [ ] **Step 3: Update config/remote guidance**

Describe `daemon_idle_timeout` in terms of detached writable daemons. Use
`daemon start` for persistent authenticated config-driven remote nodes; retain
`serve --background --host ...` examples where one-off flag overrides are the
point.

- [ ] **Step 4: Format and build Zensical docs**

```bash
mdformat README.md docs/index.md docs/quickstart.md docs/commands.md docs/configuration.md docs/remote-access.md
make docs-build
make docs-check
```

Expected: both docs commands pass with no broken links or generated-site errors.

- [ ] **Step 5: Commit documentation**

Use `@kenn:commit`, for example:

```text
docs: document canonical daemon lifecycle
```

### Task 7: Run full validation and isolated live CLI verification

**Files:** none expected unless verification exposes defects.

- [ ] **Step 1: Run Go formatting and static checks**

```bash
go fmt ./...
go vet ./...
make lint
```

Expected: PASS.

- [ ] **Step 2: Run repository tests**

```bash
make test
```

Expected: PASS.

- [ ] **Step 3: Build without touching production state**

Follow `@kenn:isolate-prod`; build the branch binary without `make install`.

```bash
make build
```

Expected: `./agentsview` is produced successfully.

- [ ] **Step 4: Exercise the real CLI against a temporary data directory**

Use a shell-created temporary directory and a cleanup trap. Never point the
branch binary at `~/.agentsview`.

```bash
tmpdir="$(mktemp -d)"
trap 'AGENTSVIEW_DATA_DIR="$tmpdir" ./agentsview daemon stop >/dev/null 2>&1 || true; rm -rf "$tmpdir"' EXIT
export AGENTSVIEW_DATA_DIR="$tmpdir"
./agentsview daemon status
./agentsview daemon start
./agentsview daemon status
./agentsview daemon restart
./agentsview daemon stop
./agentsview daemon stop
```

Observe stopped → running → restarted → stopped, confirm idempotent second stop,
and inspect the runtime directory to ensure cleanup. Also start a read-only test
server if practical and confirm writable lifecycle commands leave it alone.

- [ ] **Step 5: Re-run focused tests after any fixes**

If live verification exposes defects, return to RED/GREEN, commit fixes with
`@kenn:commit`, then repeat all validation commands.

### Task 8: Run the explicitly requested roborev remediation pass

**Files:** determined by review findings.

- [ ] **Step 1: Invoke `@roborev-fix`**

Use the skill exactly as documented to discover and fix open failing reviews for
the current branch. This invocation is explicitly authorized by the user.

- [ ] **Step 2: Verify every proposed finding against current code**

Do not accept stale or incorrect findings blindly. Use the review workflow's
required tests and responses, and preserve the writable/read-only and
compatibility contracts from the approved spec.

- [ ] **Step 3: Re-run affected tests and full required validation**

At minimum rerun focused `cmd/agentsview` tests, `go fmt ./...`, `go vet ./...`,
and `make test`; rerun docs checks for documentation changes.

- [ ] **Step 4: Commit review fixes**

Use the roborev workflow and `@kenn:commit` requirements. Do not invoke any
other roborev review command unless required by the explicitly requested
`roborev-fix` skill.

### Task 9: Burn down temporary planning documents and open the PR

**Files:**

- Delete: `docs/superpowers/specs/2026-07-10-daemon-lifecycle-cli-design.md`

- Delete: `docs/superpowers/plans/2026-07-10-daemon-lifecycle-cli.md`

- [ ] **Step 1: Remove superpowers artifacts**

Delete the design and plan after implementation and review are complete. Confirm
`docs/superpowers` contains no other user-owned files before removing empty
directories.

- [ ] **Step 2: Commit the cleanup**

Use `@kenn:commit`, for example:

```text
chore: remove completed implementation artifacts
```

- [ ] **Step 3: Scrub public-bound changes**

Run `@kenn:scrub-private-data` over `origin/main...HEAD`, including code, tests,
commit messages, and Zensical docs. Remove or replace any private machine names,
paths, tokens, URLs, or user data before pushing. If scrubbing changes tracked
files, commit those changes with `@kenn:commit` before continuing.

- [ ] **Step 4: Run final verification from the exact PR HEAD**

```bash
git status --short
git diff --check origin/main...HEAD
go vet ./...
make test
make docs-check
```

Expected: clean working tree and all commands pass.

- [ ] **Step 5: Push and open a pull request**

Run `@kenn:commit-push-pr`, which will use `@kenn:commit` if any final tracked
changes exist, push the current branch, and open a rationale-first PR. Enter
this step with a clean working tree so the verified HEAD is the pushed HEAD. If
the workflow creates an unexpected commit, stop and repeat the scrub and final
verification before opening the PR. The PR description must be summary-only with
no test plan/checklist/verification section, per `AGENTS.md`.

- [ ] **Step 6: Report status without merging**

Return the PR URL, branch/commit summary, checks run, any checks still pending,
and any limitations. Do not merge the PR.
