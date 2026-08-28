# Full Resync Finalization Progress Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the stale full-resync 100% session counter with precise
progress for every remaining finalization operation.

**Architecture:** Add an enum-free `finalizing` progress phase and a single
engine helper that emits it only for `syncWriteBulk`. Instrument the terminal
file-sync epilogue and the later database-backed tail without changing operation
order, and keep contributor finalization events free of cumulative counters.
Existing transports consume the same `Progress` payload; every detail begins
with `Finalizing sync:` so the desktop splash accepts it.

**Tech Stack:** Go 1.26, SQLite/FTS5 test harness, Testify, Rust/Tauri desktop
unit tests.

## Global Constraints

- Work only in `.worktrees/fix-resync-finalization-progress` on branch
  `fix/resync-finalization-progress`.
- Emit finalization only for `syncWriteBulk`; ordinary full, cutoff, scoped,
  watcher, and reconciliation passes keep existing progress.
- Preserve write order, transactions, cancellation, memory policy, database
  contents, and database swap behavior.
- Use the eight exact `Finalizing sync:` details from the approved design.
- Finalization events carry zero session and message counters, including
  contributor events.
- Emit the terminal-write detail only when `pending` is non-empty and only at
  the `flush:` call site.
- Emit the memory detail from the existing deferred scavenge owner and only when
  `scavengePending` is true.
- Add no dependencies and no compatibility scaffolding.
- Use real temporary SQLite databases and existing seams; do not mock the engine
  under test.

______________________________________________________________________

### Task 1: Emit bulk-resync finalization progress

**Files:**

- Modify: `internal/sync/progress.go:10-23`
- Modify: `internal/sync/engine.go:1477-1499`
- Modify: `internal/sync/engine.go:3094-3112`
- Modify: `internal/sync/engine.go:7370-7478`
- Modify: `internal/sync/engine.go:9247-10068`
- Test: `internal/sync/parse_retention_test.go`
- Test: `internal/sync/rebuild_contributor_test.go:76-173`
- Test: `internal/sync/engine_integration_test.go:4700-4805`

**Interfaces:**

- Consumes: `Engine.reportProgress(ProgressFunc, Progress)`, `syncWriteMode`,
  `syncWriteBulk`, `parseRetentionBudget.scavengePending`, and the existing
  `writeBatchOverride` and `bulkRetentionBudget.scavenge` test seams.

- Produces: `PhaseFinalizing Phase = "finalizing"` and
  `Engine.reportFinalizingProgress(onProgress ProgressFunc, writeMode syncWriteMode, detail string)`.

- Produces these exact detail strings, in order for a parse-bearing local
  resync: `Finalizing sync: committing session writes`,
  `Finalizing sync: saving session source state`,
  `Finalizing sync: linking file-backed subagent sessions`,
  `Finalizing sync: repairing subagent relationships`,
  `Finalizing sync: releasing parsed-session memory`,
  `Finalizing sync: checking database-backed sessions`,
  `Finalizing sync: linking all subagent sessions`,
  `Finalizing sync: saving the skip cache`.

- [ ] **Step 1: Write the failing terminal-flush and bulk-gating test**

Add this test beside the existing retention-through-write tests in
`internal/sync/parse_retention_test.go`. It blocks the real collector at its
write seam and observes the last progress delivered before the write starts. The
`batchSize` case proves a mid-loop flush does not receive the finalization
label.

```go
func TestCollectAndBatchReportsFinalizingOnlyBeforeBulkTerminalFlush(t *testing.T) {
	tests := []struct {
		name       string
		mode       syncWriteMode
		jobs       int
		wantPhase  Phase
		wantDetail string
	}{
		{
			name: "bulk terminal flush", mode: syncWriteBulk, jobs: 1,
			wantPhase: Phase("finalizing"),
			wantDetail: "Finalizing sync: committing session writes",
		},
		{
			name: "bulk mid-loop flush", mode: syncWriteBulk, jobs: batchSize,
			wantPhase: PhaseSyncing,
		},
		{
			name: "default terminal flush", mode: syncWriteDefault, jobs: 1,
			wantPhase: PhaseSyncing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
			t.Cleanup(engine.Close)
			writeEntered := make(chan struct{})
			allowWrite := make(chan struct{})
			engine.writeBatchOverride = func(
				batch []pendingWrite, _ syncWriteMode, _ bool,
			) (int, int, int, int) {
				close(writeEntered)
				<-allowWrite
				return len(batch), 0, 0, 0
			}
			results := make(chan syncJob, tt.jobs)
			for i := range tt.jobs {
				results <- syncJob{
					path: fmt.Sprintf("/sessions/%03d.jsonl", i),
					results: []parser.ParseResult{{Session: parser.ParsedSession{
						ID: fmt.Sprintf("session-%03d", i), Agent: parser.AgentClaude,
					}}},
				}
			}
			close(results)
			progress := make(chan Progress, tt.jobs+8)
			done := make(chan struct{})
			go func() {
				engine.collectAndBatch(
					t.Context(), results, tt.jobs, tt.jobs,
					func(p Progress) { progress <- p }, tt.mode,
				)
				close(done)
			}()
			select {
			case <-writeEntered:
			case <-time.After(time.Second):
				require.FailNow(t, "collector did not enter write")
			}
			var last Progress
		drain:
			for {
				select {
				case last = <-progress:
				default:
					break drain
				}
			}
			assert.Equal(t, tt.wantPhase, last.Phase)
			assert.Equal(t, tt.wantDetail, last.Detail)
			close(allowWrite)
			select {
			case <-done:
			case <-time.After(time.Second):
				require.FailNow(t, "collector did not finish")
			}
		})
	}
}
```

