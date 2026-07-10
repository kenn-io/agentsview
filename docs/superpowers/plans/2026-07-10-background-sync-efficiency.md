# Background Sync Efficiency Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bound watcher-driven sync frequency and restore safe, memory-bounded
Codex JSONL append parsing without losing title, termination, or message
correctness.

**Architecture:** Replace the watcher's idle ticker and synchronous callback
with an event-driven scheduler plus one serialized callback worker. Re-enable
Codex incremental append through the provider facade, using a factory-owned LRU
of exact path/identity/offset continuation states; the database offset remains
the cursor commit token and full parsing remains the fallback.

**Tech Stack:** Go, fsnotify, SQLite/FTS5, testify, Go benchmarks.

______________________________________________________________________

## File map

- `cmd/agentsview/main.go`: production watcher batching and five-second floor.
- `internal/sync/watcher.go`: idle-free timer scheduler and serialized callback
  worker.
- `internal/sync/watcher_test.go`: observable batching, throttling, intake, and
  shutdown behavior.
- `internal/db/sessions.go`: optional authoritative termination status in an
  incremental transaction.
- `internal/db/db_test.go`: incremental status update/clear contract.
- `internal/parser/provider.go`: provider outcome field for authoritative
  incremental termination status.
- `internal/parser/codex_cursor.go`: bounded LRU and exact cursor keys.
- `internal/parser/codex_cursor_test.go`: entry/byte eviction and exact-key
  behavior.
- `internal/parser/codex.go`: digest-based replay state, safe-boundary probe,
  lifecycle seed, and warm/cold continuation.
- `internal/parser/codex_provider.go`: shared cache ownership, capability, file
  identity, and `ParseIncremental` implementation.
- `internal/parser/codex_provider_test.go`: provider contract, boundary, and
  sidecar-independent append coverage.
- `internal/parser/codex_parser_test.go`: warm/cold parser parity and fallback
  behavior.
- `internal/sync/engine.go`: Codex-specific gates, database-selected ID,
  termination handoff, and incremental update fields.
- `internal/sync/engine_integration_test.go`: end-to-end Codex append, title,
  boundary, status, fallback, and write-shape behavior.
- `internal/parser/codex_bench_test.go`: cold-seed versus warm-cursor allocation
  and time evidence.

### Task 1: Event-driven watcher throttling

**Files:**

- Modify: `internal/sync/watcher.go`
- Modify: `internal/sync/watcher_test.go`
- Modify: `cmd/agentsview/main.go`

Use `@superpowers:test-driven-development` and `@testing-without-tautologies`.
Tests must assert callback payloads, ordering, and timing; they must not inspect
source text or timer fields.

- [ ] **Step 1: Add failing batching and floor tests**

Add a test constructor that accepts a short batch delay and minimum interval,
then add tests equivalent to:

```go
func TestWatcherBatchesPathsAndEnforcesDispatchFloor(t *testing.T) {
    calls := make(chan []string, 4)
    w, dir := startTestWatcherWithIntervals(
        t, func(paths []string) { calls <- paths },
        20*time.Millisecond, 150*time.Millisecond,
    )

    writeFile(t, filepath.Join(dir, "a.jsonl"), "a")
    writeFile(t, filepath.Join(dir, "b.jsonl"), "b")
    first := receivePaths(t, calls)
    assert.ElementsMatch(t, []string{
        filepath.Join(dir, "a.jsonl"),
        filepath.Join(dir, "b.jsonl"),
    }, first)

    started := time.Now()
    writeFile(t, filepath.Join(dir, "c.jsonl"), "c")
    second := receivePaths(t, calls)
    assert.GreaterOrEqual(t, time.Since(started), 100*time.Millisecond)
    assert.Contains(t, second, filepath.Join(dir, "c.jsonl"))
}
```

Add a sustained-write test that repeatedly records the same path faster than the
batch delay and still receives it within one minimum interval.

