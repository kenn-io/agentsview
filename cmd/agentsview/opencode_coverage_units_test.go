package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

// newOpenCodeSQLiteWatchDir creates a configured OpenCode directory whose
// only source is a SQLite database, the shape of the reported
// budget-starvation regression.
func newOpenCodeSQLiteWatchDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "opencode.db"), []byte("sqlite"))
	return root
}

func registerCollectedWatchRoots(
	t *testing.T, roots []watchRoot, budget int,
) []agentsync.RecursiveWatchResult {
	t.Helper()
	watcher, err := agentsync.NewWatcherWithCallback(
		time.Millisecond, 0,
		func(context.Context, agentsync.WatchBatch) error { return nil },
		nil, agentsync.WatcherOptions{},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	registered := make([]agentsync.WatchRoot, 0, len(roots))
	for _, root := range roots {
		registered = append(registered, root.registeredRoot())
	}
	return watcher.RegisterRoots(registered, budget)
}

// TestOpenCodeSQLiteRootSurvivesExhaustedRecursiveBudget is the reproduction
// for the reported failure: with the recursive watch budget exhausted, a
// pure-SQLite OpenCode root used to fail registration entirely and fall to
// the unwatched-root poller, whose every pass is an authoritative archive
// reconciliation. The container unit registers shallowly without consulting
// the budget, so the poller must never receive the OpenCode root.
func TestOpenCodeSQLiteRootSurvivesExhaustedRecursiveBudget(t *testing.T) {
	openCodeDir := newOpenCodeSQLiteWatchDir(t)
	sentinel := requireExistingPollRoot(t, t.TempDir(), "sentinel")
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentOpenCode: {openCodeDir},
	}}

	roots, unwatchedDirs, symlinkGatedDirs, persistentDirAgents :=
		collectWatchRoots(cfg)
	results := registerCollectedWatchRoots(t, roots, 0)
	unwatchedDirs = accountRegisteredWatchRoots(unwatchedDirs, roots, results)
	obligations := watchPollingObligations(
		roots, results, unwatchedDirs, persistentDirAgents,
	)
	obligations = append(
		obligations, symlinkPollingObligations(symlinkGatedDirs)...,
	)

	syncer := &recordingUnwatchedPollSyncer{wake: make(chan struct{}, 2)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, make(chan time.Time), func() {},
		func(run func()) { run() }, nil, time.Now,
		func(d time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		},
	)
	t.Cleanup(coordinator.Stop)
	for _, obligation := range obligations {
		require.NoError(t,
			coordinator.AddObligation(syncObligationToPoller(obligation)))
	}
	// The sentinel keeps one pollable root installed so the poll pass is
	// observable even when the OpenCode root correctly stays out of it.
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "sentinel", Scopes: []pollingScope{{Root: sentinel}},
	}))

	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)
	calls := syncer.snapshot()
	require.NotEmpty(t, calls)
	for _, call := range calls {
		assert.NotContains(t, call, filepath.Clean(openCodeDir),
			"a budget-starved SQLite root must not reach the archive poller")
	}
	assert.Contains(t, slices.Concat(calls...), sentinel)
}

// TestOpenCodeContainerUnitIsBudgetExempt pins the registration mechanism in
// a mixed provider plan: with the recursive budget at zero, recursive roots
// degrade while the OpenCode container root still registers natively.
func TestOpenCodeContainerUnitIsBudgetExempt(t *testing.T) {
	openCodeDir := newOpenCodeSQLiteWatchDir(t)
	claudeDir := t.TempDir()
	writeTestFile(t,
		filepath.Join(claudeDir, "session.jsonl"), []byte("{}\n"))
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentOpenCode: {openCodeDir},
		parser.AgentClaude:   {claudeDir},
	}}

	roots, _, _, _ := collectWatchRoots(cfg)
	results := registerCollectedWatchRoots(t, roots, 0)

	openCodeIdx := slices.IndexFunc(roots, func(root watchRoot) bool {
		return root.path == filepath.Clean(openCodeDir)
	})
	require.GreaterOrEqual(t, openCodeIdx, 0,
		"opencode container root must be in the collected plan")
	assert.False(t, roots[openCodeIdx].recursive)
	require.NoError(t, results[openCodeIdx].Err)
	assert.Equal(t, 1, results[openCodeIdx].Watched,
		"the shallow container unit must register despite budget 0")
	assert.False(t, results[openCodeIdx].BudgetExhausted)

	claudeIdx := slices.IndexFunc(roots, func(root watchRoot) bool {
		return root.path == filepath.Clean(claudeDir)
	})
	require.GreaterOrEqual(t, claudeIdx, 0)
	require.True(t, roots[claudeIdx].recursive)
	if recursiveWatchesMeterBudget() {
		assert.True(t, results[claudeIdx].BudgetExhausted,
			"recursive roots still compete for the exhausted budget")
	}
}

// recursiveWatchesMeterBudget reports whether this platform's watch backend
// counts recursive roots against the shared directory budget. The macOS
// FSEvents backend owns the whole root plan and opens one stream per logical
// root, so it never consults the budget and never reports exhaustion.
func recursiveWatchesMeterBudget() bool {
	return runtime.GOOS != "darwin"
}