- [ ] **Step 2: Write the failing ordered-memory and resync-tail assertions**

Add this test to `internal/sync/parse_retention_test.go`. It installs the real
bulk budget, makes a scavenge necessary, and blocks inside the injected scavenge
until the test observes the preceding progress event.

```go
func TestCollectAndBatchReportsOrderedBulkFinalization(t *testing.T) {
	engine := NewEngine(openTestDB(t), EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	restore := engine.beginBulkRetentionPass()
	defer restore()
	budget := engine.retentionBudget()
	lease, err := budget.acquire(t.Context(), 1)
	require.NoError(t, err)

	scavengeEntered := make(chan struct{})
	allowScavenge := make(chan struct{})
	budget.scavenge = func() {
		close(scavengeEntered)
		<-allowScavenge
	}
	engine.writeBatchOverride = func(
		batch []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		return len(batch), 0, 0, 0
	}
	results := make(chan syncJob, 1)
	results <- syncJob{
		path: "/sessions/one.jsonl", retentionLease: lease,
		results: []parser.ParseResult{{Session: parser.ParsedSession{
			ID: "one", Agent: parser.AgentClaude,
		}}},
	}
	close(results)
	progress := make(chan Progress, 16)
	done := make(chan struct{})
	go func() {
		engine.collectAndBatch(
			t.Context(), results, 1, 1,
			func(p Progress) { progress <- p }, syncWriteBulk,
		)
		close(done)
	}()
	select {
	case <-scavengeEntered:
	case <-time.After(time.Second):
		require.FailNow(t, "collector did not enter memory scavenge")
	}
	var events []Progress
drain:
	for {
		select {
		case event := <-progress:
			events = append(events, event)
		default:
			break drain
		}
	}
	require.NotEmpty(t, events)
	assert.Equal(t, "Finalizing sync: releasing parsed-session memory",
		events[len(events)-1].Detail)
	close(allowScavenge)
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "collector did not finish after memory scavenge")
	}
	var details []string
	for _, event := range events {
		if event.Phase == Phase("finalizing") {
			details = append(details, event.Detail)
		}
	}
	assert.Equal(t, []string{
		"Finalizing sync: committing session writes",
		"Finalizing sync: saving session source state",
		"Finalizing sync: linking file-backed subagent sessions",
		"Finalizing sync: repairing subagent relationships",
		"Finalizing sync: releasing parsed-session memory",
	}, details)
}
```

Extend the resync portion of `TestSyncEngineProgress` in
`internal/sync/engine_integration_test.go` with this literal filter and
assertion:

```go
	var finalizingDetails []string
	for _, event := range resyncEvents {
		if event.Phase != sync.Phase("finalizing") {
			continue
		}
		assert.True(t, event.Resync)
		assert.Zero(t, event.SessionsTotal)
		assert.Zero(t, event.SessionsDone)
		assert.Zero(t, event.MessagesIndexed)
		finalizingDetails = append(finalizingDetails, event.Detail)
	}
assert.Equal(t, []string{
	"Finalizing sync: committing session writes",
	"Finalizing sync: saving session source state",
	"Finalizing sync: linking file-backed subagent sessions",
	"Finalizing sync: repairing subagent relationships",
	"Finalizing sync: releasing parsed-session memory",
	"Finalizing sync: checking database-backed sessions",
	"Finalizing sync: linking all subagent sessions",
	"Finalizing sync: saving the skip cache",
}, finalizingDetails)
```

Extend `TestResyncContributorsRunInOrderWithCumulativeProgress` in
`internal/sync/rebuild_contributor_test.go`. Add this map beside
`progressByContributor`, append inside `onProgress`, and assert it after the
existing final `PhaseDone` expectations:

