package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type countingEmitter struct {
	events atomic.Int32
}

func (e *countingEmitter) Emit(string) { e.events.Add(1) }

func writeCodexRolloutFile(t *testing.T, path, content string, compressed bool) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	data := []byte(content)
	if compressed {
		enc, err := zstd.NewWriter(nil)
		require.NoError(t, err)
		data = enc.EncodeAll(data, nil)
		require.NoError(t, enc.Close())
	}
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func TestUnresolvedCodexForkSettlesInWatcherAndWakesOnParentArrival(t *testing.T) {
	requirePollChangeTimes(t)
	const parent = "3f7746a2-1111-4111-8111-111111111111"
	const child = "019f41aa-2222-4222-8222-222222222222"
	const id = "codex:" + child
	const ts = "2026-07-08T10:00:00Z"

	for _, compressed := range []bool{false, true} {
		name := "plain"
		ext := ".jsonl"
		if compressed {
			name = "compressed"
			ext = ".jsonl.zst"
		}
		t.Run(name, func(t *testing.T) {
			liveRoot := t.TempDir()
			archivedRoot := t.TempDir()
			childPath := filepath.Join(liveRoot, "2026", "07", "08", "rollout-2026-07-08T11-00-00-"+child+ext)
			parentPath := filepath.Join(archivedRoot, "rollout-2026-07-08T10-00-00-"+parent+ext)

			childContent := testjsonl.JoinJSONL(
				testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
				testjsonl.CodexMsgJSON("assistant", "replayed parent answer", ts),
				testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", ts),
				testjsonl.CodexMsgJSON("assistant", "child answer", ts),
				testjsonl.CodexTokenCountJSON(ts, 20, 5, 0),
			)
			writeCodexRolloutFile(t, childPath, childContent, compressed)

			database := openTestDB(t)
			emitter := &countingEmitter{}
			cfg := EngineConfig{
				AgentDirs:                         map[parser.AgentType][]string{parser.AgentCodex: {liveRoot, archivedRoot}},
				Machine:                           "local",
				DisableFilesystemProjectDiscovery: true,
				Emitter:                           emitter,
			}
			engine := NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)

			// Pass 1: discovers the missing parent, stores fallback messages with
			// withheld usage, and returns a defer-only retry for the single path.
			firstStats, firstErr := engine.SyncWatchBatchThenRun(
				t.Context(), WatchBatch{Paths: []string{childPath}}, nil, nil,
			)
			require.Error(t, firstErr)
			assert.True(t, watchBatchDeferOnlyError(firstErr))
			assert.Equal(t, 1, firstStats.Deferred)
			assert.Equal(t, 1, firstStats.Synced)
			assert.Equal(t, []string{"replayed parent answer", "child answer"}, messageContents(t, database, id))
			assert.Less(t, database.GetSessionDataVersion(t.Context(), id), db.CurrentDataVersion())

			emitsBeforeSettle := emitter.events.Load()

			// Pass 2 (watcher retry while file is unchanged and parent still missing):
			// settles with zero writes, zero deferred errors, and zero SSE emits.
			secondStats, secondErr := engine.SyncWatchBatchThenRun(
				t.Context(), WatchBatch{Paths: []string{childPath}}, nil, nil,
			)
			require.NoError(t, secondErr)
			assert.Zero(t, secondStats.Deferred)
			assert.Zero(t, secondStats.Synced)
			assert.Equal(t, 1, secondStats.Skipped)
			assert.Equal(t, emitsBeforeSettle, emitter.events.Load(),
				"a settled unresolved fork must not rewrite or emit session events")

			// Editing the fork rollout still updates its visible messages.
			updatedContent := childContent + testjsonl.JoinJSONL(
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "later-turn", ts),
				testjsonl.CodexMsgJSON("assistant", "later child answer", ts),
				testjsonl.CodexTokenCountJSON(ts, 20, 7, 0),
			)
			writeCodexRolloutFile(t, childPath, updatedContent, compressed)
			editStats, editErr := engine.SyncWatchBatchThenRun(
				t.Context(), WatchBatch{Paths: []string{childPath}}, nil, nil,
			)
			require.Error(t, editErr)
			assert.True(t, watchBatchDeferOnlyError(editErr))
			assert.Equal(t, 1, editStats.Synced)
			assert.Equal(t, []string{"replayed parent answer", "child answer", "later child answer"}, messageContents(t, database, id))

			// Settling again after the edit stops the retry loop cleanly.
			settleAgain, settleErr := engine.SyncWatchBatchThenRun(
				t.Context(), WatchBatch{Paths: []string{childPath}}, nil, nil,
			)
			require.NoError(t, settleErr)
			assert.Zero(t, settleAgain.Deferred)
			assert.Zero(t, settleAgain.Synced)

			// Simulate a daemon restart so the in-memory tracker is empty, then let
			// only the missing parent rollout arrive as a watched changed path.
			engine.Close()
			restarted := NewEngine(t.Context(), database, cfg)
			t.Cleanup(restarted.Close)

			parentContent := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", ts),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
				testjsonl.CodexMsgJSON("assistant", "replayed parent answer", ts),
				testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
			)
			writeCodexRolloutFile(t, parentPath, parentContent, compressed)

			// Delivering only parentPath must wake the waiting child fork from SQLite
			// provenance, strip inherited turns, restore child usage, and upgrade it.
			wakeStats, wakeErr := restarted.SyncWatchBatchThenRun(
				t.Context(), WatchBatch{Paths: []string{parentPath}}, nil, nil,
			)
			require.NoError(t, wakeErr)
			assert.Zero(t, wakeStats.Deferred)
			assert.Equal(t, []string{"child answer", "later child answer"}, messageContents(t, database, id))
			assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), id))
			usage, err := database.GetSessionUsage(t.Context(), id, true)
			require.NoError(t, err)
			require.NotNil(t, usage)
			assert.Equal(t, 12, usage.TotalOutputTokens)
		})
	}
}