// TestOpenCodeSQLiteRootPollableUnderWatcherUnavailableFallback proves the
// conditional storage-unit emission does not over-block: when the watcher
// cannot be constructed at all, a pure-SQLite root's configured directory
// must remain pollable rather than being deferred behind a probe for a
// storage subtree that will never exist.
func TestOpenCodeSQLiteRootPollableUnderWatcherUnavailableFallback(
	t *testing.T,
) {
	openCodeDir := newOpenCodeSQLiteWatchDir(t)
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentOpenCode: {openCodeDir},
	}}

	roots, unwatchedDirs, _, persistentDirAgents := collectWatchRoots(cfg)
	obligations := watchPollingObligations(
		roots, nil, unwatchedDirs, persistentDirAgents,
	)

	gates := make([]pollingObligation, 0, len(obligations))
	for _, obligation := range obligations {
		gates = append(gates, syncObligationToPoller(obligation))
	}
	assert.Contains(t,
		availableUnwatchedPollRootsFlat(gates), filepath.Clean(openCodeDir),
		"a pure-SQLite root must stay pollable when no watcher exists")
}

// TestOpenCodeSymlinkedStorageSubtreeKeepsExistingGate pins the symlink
// boundary: a storage subtree reached through a directory symlink keeps the
// existing recursive-symlink polling gate while the container unit still
// joins the watch plan natively.
func TestOpenCodeSymlinkedStorageSubtreeKeepsExistingGate(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "storage-target")
	require.NoError(t, os.MkdirAll(filepath.Join(target, "session"), 0o755))
	storageRoot := filepath.Join(root, "storage")
	requireSymlinkOrSkip(t, target, storageRoot)
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentOpenCode: {root},
	}}

	roots, unwatchedDirs, symlinkGatedDirs, _ := collectWatchRoots(cfg)

	container, ok := findCollectedWatchRoot(roots, root)
	require.True(t, ok, "container unit must register natively")
	assert.False(t, container.recursive)
	assert.True(t, container.exists)
	_, storageInPlan := findCollectedWatchRoot(roots, storageRoot)
	assert.False(t, storageInPlan,
		"a symlinked recursive root never joins the watcher plan")
	assert.Equal(t, map[string][]watchScope{
		filepath.Clean(storageRoot): {
			{agent: parser.AgentOpenCode, syncDir: filepath.Clean(root)},
		},
	}, symlinkGatedDirs,
		"the symlinked storage subtree keeps the polling.symlinkRoots gate")
	assert.Contains(t, unwatchedDirs, filepath.Clean(root))
}

// TestOpenCodeModeNoneRootProducesZeroObligationsAtZeroBudget pins the
// cheapest row of the mode matrix: an existing root with neither database
// nor storage layout costs exactly one shallow watch and never creates a
// polling obligation, even with no recursive budget at all.
func TestOpenCodeModeNoneRootProducesZeroObligationsAtZeroBudget(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentOpenCode: {root},
	}}

	roots, unwatchedDirs, symlinkGatedDirs, persistentDirAgents :=
		collectWatchRoots(cfg)
	require.Len(t, roots, 1)
	assert.False(t, roots[0].recursive)
	results := registerCollectedWatchRoots(t, roots, 0)
	require.NoError(t, results[0].Err)
	assert.Equal(t, 1, results[0].Watched)

	unwatchedDirs = accountRegisteredWatchRoots(unwatchedDirs, roots, results)
	obligations := watchPollingObligations(
		roots, results, unwatchedDirs, persistentDirAgents,
	)
	obligations = append(
		obligations, symlinkPollingObligations(symlinkGatedDirs)...,
	)
	assert.Empty(t, obligations,
		"a mode-none root must produce zero polling obligations")
}

// TestShallowWatchObservesDirectChildrenOnly is the live filesystem proof
// behind the container unit: one non-recursive watch on the configured root
// observes the database, its WAL, and storage/ directory creation as direct
// children, and does not observe grandchildren. It drives the real watcher
// backend against real writes, so it exercises this platform's fsnotify
// semantics rather than assuming them.
func TestShallowWatchObservesDirectChildrenOnly(t *testing.T) {
	root := t.TempDir()
	batches := make(chan agentsync.WatchBatch, 16)
	watcher, err := agentsync.NewWatcherWithCallback(
		10*time.Millisecond, 0,
		func(_ context.Context, batch agentsync.WatchBatch) error {
			batches <- batch
			return nil
		},
		nil, agentsync.WatcherOptions{},
	)
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)

	results := watcher.RegisterRoots([]agentsync.WatchRoot{{
		Path: root, Recursive: false, Exists: true,
	}}, 0)
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	require.NoError(t, watcher.StartCollecting())
	watcher.OpenDispatch()

	dbPath := filepath.Join(root, "opencode.db")
	walPath := filepath.Join(root, "opencode.db-wal")
	storageDir := filepath.Join(root, "storage")
	grandchild := filepath.Join(storageDir, "deep.json")
	writeTestFile(t, dbPath, []byte("sqlite"))
	writeTestFile(t, walPath, []byte("wal"))
	require.NoError(t, os.Mkdir(storageDir, 0o755))
	writeTestFile(t, grandchild, []byte("{}"))

	observed := make(map[string]bool)
	directChildren := []string{dbPath, walPath, storageDir}
	deadline := time.After(5 * time.Second)
	for {
		allDirect := true
		for _, child := range directChildren {
			if !observed[filepath.Clean(child)] {
				allDirect = false
			}
		}
		if allDirect {
			break
		}
		select {
		case batch := <-batches:
			for _, path := range batch.Paths {
				observed[filepath.Clean(path)] = true
			}
		case <-deadline:
			require.FailNow(t,
				"direct-child events did not arrive", "observed %v", observed)
		}
	}
	// Grace window: any pending grandchild event would have been batched
	// alongside or shortly after the direct-child events above.
	grace := time.After(300 * time.Millisecond)
	for {
		select {
		case batch := <-batches:
			for _, path := range batch.Paths {
				observed[filepath.Clean(path)] = true
			}
			continue
		case <-grace:
		}
		break
	}
	assert.False(t, observed[filepath.Clean(grandchild)],
		"a shallow watch must not observe grandchildren")
}