- [ ] **Step 2: Run the focused watcher tests and observe RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
  -run 'TestWatcher(BatchesPathsAndEnforcesDispatchFloor|SustainedWritesProgress)' \
  -count=1
```

Expected: FAIL because the current trailing-edge per-path debounce either
dispatches too frequently or starves the continuously rewritten path.

- [ ] **Step 3: Add a failing nonblocking-intake test**

Block the first callback, write another path while it is blocked, release it,
and assert the second path arrives in a later callback and callback concurrency
never exceeds one:

```go
assert.Equal(t, int32(1), maxConcurrent.Load())
assert.Contains(t, secondBatch, duringCallbackPath)
```

- [ ] **Step 4: Run the intake test and observe RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
  -run TestWatcherContinuesIntakeDuringCallback -count=1
```

Expected: FAIL because the current fsnotify loop executes `onChange`
synchronously.

- [ ] **Step 5: Implement the one-shot scheduler and callback worker**

Keep `NewWatcher` as a compatibility constructor and add a production
constructor accepting both durations:

```go
func NewWatcherWithInterval(
    batchDelay, minInterval time.Duration,
    onChange func([]string), excludes []string,
) (*Watcher, error)
```

Have `NewWatcher(delay, ...)` delegate with `minInterval == delay`. Replace the
always-running ticker with a stopped/nil one-shot timer. Preserve the first
pending timestamp for a path instead of moving it on every write. Compute the
next deadline as:

```go
deadline := firstPendingAt.Add(w.batchDelay)
if floor := lastDispatch.Add(w.minInterval); floor.After(deadline) {
    deadline = floor
}
```

Run callbacks on one worker goroutine. The main loop owns `callbackBusy`, keeps
draining fsnotify while busy, retains pending paths, and schedules or dispatches
them when the worker reports completion. Stop cancels the timer, discards
pending paths, closes the worker input, and waits for an in-flight callback.

- [ ] **Step 6: Replace obsolete white-box watcher tests**

Rewrite tests that directly mutate `w.pending`, replace `w.now`, read
`w.debounce`, or call `w.flush()`. In particular, replace the existing pending
flush/stop, remove-and-rename, and debounce-logic tests with public
filesystem-event tests that assert:

- remove and rename paths arrive in the callback payload;
- a pending one-shot timer is canceled by `Stop` without a post-stop callback;
- `Stop` waits for at most the already-running callback and never dispatches a
  second batch; and
- repeated events are coalesced and dispatched by the observable timing
  contract.

Do not retain production-only compatibility fields or methods solely for old
tests. Keep direct inspection only for the fsnotify watch list, which is the
existing integration boundary for recursive-watch coverage.

- [ ] **Step 7: Configure production for 500 ms batching and a five-second
  floor**

In `cmd/agentsview/main.go`, replace `watcherDebounce` with:

```go
watcherBatchDelay      = 500 * time.Millisecond
watcherSyncMinInterval = 5 * time.Second
```

Construct the watcher through `NewWatcherWithInterval`.

- [ ] **Step 8: Run watcher tests and race coverage**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync -run TestWatcher -count=1
CGO_ENABLED=1 go test -race -tags fts5 ./internal/sync -run TestWatcher -count=1
```

Expected: PASS with no race reports.

- [ ] **Step 9: Commit the watcher change**

Use `@kenn:commit` and commit only the three watcher files:

```bash
git add cmd/agentsview/main.go internal/sync/watcher.go \
  internal/sync/watcher_test.go
git commit -m "perf(sync): throttle watcher batches"
```

### Task 2: Preserve authoritative incremental termination status

**Files:**

- Modify: `internal/db/sessions.go`
- Modify: `internal/db/db_test.go`

Use `@superpowers:test-driven-development` and the real `testDB(t)` helper.

- [ ] **Step 1: Add a failing DB contract test**

Seed a session with `termination_status=tool_call_pending`, then table-test two
incremental updates:

```go
tests := []struct {
    name string
    status *string
    want *string
}{
    {name: "authoritative status", status: ptr("awaiting_user"),
        want: ptr("awaiting_user")},
    {name: "nil clears status", status: nil, want: nil},
}
```

Call `UpdateSessionIncremental`, read the session, and assert the literal stored
status. Keep the existing nil-clears behavior for Claude.

- [ ] **Step 2: Run the DB test and observe RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run TestUpdateSessionIncrementalTerminationStatus -count=1
```