func TestSyncAllSkipsRewritingUnchangedUnresolvedCodexFork(t *testing.T) {
	requirePollChangeTimes(t)
	const parent = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const child = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	root := t.TempDir()
	writeOrphanCodexFork(t, root, child, parent)

	database := openTestDB(t)
	emitter := &countingEmitter{}
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs:                         map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:                           "local",
		DisableFilesystemProjectDiscovery: true,
		Emitter:                           emitter,
	})
	t.Cleanup(engine.Close)

	first := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 1, first.Deferred)
	assert.Equal(t, 1, first.Synced)
	emitsAfterFirst := emitter.events.Load()

	second := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 1, second.Deferred, "explicit SyncAll still reports unresolved forks")
	assert.Zero(t, second.Synced, "unchanged unresolved fork must not rewrite SQLite")
	assert.Equal(t, 1, second.Skipped)
	assert.Equal(t, emitsAfterFirst, emitter.events.Load())
}

func TestWatcherAcknowledgesLifecycleOnDeferOnlyWatchBatch(t *testing.T) {
	t.Run("defer-only acknowledges lifecycle immediately", func(t *testing.T) {
		backend := newFakeWatchBackend()
		gate := &recordingLifecycleGate{acknowledged: make(chan uint64, 1)}
		calls := make(chan WatchBatch, 2)
		var attempts atomic.Int32
		const deferredPath = "/tmp/codex-orphan-fork.jsonl"

		w, err := newWatcherWithBackend(
			0, 0,
			func(_ context.Context, batch WatchBatch) error {
				calls <- batch
				if attempts.Add(1) == 1 {
					return watchBatchReconciliationError(
						&incompleteReconciliationError{
							deferred:  1,
							paths:     []string{deferredPath},
							deferOnly: true,
						},
						nil, nil, true, false,
					)
				}
				return nil
			},
			backend, 16, 1_000,
		)
		require.NoError(t, err)
		w.Start()
		t.Cleanup(w.Stop)

		w.eventSink.RetainAuthoritative(backendLifecycleToken{gate: gate, generation: 7})

		first := receiveWatchBatch(t, calls)
		assert.True(t, first.FullSync)
		select {
		case gen := <-gate.acknowledged:
			assert.Equal(t, uint64(7), gen)
		case <-time.After(5 * time.Second):
			require.Fail(t, "timed out waiting for defer-only lifecycle acknowledgement")
		}
		second := receiveWatchBatch(t, calls)
		assert.Equal(t, []string{deferredPath}, second.Paths)
		assert.Empty(t, second.lifecycleTokens, "acknowledged lifecycle token must not stay on path retry")
	})

	t.Run("composed defer-only plus hard error withholds lifecycle acknowledgement", func(t *testing.T) {
		pathErr := watchBatchReconciliationError(
			&incompleteReconciliationError{
				deferred:  1,
				paths:     []string{"/tmp/deferred.jsonl"},
				deferOnly: true,
			},
			nil, nil, false, false,
		)
		rootErr := watchBatchReconciliationError(
			errors.New("root discovery failed"),
			nil, []string{"/tmp/root"}, false, false,
		)
		composed := composeWatchBatchErrors(pathErr, rootErr)
		assert.False(t, watchBatchDeferOnlyError(composed),
			"mixed defer-only and hard errors must not report defer-only")
	})
}