```go
finalizingByContributor := map[string][]Progress{}

onProgress := func(p Progress) {
	if p.Detail == "A" || p.Detail == "B" {
		progressByContributor[p.Detail] = p
		if p.Phase == Phase("finalizing") {
			finalizingByContributor[p.Detail] = append(
				finalizingByContributor[p.Detail], p,
			)
		}
	}
}

for _, contributor := range []string{"A", "B"} {
	events := finalizingByContributor[contributor]
	require.NotEmpty(t, events)
	for _, event := range events {
		assert.Zero(t, event.SessionsTotal)
		assert.Zero(t, event.SessionsDone)
		assert.Zero(t, event.MessagesIndexed)
	}
}
```

- [ ] **Step 3: Run the focused tests and verify RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
  -run '^(TestCollectAndBatchReportsFinalizingOnlyBeforeBulkTerminalFlush|TestCollectAndBatchReportsOrderedBulkFinalization|TestResyncContributorsRunInOrderWithCumulativeProgress|TestSyncEngineProgress)$' \
  -count=1
```

Expected: FAIL because the bulk terminal flush still reports `PhaseSyncing`, no
ordered finalization events exist, and contributor events still receive
cumulative counters.

- [ ] **Step 4: Add the phase, exact labels, and bulk-only reporter**

Add `PhaseFinalizing` in `internal/sync/progress.go`:

```go
PhaseFinalizing Phase = "finalizing"
```

Add the exact detail constants near the sync write-mode declarations in
`internal/sync/engine.go`:

```go
const (
	finalizingSessionWritesDetail = "Finalizing sync: committing session writes"
	finalizingSourceStateDetail = "Finalizing sync: saving session source state"
	finalizingFileLinksDetail = "Finalizing sync: linking file-backed subagent sessions"
	finalizingParentRepairDetail = "Finalizing sync: repairing subagent relationships"
	finalizingMemoryDetail = "Finalizing sync: releasing parsed-session memory"
	finalizingDBBackedDetail = "Finalizing sync: checking database-backed sessions"
	finalizingAllLinksDetail = "Finalizing sync: linking all subagent sessions"
	finalizingSkipCacheDetail = "Finalizing sync: saving the skip cache"
)
```

Add this method beside `reportProgress`:

```go
func (e *Engine) reportFinalizingProgress(
	onProgress ProgressFunc, writeMode syncWriteMode, detail string,
) {
	if writeMode != syncWriteBulk {
		return
	}
	e.reportProgress(onProgress, Progress{
		Phase: PhaseFinalizing, Detail: detail,
	})
}
```

- [ ] **Step 5: Instrument the file-sync epilogue without changing its order**

Replace `defer budget.scavengeIfNeeded()` in `collectAndBatchWithOptions` with:

```go
defer func() {
	if writeMode == syncWriteBulk && budget.scavengePending.Load() {
		e.reportFinalizingProgress(
			onProgress, writeMode, finalizingMemoryDetail,
		)
	}
	budget.scavengeIfNeeded()
}()
```

At the `flush:` label, add only these reports around the existing operations:

```go
flush:
	if len(pending) > 0 {
		e.reportFinalizingProgress(
			onProgress, writeMode, finalizingSessionWritesDetail,
		)
	}
	flushPending()
	if ctx.Err() != nil && e.discardWritesOnCancel {
		return stats
	}
	e.reportFinalizingProgress(
		onProgress, writeMode, finalizingSourceStateDetail,
	)
	flushBaselineSources()
```

Immediately before the existing file-backed link and repair calls, add:

```go
if deferred, _ := ctx.Value(deferGlobalLinkContextKey{}).(bool); !deferred {
	e.reportFinalizingProgress(
		onProgress, writeMode, finalizingFileLinksDetail,
	)
	if err := e.linkSubagentSessions(postWriteCtx); err != nil {
		log.Printf("link subagent sessions: %v", err)
		stats.RecordFailed()
	}
}
e.reportFinalizingProgress(
	onProgress, writeMode, finalizingParentRepairDetail,
)
if err := e.db.RepairQueuedSubagentParentsContext(postWriteCtx); err != nil {
	log.Printf("repair queued subagent parents: %v", err)
	stats.RecordFailed()
}
```

- [ ] **Step 6: Instrument the post-scavenge tail and exclude contributor
  counters**

In the contributor wrapper, keep the contributor-specific `Progress` mapper but
gate all cumulative counters:

```go
if contributor.Progress != nil {
	p = contributor.Progress(p)
}
if p.Phase != PhaseFinalizing {
	p.SessionsDone += stats.TotalSessions
	p.SessionsTotal += stats.TotalSessions
	p.MessagesIndexed += stats.messagesIndexed
}
reportResyncProgress(p)
```

In `syncAllLocked`, after the final cancellation check and immediately before
the database-backed provider blocks, add:

```go
e.reportFinalizingProgress(
	onProgress, writeMode, finalizingDBBackedDetail,
)
```

Immediately before the second `LinkSubagentSessions` call and skip-cache
persistence, add:

```go
e.reportFinalizingProgress(
	onProgress, writeMode, finalizingAllLinksDetail,
)
if err := e.db.LinkSubagentSessions(); err != nil {
	log.Printf("link subagent sessions: %v", err)
}