Expected: compile failure because `IncrementalSessionUpdate` has no termination
field.

- [ ] **Step 3: Add the optional field and transactional write**

Add:

```go
TerminationStatus *string
```

to `IncrementalSessionUpdate`, and change the SQL assignment from the hard-coded
`termination_status = NULL` to `termination_status = ?`. Pass the pointer in the
argument list. Nil continues storing SQL NULL; a non-nil pointer stores the
authoritative Codex value.

- [ ] **Step 4: Run DB tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run 'Test(UpdateSessionIncrementalTerminationStatus|.*Incremental.*)' \
  -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the DB contract**

Use `@kenn:commit`:

```bash
git add internal/db/sessions.go internal/db/db_test.go
git commit -m "feat(db): preserve incremental termination status"
```

### Task 3: Build bounded Codex continuation cursors

**Files:**

- Create: `internal/parser/codex_cursor.go`

- Create: `internal/parser/codex_cursor_test.go`

- Modify: `internal/parser/codex.go`

- Modify: `internal/parser/codex_parser_test.go`

- Modify: `internal/parser/codex_provider.go`

- Modify: `internal/parser/codex_provider_test.go`

- Modify: `internal/parser/provider.go`

- [ ] **Step 1: Add failing cursor-cache tests**

Specify the wished-for cache API in same-package tests:

```go
cache := newCodexCursorCache(2, 1024)
cache.Put(key("a", 10), seedA)
cache.Put(key("b", 20), seedB)
_, ok := cache.Get(key("a", 10))
require.True(t, ok)
cache.Put(key("c", 30), seedC)
_, oldOK := cache.Get(key("b", 20))
assert.False(t, oldOK)
```

Also assert exact-offset and exact-known-identity misses, replacement of an
existing key, and rejection/eviction when estimated bytes exceed the configured
budget. Expected values must be literal seeds, not values derived through the
cache implementation.

- [ ] **Step 2: Run cache tests and observe RED**

Run:

```bash
go test ./internal/parser -run TestCodexCursorCache -count=1
```

Expected: compile failure because the cache does not exist.

- [ ] **Step 3: Implement the bounded LRU**

Use `container/list` plus a mutex. Key entries by cleaned path, offset, inode,
and device. Cap production construction at 256 entries and 2 MiB. Estimate
retained bytes from the key path, model, cwd, lifecycle marker, and fixed seed
fields. Never retain messages, JSON lines, or file handles.

- [ ] **Step 4: Add failing digest and warm/cold parity tests**

Extend existing replay tests so the same fixture parses identically via:

1. a full parse that seeds the cursor;
1. a warm append from that cursor; and
1. a fresh provider whose cold path scans the prefix.

Assert literal roles, contents, models, ordinals, replay suppression, and
termination marker. Include the initial-content extractor case used by plugin
and system envelopes.

- [ ] **Step 5: Run parser parity tests and observe RED**

Run:

```bash
go test ./internal/parser \
  -run 'TestCodex(CursorWarmColdParity|PromptReplayDigestParity)' -count=1
```

Expected: FAIL because providers do not share cursor state and the builder keeps
the complete first prompt.

- [ ] **Step 6: Refactor prompt replay state to one digest helper**

Replace retained first-prompt text in both `codexSessionBuilder` and
`codexIncrementalSeed` with a SHA-256 digest plus an explicit seen flag. Route
full parse, `observeUserMessage`, and tail parse through the same helper so the
comparison behavior is identical on warm and cold paths.