func TestPollProviderRootsGroupedSettlesOrphanCodexForksAndPreservesHealthySnapshots(t *testing.T) {
	requirePollChangeTimes(t)
	const parent = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const ts = "2026-07-08T10:00:00Z"

	claudeRoot := t.TempDir()
	codexRoot := t.TempDir()
	writeClaudePollSession(t, filepath.Join(claudeRoot, "project", "healthy.jsonl"), "hello")

	// Create multiple orphaned Codex forks alongside a healthy Codex session.
	const healthyID = "33333333-3333-4333-8333-333333333333"
	healthyPath := filepath.Join(codexRoot, "2026", "07", "08", "rollout-2026-07-08T09-00-00-"+healthyID+".jsonl")
	writeCodexRolloutFile(t, healthyPath, testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(healthyID, "/work/project", "user", ts),
		testjsonl.CodexMsgJSON("assistant", "healthy answer", ts),
	), false)

	var forkIDs []string
	for i := range 8 {
		child := fmt.Sprintf("019f41aa-2222-4222-8222-%012d", i)
		forkIDs = append(forkIDs, child)
		childPath := filepath.Join(codexRoot, "2026", "07", "08", "rollout-2026-07-08T11-00-00-"+child+".jsonl.zst")
		writeCodexRolloutFile(t, childPath, testjsonl.JoinJSONL(
			testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts),
			testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
			testjsonl.CodexMsgJSON("assistant", "parent answer", ts),
			testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
			testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", ts),
			testjsonl.CodexMsgJSON("assistant", fmt.Sprintf("child-%d", i), ts),
			testjsonl.CodexTokenCountJSON(ts, 20, 5, 0),
		), true)
	}

	f := newPollScanFixture(t, map[parser.AgentType][]string{
		parser.AgentClaude: {claudeRoot},
		parser.AgentCodex:  {codexRoot},
	})
	groups := []ProviderRootsGroup{
		{Agent: parser.AgentClaude, Roots: []string{claudeRoot}},
		{Agent: parser.AgentCodex, Roots: []string{codexRoot}},
	}

	// Pass 1: reconciles both groups once; codex reports the initial deferred forks,
	// yet both roots keep their metadata snapshots.
	err := f.engine.PollProviderRootsGrouped(t.Context(), groups, false)
	require.Error(t, err)
	assert.True(t, watchBatchDeferOnlyError(err))
	require.EqualValues(t, 2, f.reconciled.Load())

	// Pass 2 (nothing changed): metadata scan finds zero changes, reconciles zero
	// groups, and returns nil.
	require.NoError(t, f.engine.PollProviderRootsGrouped(t.Context(), groups, false))
	assert.EqualValues(t, 2, f.reconciled.Load(),
		"subsequent polls must not re-reconcile unchanged roots when only orphan forks exist")

	// Authoritative pass (e.g. pending release): settles all 8 unchanged orphan forks
	// and succeeds without reporting deferred errors.
	require.NoError(t, f.engine.PollProviderRootsGrouped(t.Context(), groups, true))
	assert.EqualValues(t, 4, f.reconciled.Load())

	// Parent rollout finally arrives on disk: next poll detects only the new parent
	// file via metadata scan (zero full reconciliations) and resolves all 8 forks.
	parentPath := filepath.Join(codexRoot, "2026", "07", "08", "rollout-2026-07-08T08-00-00-"+parent+".jsonl.zst")
	writeCodexRolloutFile(t, parentPath, testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", ts),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "parent answer", ts),
		testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
	), true)

	require.NoError(t, f.engine.PollProviderRootsGrouped(t.Context(), groups, false))
	assert.EqualValues(t, 4, f.reconciled.Load(),
		"parent arrival must sync via metadata diff without reconciling the whole root")
	for i, child := range forkIDs {
		id := "codex:" + child
		assert.Equal(t, db.CurrentDataVersion(), f.database.GetSessionDataVersion(t.Context(), id))
		assert.Equal(t, []string{fmt.Sprintf("child-%d", i)}, messageContents(t, f.database, id))
	}
}