e.reportFinalizingProgress(
	onProgress, writeMode, finalizingSkipCacheDetail,
)
tPersist := time.Now()
skipCount := e.persistSkipCache()
if verbose {
	log.Printf(
		"persist skip cache (%d entries): %s",
		skipCount,
		time.Since(tPersist).Round(time.Millisecond),
	)
}
```

- [ ] **Step 7: Run focused and package verification**

Run:

```bash
go fmt ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
  -run '^(TestCollectAndBatchReportsFinalizingOnlyBeforeBulkTerminalFlush|TestCollectAndBatchReportsOrderedBulkFinalization|TestResyncContributorsRunInOrderWithCumulativeProgress|TestSyncEngineProgress)$' \
  -count=1
CGO_ENABLED=1 go test -tags fts5 ./internal/sync/... -count=1
go vet ./...
```

Expected: every command exits 0. Review `git diff --check`, `git diff --stat`,
and `git diff` to confirm only the planned phase, engine, and test changes
exist.

- [ ] **Step 8: Commit the engine behavior**

Use the required commit and private-data scrub workflows. Stage only the files
listed in Task 1 and commit with a message focused on why the stale 100%
interval needed named work:

```bash
git add internal/sync/progress.go internal/sync/engine.go \
  internal/sync/parse_retention_test.go \
  internal/sync/rebuild_contributor_test.go \
  internal/sync/engine_integration_test.go
git commit
```

### Task 2: Protect command-line and desktop rendering contracts

**Files:**

- Test: `cmd/agentsview/sync_test.go:1360-1410`
- Test: `desktop/src-tauri/src/lib.rs:3806-3860`

**Interfaces:**

- Consumes: `PhaseFinalizing`, `resyncProgressPrinter.Print`, and
  `extract_startup_status`.

- Produces: regression coverage that finalization is a timed named command-line
  step and that all eight labels pass the unchanged desktop startup filter.

- [ ] **Step 1: Extend the command-line progress-printer contract**

In `TestResyncProgressPrinterWritesPhaseTimingsOnNewLines`, insert a
finalization event after the 100% syncing event and before search rebuild:

```go
now = now.Add(350 * time.Millisecond)
printer.Print(agentsync.Progress{
	Phase:  agentsync.PhaseFinalizing,
	Detail: "Finalizing sync: committing session writes",
	Resync: true,
})
```

Advance `now` by 500 milliseconds before the search event, then assert:

```go
assert.Contains(t, got,
	"  Finalizing sync: committing session writes...\n")
assert.Contains(t, got,
	"  Finalizing sync: committing session writes completed in 500ms\n")
assert.NotContains(t, got,
	"\r  Finalizing sync: committing session writes")
```

- [ ] **Step 2: Extend the desktop startup-filter contract**

In `extract_startup_status_parses_progress_and_messages`, add an inline loop
with literal expected values and no new helper:

```rust
for detail in [
    "Finalizing sync: committing session writes",
    "Finalizing sync: saving session source state",
    "Finalizing sync: linking file-backed subagent sessions",
    "Finalizing sync: repairing subagent relationships",
    "Finalizing sync: releasing parsed-session memory",
    "Finalizing sync: checking database-backed sessions",
    "Finalizing sync: linking all subagent sessions",
    "Finalizing sync: saving the skip cache",
] {
    assert_eq!(
        extract_startup_status(&format!("\r  {detail}")),
        Some(detail.to_string())
    );
}
```

- [ ] **Step 3: Run consumer and full relevant verification**

Run:

```bash
go fmt ./...
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview \
  -run '^TestResyncProgressPrinterWritesPhaseTimingsOnNewLines$' -count=1
cargo test --locked --manifest-path desktop/src-tauri/Cargo.toml --lib \
  extract_startup_status_parses_progress_and_messages
CGO_ENABLED=1 go test -tags fts5 ./internal/sync/... ./cmd/agentsview/... -count=1
go vet ./...
```

Expected: every command exits 0. Review `git diff --check`, `git diff --stat`,
and the complete branch diff from `origin/main`. Confirm `main` remains
unchanged and the implementation worktree is clean except for the two planned
test files.

- [ ] **Step 4: Commit the consumer contracts**

Use the required commit and private-data scrub workflows. Stage only the two
Task 2 test files and commit:

```bash
git add cmd/agentsview/sync_test.go desktop/src-tauri/src/lib.rs
git commit
```