Add `lastTaskEvent` to `codexIncrementalSeed`; make prefix reconstruction
observe all lifecycle events handled by the full builder. Add a builder method
that returns a compact seed for cache insertion.

- [ ] **Step 7: Add failing record-boundary tests**

Table-test empty offset, newline-terminated offset, valid JSON without a final
newline, and an offset inside a partial record. Assert that unsafe nonzero
offsets return `IncrementalNeedsFullParse` with `ForceReplace=true` and that the
completed record appears after the fallback full parse.

- [ ] **Step 8: Implement boundary probing and cursor-aware tail parsing**

Add an O(1) helper that opens the file, seeks to `offset-1`, and requires the
byte to equal `\n`. The full parser may seed the cache only at offset zero or a
newline-terminated EOF. Capture the final seed from its existing builder rather
than rescanning.

On incremental parse, look up the exact path/identity/offset cursor. On miss,
scan the prefix once. Parse complete tail records, update the seed, and stage a
new exact-offset entry without deleting the old one.

- [ ] **Step 9: Implement the provider contract**

Have `codexProviderFactory` own the shared cache pointer and pass it to every
provider instance. Set Codex `IncrementalAppend` to `CapabilitySupported`, add
inode/device to its source fingerprint, and implement `ParseIncremental` with:

- truncation and equal-size handling;

- the boundary probe;

- warm/cold seed selection;

- existing retroactive-update fallbacks;

- `ConsumedBytes`, messages, counts, tokens, and ended time; and

- an optional `TerminationStatus *TerminationStatus` on `IncrementalOutcome`.

- [ ] **Step 10: Run all focused parser tests**

Run:

```bash
go test ./internal/parser \
  -run 'Test(CodexCursor|CodexProvider|ParseCodexSessionFrom)' -count=1
```

Expected: PASS.

- [ ] **Step 11: Commit cursor and provider behavior**

Use `@kenn:commit`:

```bash
git add internal/parser/codex_cursor.go \
  internal/parser/codex_cursor_test.go internal/parser/codex.go \
  internal/parser/codex_parser_test.go internal/parser/codex_provider.go \
  internal/parser/codex_provider_test.go internal/parser/provider.go
git commit -m "perf(parser): cache Codex append cursors"
```

### Task 4: Wire safe Codex appends through the sync engine

**Files:**

- Modify: `internal/sync/engine.go`

- Modify: `internal/sync/engine_integration_test.go`

- [ ] **Step 1: Add failing normal-append integration coverage**

Seed a Codex rollout, sync it, append one assistant record, and call
`SyncPaths`. Assert:

```go
assertSessionMessageCount(t, env.db, id, 2)
sess := requireSessionFull(t, env.db, id)
assert.True(t, sess.LastWriteIncremental)
assert.Equal(t, 2, sess.NextOrdinal)
```

Also preserve the first stored message ID across the append to prove the engine
did not replace existing message rows.

- [ ] **Step 2: Run the normal append test and observe RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
  -run TestSyncPathsCodexNormalAppendIsIncremental -count=1