func TestPollProviderRootsGroupedKeepsHealthyGroupSnapshotWhenAnotherGroupFails(t *testing.T) {
	requirePollChangeTimes(t)
	claudeRoot := t.TempDir()
	codexRoot := t.TempDir()
	writeClaudePollSession(t, filepath.Join(claudeRoot, "project", "healthy.jsonl"), "hello")

	// Make codex fail reconciliation with a real non-defer error by passing an
	// unreadable file inside codexRoot via a failing provider, or use a second
	// poll after introducing a hard failure on codex only.
	f := newPollScanFixture(t, map[parser.AgentType][]string{
		parser.AgentClaude: {claudeRoot},
		parser.AgentCodex:  {codexRoot},
	})
	badCodexDir := filepath.Join(codexRoot, "2026", "07", "08")
	require.NoError(t, os.MkdirAll(badCodexDir, 0o700))
	badCodexFile := filepath.Join(badCodexDir, "rollout-2026-07-08T10-00-00-11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.WriteFile(badCodexFile, []byte("{}\n"), 0o600))
	require.NoError(t, os.Chmod(badCodexFile, 0o000))
	t.Cleanup(func() { _ = os.Chmod(badCodexFile, 0o600) })
	if _, err := os.ReadFile(badCodexFile); err == nil {
		t.Skip("running as root bypasses chmod 0000 read failure")
	}

	groups := []ProviderRootsGroup{
		{Agent: parser.AgentClaude, Roots: []string{claudeRoot}},
		{Agent: parser.AgentCodex, Roots: []string{codexRoot}},
	}
	err := f.engine.PollProviderRootsGrouped(t.Context(), groups, false)
	require.Error(t, err)
	assert.False(t, watchBatchDeferOnlyError(err))
	require.EqualValues(t, 2, f.reconciled.Load())

	// On the next pass, claude must reuse its kept snapshot (0 reconciliations)
	// while only the failing codex group reconciles again (+1).
	err = f.engine.PollProviderRootsGrouped(t.Context(), groups, false)
	require.Error(t, err)
	assert.EqualValues(t, 3, f.reconciled.Load(),
		"healthy provider group must keep its snapshot when another group fails")
}

func TestUnresolvedCodexForkScalingAndDeletionCleanup(t *testing.T) {
	requirePollChangeTimes(t)
	const parent = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	const ts = "2026-07-08T10:00:00Z"

	for _, count := range []int{1, 24} {
		t.Run(fmt.Sprintf("orphans=%d", count), func(t *testing.T) {
			root := t.TempDir()
			paths := make([]string, 0, count)
			for i := range count {
				child := fmt.Sprintf("019f42bb-3333-4333-8333-%012d", i)
				p := filepath.Join(root, "2026", "07", "08", "rollout-2026-07-08T11-00-00-"+child+".jsonl.zst")
				writeCodexRolloutFile(t, p, testjsonl.JoinJSONL(
					testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
					testjsonl.CodexMsgJSON("assistant", "parent answer", ts),
					testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", ts),
					testjsonl.CodexMsgJSON("assistant", fmt.Sprintf("child-%d", i), ts),
					testjsonl.CodexTokenCountJSON(ts, 20, 5, 0),
				), true)
				paths = append(paths, p)
			}

			database := openTestDB(t)
			emitter := &countingEmitter{}
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs:                         map[parser.AgentType][]string{parser.AgentCodex: {root}},
				Machine:                           "local",
				DisableFilesystemProjectDiscovery: true,
				Emitter:                           emitter,
			})
			t.Cleanup(engine.Close)

			// Initial pass discovers all unresolved forks and records them.
			firstStats, firstErr := engine.SyncWatchBatchThenRun(
				t.Context(), WatchBatch{Paths: paths}, nil, nil,
			)
			require.Error(t, firstErr)
			assert.True(t, watchBatchDeferOnlyError(firstErr))
			assert.Equal(t, count, firstStats.Deferred)
			assert.Equal(t, count, firstStats.Synced)

			emitsBeforeSettle := emitter.events.Load()

			// Steady-state pass: unchanged work stays 0 writes, 0 emits, 0 deferred
			// regardless of orphan fork cardinality.
			settledStats, settledErr := engine.SyncWatchBatchThenRun(
				t.Context(), WatchBatch{Paths: paths}, nil, nil,
			)
			require.NoError(t, settledErr)
			assert.Zero(t, settledStats.Synced)
			assert.Zero(t, settledStats.Deferred)
			assert.Equal(t, count, settledStats.Skipped)
			assert.Equal(t, emitsBeforeSettle, emitter.events.Load())

			// Deleting the first orphan fork removes its tracked state and waiter
			// entry when the deletion is synced.
			deletedPath := paths[0]
			require.NoError(t, os.Remove(deletedPath))
			_, delErr := engine.SyncWatchBatchThenRun(
				t.Context(), WatchBatch{Paths: []string{deletedPath}}, nil, nil,
			)
			require.NoError(t, delErr)

			waiters := engine.unresolvedCodexForks.waitersForParent(t.Context(), database, parent)
			for _, w := range waiters {
				assert.NotEqual(t, filepath.Clean(deletedPath), w.path,
					"deleted fork must be removed from unresolved waiter index")
			}
			assert.Len(t, waiters, count-1)
		})
	}
}