```

Expected: FAIL because Codex currently full-parses and resets
`last_write_incremental`.

- [ ] **Step 3: Add failing title and ForceParse tests**

Cover both:

- transcript growth plus unchanged index title uses incremental append; and

- changed title or per-file `ForceParse` bypasses incremental, refreshes the
  title, and leaves `LastWriteIncremental=false` after full replacement.

- [ ] **Step 4: Add failing lifecycle-status integration tests**

After an initial full sync, append `task_started`, sync, and assert
`tool_call_pending`; then append `task_complete`, sync, and assert
`awaiting_user`. Query through the normal session read so the test protects the
UI/filter-visible contract.

- [ ] **Step 5: Add failing partial-record recovery coverage**

Full-sync a file ending in partial JSON so raw `file_size` is stored. Complete
that record and append another newline-terminated record. `SyncPaths` must
force-replace and preserve both completed records rather than skipping the one
that crossed the stored offset.

- [ ] **Step 6: Generalize the provider incremental adapter**

Keep `providerSingleSessionFresh` explicitly Claude-only. Change the shared
incremental parse callback to receive the `inc.ID` selected by
`GetSessionForIncremental`, and pass that ID in `IncrementalRequest` instead of
calling `claudeSessionIDFromPath`.

Inside `tryProviderIncrementalAppend`, add the Codex-only early gate:

```go
if file.Agent == parser.AgentCodex &&
    (file.ForceParse || e.codexIndexSessionNameChanged(path)) {
    return processResult{forceReplace: true}, false
}
```

Leave Claude's documented per-file `ForceParse` behavior unchanged.

- [ ] **Step 7: Carry termination through the incremental write**

Extend the engine's internal incremental parse/update structs with
`terminationStatus *string`. Convert the provider's optional parser status to a
string pointer and pass it to `db.IncrementalSessionUpdate`. Nil remains the
Claude clear-to-NULL behavior.

- [ ] **Step 8: Run focused engine tests**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
  -run 'Test.*Codex.*(Incremental|Append|Title|Partial|Termination)' -count=1
```

Expected: PASS, including the existing retroactive tool/token/subagent fallback
tests.

- [ ] **Step 9: Commit engine integration**

Use `@kenn:commit`:

```bash
git add internal/sync/engine.go internal/sync/engine_integration_test.go
git commit -m "perf(sync): restore Codex incremental appends"
```

### Task 5: Benchmark and verify the complete change

**Files:**

- Create: `internal/parser/codex_bench_test.go`
- Modify if needed: files from Tasks 1-4 for verified fixes only

Use `@kenn:isolate-prod`: run only tests and benchmarks backed by `t.TempDir()`
and test databases. Do not start the branch daemon against `~/.agentsview` and
do not run `make install`.

- [ ] **Step 1: Add the append cursor benchmark**

Build one large newline-terminated Codex fixture with literal prior turns and a
small appended tail. Benchmark two subcases with `b.ReportAllocs()`:

- `cold_prefix_seed`: a fresh cache reconstructs the prefix;
- `warm_cursor`: a full parse seeds the cache before timed tail parses.

Validate the returned message count outside the timed loop so the benchmark
cannot silently measure a skipped parse.

- [ ] **Step 2: Run the benchmark and record evidence**

Run:

```bash
go test ./internal/parser -run '^$' \
  -bench BenchmarkCodexIncrementalCursor -benchmem -count=5
```

Expected: the warm cursor materially reduces time and allocations versus cold
prefix reconstruction. Report the measured numbers in the handoff; do not add a
flaky performance assertion.

- [ ] **Step 3: Run formatting and focused verification**

Run:

```bash
go fmt ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/parser ./internal/db ./internal/sync
CGO_ENABLED=1 go test -race -tags fts5 ./internal/sync -run TestWatcher -count=1
go vet ./...
```

Expected: all commands pass with clean output.

- [ ] **Step 4: Run the repository Go suite**

Run:

```bash
make test
```

Expected: PASS. If environment-only integration prerequisites prevent a test,
record the exact limitation rather than weakening the test.

- [ ] **Step 5: Commit benchmark or verification-driven fixes**

Use `@kenn:commit`; do not create an empty commit:

```bash
git add internal/parser/codex_bench_test.go
git commit -m "test(parser): benchmark Codex append cursors"
```

- [ ] **Step 6: Run completion verification and requested review repair**

Use `@superpowers:verification-before-completion` and
`@kenn:verify-before-handoff` to rerun fresh verification. Then invoke the
user-requested `@roborev-fix` exactly once. Address discovered open failing
reviews according to that skill, rerun affected tests, and commit any resulting
tracked fixes with `@kenn:commit`.

- [ ] **Step 7: Confirm final repository state**

Run:

```bash
git status --short
git log --oneline --decorate -8
```

Expected: clean worktree and focused commits only; do not push, rebase, change
branches, or merge.
