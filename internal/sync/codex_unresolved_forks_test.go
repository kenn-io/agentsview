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

	// Pass 1: reconciles both groups once and codex reports the initial
	// deferred forks. Only the healthy claude group keeps its snapshot: the
	// forks were written but their parents were not yet checked, so a parent
	// that merely failed to read must stay eligible for the next poll.
	err := f.engine.PollProviderRootsGrouped(t.Context(), groups, false)
	require.Error(t, err)
	assert.True(t, watchBatchDeferOnlyError(err))
	require.EqualValues(t, 2, f.reconciled.Load())

	// Pass 2: codex reconciles once more, confirms every parent is absent, and
	// settles all 8 unchanged forks without a deferred error.
	require.NoError(t, f.engine.PollProviderRootsGrouped(t.Context(), groups, false))
	assert.EqualValues(t, 3, f.reconciled.Load(),
		"only the group with unverified forks may reconcile again")

	// Pass 3 (nothing changed): metadata scan finds zero changes, reconciles
	// zero groups, and returns nil.
	require.NoError(t, f.engine.PollProviderRootsGrouped(t.Context(), groups, false))
	assert.EqualValues(t, 3, f.reconciled.Load(),
		"subsequent polls must not re-reconcile unchanged roots when only orphan forks exist")

	// Authoritative pass (e.g. pending release): settles all 8 unchanged orphan forks
	// and succeeds without reporting deferred errors.
	require.NoError(t, f.engine.PollProviderRootsGrouped(t.Context(), groups, true))
	assert.EqualValues(t, 5, f.reconciled.Load())

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
	assert.EqualValues(t, 5, f.reconciled.Load(),
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

func TestPollProviderRootsGroupedDoesNotKeepSnapshotOnMixedPageErrors(t *testing.T) {
	requirePollChangeTimes(t)
	const parent = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	const child = "019f43cc-4444-4444-8444-444444444444"
	const ts = "2026-07-08T10:00:00Z"

	// Direct check: errors.Join combining a defer-only error and a hard error
	// must never be classified as defer-only.
	deferErr := &incompleteReconciliationError{
		deferred:  1,
		paths:     []string{"/tmp/fork.jsonl"},
		deferOnly: true,
	}
	joined := fmt.Errorf("sync changed polled paths: %w", errors.Join(deferErr, errors.New("disk read failed")))
	assert.False(t, watchBatchDeferOnlyError(joined))

	// End-to-end multi-page poll (> pollScanPageSize changed files):
	// page 1 contains an unresolved Codex fork (defer-only), while page 2
	// contains an unreadable rollout file (hard error). PollProviderRootsGrouped
	// must NOT keep afterSync, so once the unreadable file recovers it is
	// picked up on the next poll.
	codexRoot := t.TempDir()
	f := newPollScanFixture(t, map[parser.AgentType][]string{
		parser.AgentCodex: {codexRoot},
	})
	group := ProviderRootsGroup{Agent: parser.AgentCodex, Roots: []string{codexRoot}}
	// Seed an initial empty snapshot so the next poll takes the syncPollScanPaths path.
	require.NoError(t, f.engine.PollProviderRootsGrouped(t.Context(), []ProviderRootsGroup{group}, false))

	// Create 1 orphan fork plus pollScanPageSize healthy files and 1 unreadable
	// file sorting onto page 2.
	orphanPath := filepath.Join(codexRoot, "2026", "07", "08", "rollout-2026-07-08T00-00-00-"+child+".jsonl")
	writeCodexRolloutFile(t, orphanPath, testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "parent answer", ts),
		testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "child answer", ts),
		testjsonl.CodexTokenCountJSON(ts, 20, 5, 0),
	), false)
	for i := range pollScanPageSize {
		id := fmt.Sprintf("11111111-1111-4111-8111-%012d", i)
		p := filepath.Join(codexRoot, "2026", "07", "08", fmt.Sprintf("rollout-2026-07-08T01-%02d-%02d-%s.jsonl", i/60, i%60, id))
		writeCodexRolloutFile(t, p, testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(id, "/work/project", "user", ts),
			testjsonl.CodexMsgJSON("assistant", "ok", ts),
		), false)
	}
	const failingID = "99999999-9999-4999-8999-999999999999"
	failingPath := filepath.Join(codexRoot, "2026", "07", "09", "rollout-2026-07-09T23-59-59-"+failingID+".jsonl")
	writeCodexRolloutFile(t, failingPath, testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(failingID, "/work/project", "user", ts),
		testjsonl.CodexMsgJSON("assistant", "recovered session", ts),
	), false)
	require.NoError(t, os.Chmod(failingPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(failingPath, 0o600) })
	if _, err := os.ReadFile(failingPath); err == nil {
		t.Skip("running as root bypasses chmod 0000 read failure")
	}

	err := f.engine.PollProviderRootsGrouped(t.Context(), []ProviderRootsGroup{group}, false)
	require.Error(t, err)
	assert.False(t, watchBatchDeferOnlyError(err))
	key := pollScanKey{parser.AgentCodex, filepath.Clean(codexRoot)}
	f.engine.pollScans.mu.Lock()
	snapshotLen := len(f.engine.pollScans.snapshots[key])
	f.engine.pollScans.mu.Unlock()
	assert.Zero(t, snapshotLen,
		"mixed defer-only and hard page errors must not advance the poll snapshot")

	// Fix the unreadable file and confirm the next poll syncs it.
	require.NoError(t, os.Chmod(failingPath, 0o600))
	require.NoError(t, f.engine.PollProviderRootsGrouped(t.Context(), []ProviderRootsGroup{group}, false))
	assert.Equal(t, []string{"recovered session"}, messageContents(t, f.database, "codex:"+failingID),
		"previously failed page-2 session must sync after error clears")
}

func TestPollProviderRootsGroupedWakesCrossRootCodexForkDuringScopedReconciliation(t *testing.T) {
	requirePollChangeTimes(t)
	const parent = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	const child = "019f44dd-5555-4555-8555-555555555555"
	const transient = "22222222-2222-4222-8222-222222222222"
	const childID = "codex:" + child
	const ts = "2026-07-08T10:00:00Z"

	liveRoot := t.TempDir()
	archivedRoot := t.TempDir()

	childPath := filepath.Join(liveRoot, "2026", "07", "08", "rollout-2026-07-08T11-00-00-"+child+".jsonl")
	writeCodexRolloutFile(t, childPath, testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "replayed parent answer", ts),
		testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "child answer", ts),
		testjsonl.CodexTokenCountJSON(ts, 20, 7, 0),
	), false)
	transientPath := filepath.Join(archivedRoot, "rollout-2026-07-08T08-00-00-"+transient+".jsonl")
	writeCodexRolloutFile(t, transientPath, testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(transient, "/work/project", "user", ts),
		testjsonl.CodexMsgJSON("assistant", "ephemeral", ts),
	), false)

	f := newPollScanFixture(t, map[parser.AgentType][]string{
		parser.AgentCodex: {liveRoot, archivedRoot},
	})
	groups := []ProviderRootsGroup{{Agent: parser.AgentCodex, Roots: []string{liveRoot, archivedRoot}}}

	// Pass 1 discovers both roots and stores the unresolved fork in liveRoot;
	// pass 2 confirms its parent is absent, settles it, and keeps snapshots.
	err := f.engine.PollProviderRootsGrouped(t.Context(), groups, false)
	require.Error(t, err)
	assert.True(t, watchBatchDeferOnlyError(err))
	require.NoError(t, f.engine.PollProviderRootsGrouped(t.Context(), groups, false))
	assert.Equal(t, []string{"replayed parent answer", "child answer"}, messageContents(t, f.database, childID))

	// Simultaneously add parent rollout to archivedRoot AND remove transientPath
	// from archivedRoot so PollProviderRootsGrouped runs scoped reconciliation
	// on archivedRoot only (while liveRoot has zero changes).
	parentPath := filepath.Join(archivedRoot, "rollout-2026-07-08T09-00-00-"+parent+".jsonl")
	writeCodexRolloutFile(t, parentPath, testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", ts),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "replayed parent answer", ts),
		testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
	), false)
	require.NoError(t, os.Remove(transientPath))

	require.NoError(t, f.engine.PollProviderRootsGrouped(t.Context(), groups, false))
	assert.Equal(t, db.CurrentDataVersion(), f.database.GetSessionDataVersion(t.Context(), childID),
		"cross-root waiting fork in liveRoot must resolve when parent appears during scoped archivedRoot reconciliation")
	assert.Equal(t, []string{"child answer"}, messageContents(t, f.database, childID))
	usage, err := f.database.GetSessionUsage(t.Context(), childID, true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 7, usage.TotalOutputTokens)
}

func TestDeferOnlyLostEventsRetryClearsLostEventsFlag(t *testing.T) {
	// Simulate an overflowed defer-only reconciliation error during LostEvents
	// recovery (e.g. > 256 unresolved Codex forks). The retry batch must clear
	// LostEvents so the follow-up pass runs normal reconciliation and settles
	// instead of re-entering forced overflow recovery forever.
	root := filepath.Join(t.TempDir(), "codex")
	cause := &incompleteReconciliationError{
		deferred:  257,
		roots:     []string{root},
		paths:     []string{filepath.Join(root, "fork.jsonl")},
		overflow:  true,
		deferOnly: true,
	}
	err := watchBatchReconciliationError(cause, nil, []string{root}, true, true)
	var retry interface{ WatchRetryBatch() WatchBatch }
	require.ErrorAs(t, err, &retry)
	assert.False(t, retry.WatchRetryBatch().LostEvents,
		"defer-only retry after lost-events recovery must not re-force overflow recovery")
	assert.Equal(t, []string{root}, retry.WatchRetryBatch().ReconcileRoots)

	// Hard failures during LostEvents recovery must preserve LostEvents=true.
	hardCause := &incompleteReconciliationError{
		failures:  1,
		deferred:  257,
		roots:     []string{root},
		overflow:  true,
		deferOnly: false,
	}
	hardErr := watchBatchReconciliationError(hardCause, nil, []string{root}, true, true)
	require.ErrorAs(t, hardErr, &retry)
	assert.True(t, retry.WatchRetryBatch().LostEvents,
		"non-defer-only failure must keep LostEvents=true for retry")
}

func TestAppendWaitingCodexForkPathsHandlesRemovedParentBeforeReplacement(t *testing.T) {
	requirePollChangeTimes(t)
	const parent = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	const child = "019f45ee-6666-4666-8666-666666666666"
	const childID = "codex:" + child
	const ts = "2026-07-08T10:00:00Z"

	liveRoot := t.TempDir()
	archivedRoot := t.TempDir()
	childPath := filepath.Join(liveRoot, "2026", "07", "08", "rollout-2026-07-08T11-00-00-"+child+".jsonl")
	removedParentPath := filepath.Join(liveRoot, "2026", "07", "08", "rollout-2026-07-08T09-00-00-"+parent+".jsonl")
	replacementParentPath := filepath.Join(archivedRoot, "rollout-2026-07-08T09-00-00-"+parent+".jsonl.zst")

	writeCodexRolloutFile(t, childPath, testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "replayed parent answer", ts),
		testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "child answer", ts),
		testjsonl.CodexTokenCountJSON(ts, 20, 9, 0),
	), false)

	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs:                         map[parser.AgentType][]string{parser.AgentCodex: {liveRoot, archivedRoot}},
		Machine:                           "local",
		DisableFilesystemProjectDiscovery: true,
	})
	t.Cleanup(engine.Close)

	// Record and settle the orphan fork.
	_, firstErr := engine.SyncWatchBatchThenRun(
		t.Context(), WatchBatch{Paths: []string{childPath}}, nil, nil,
	)
	require.Error(t, firstErr)
	_, secondErr := engine.SyncWatchBatchThenRun(
		t.Context(), WatchBatch{Paths: []string{childPath}}, nil, nil,
	)
	require.NoError(t, secondErr)

	// Write only the replacement parent file; pass the removed path first in
	// the same changed-path batch as the replacement path.
	writeCodexRolloutFile(t, replacementParentPath, testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", ts),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "replayed parent answer", ts),
		testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
	), true)

	_, wakeErr := engine.SyncWatchBatchThenRun(
		t.Context(),
		WatchBatch{Paths: []string{removedParentPath, replacementParentPath}},
		nil, nil,
	)
	require.NoError(t, wakeErr)
	assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), childID),
		"nonexistent parent path earlier in the batch must not block wake-up from its replacement")
	assert.Equal(t, []string{"child answer"}, messageContents(t, database, childID))
	usage, err := database.GetSessionUsage(t.Context(), childID, true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 9, usage.TotalOutputTokens)
}

func TestScopedReconciliationWakesWaitingForkInSiblingDirectory(t *testing.T) {
	requirePollChangeTimes(t)
	const parent = "abababab-abab-4bab-8bab-abababababab"
	const child = "019f46ff-7777-4777-8777-777777777777"
	const childID = "codex:" + child
	const ts = "2026-07-08T10:00:00Z"

	root := t.TempDir()
	// The fork's cwd sits inside a local Git repository. With filesystem
	// project discovery disabled, waking the fork must keep the lexical
	// project instead of probing the repository.
	repo := filepath.Join(t.TempDir(), "host-repo")
	cwd := filepath.Join(repo, "nested-app")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o700))
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	childPath := filepath.Join(root, "2026", "07", "07", "rollout-2026-07-07T11-00-00-"+child+".jsonl")
	writeCodexRolloutFile(t, childPath, testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(child, parent, cwd, "user", ts),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "replayed parent answer", ts),
		testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "child answer", ts),
		testjsonl.CodexTokenCountJSON(ts, 20, 3, 0),
	), false)

	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs:                         map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:                           "local",
		DisableFilesystemProjectDiscovery: true,
	})
	t.Cleanup(engine.Close)

	// SyncPathsContext applies the engine's parse policy, so the stored
	// project is the lexical one the wake must keep.
	require.Error(t, engine.SyncPathsContext(t.Context(), []string{childPath}))
	stored, err := database.GetSession(t.Context(), childID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	lexicalProject := stored.Project

	// The parent arrives in another dated directory, and the watcher reports
	// only that directory for reconciliation.
	parentDir := filepath.Join(root, "2026", "07", "08")
	writeCodexRolloutFile(t, filepath.Join(parentDir, "rollout-2026-07-08T09-00-00-"+parent+".jsonl"), testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", ts),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "replayed parent answer", ts),
		testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
	), false)
	_, err = engine.SyncWatchBatchThenRun(t.Context(), WatchBatch{ReconcileRoots: []string{parentDir}}, nil, nil)
	require.NoError(t, err)

	assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), childID),
		"a waiting fork outside the reconciled directory must wake when its parent appears")
	assert.Equal(t, []string{"child answer"}, messageContents(t, database, childID))
	woken, err := database.GetSession(t.Context(), childID)
	require.NoError(t, err)
	require.NotNil(t, woken)
	assert.Equal(t, lexicalProject, woken.Project,
		"waking a fork must apply the same project discovery policy as other parses")
}

func TestScopedReconciliationWithWokenForkKeepsAnomalies(t *testing.T) {
	requirePollChangeTimes(t)
	const parent = "cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd"
	const child = "019f47aa-8888-4888-8888-888888888888"
	const ts = "2026-07-08T10:00:00Z"

	root := t.TempDir()
	childPath := filepath.Join(root, "2026", "07", "07", "rollout-2026-07-07T11-00-00-"+child+".jsonl")
	writeCodexRolloutFile(t, childPath, testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "child answer", ts),
	), false)
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs:                         map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:                           "local",
		DisableFilesystemProjectDiscovery: true,
	})
	t.Cleanup(engine.Close)
	_, err := engine.SyncWatchBatchThenRun(t.Context(), WatchBatch{Paths: []string{childPath}}, nil, nil)
	require.Error(t, err)

	// The reconciled directory holds the arriving parent and an unrelated
	// rollout with a malformed line, so the pass reports a parser anomaly
	// while it wakes the fork.
	parentDir := filepath.Join(root, "2026", "07", "08")
	writeCodexRolloutFile(t, filepath.Join(parentDir, "rollout-2026-07-08T09-00-00-"+parent+".jsonl"), testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", ts),
		testjsonl.CodexMsgJSON("assistant", "parent answer", ts),
	), false)
	const noisyID = "efefefef-efef-4fef-8fef-efefefefefef"
	writeCodexRolloutFile(t, filepath.Join(parentDir, "rollout-2026-07-08T10-00-00-"+noisyID+".jsonl"), testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(noisyID, "/work/project", "user", ts),
		"{not json",
		testjsonl.CodexMsgJSON("assistant", "noisy answer", ts),
	), false)

	stats, err := engine.SyncWatchBatchThenRun(t.Context(), WatchBatch{ReconcileRoots: []string{parentDir}}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), "codex:"+child),
		"the fork must wake in the same pass")
	assert.Equal(t, 1, stats.Anomalies.MalformedLinesTotal,
		"waking forks must not erase the reconciliation pass's anomalies")
}

func TestWatchBatchReconciliationErrorRetriesEveryJoinedSource(t *testing.T) {
	// A scoped reconciliation joins its own deferral with the deferral from
	// syncing woken forks. The retry must keep the sources of both.
	reconcileErr := &incompleteReconciliationError{
		deferred:  1,
		paths:     []string{"/codex/a.jsonl"},
		roots:     []string{"/codex/archived"},
		deferOnly: true,
	}
	wakeErr := &incompleteReconciliationError{
		deferred:  1,
		paths:     []string{"/codex/b.jsonl"},
		deferOnly: true,
	}
	err := watchBatchReconciliationError(
		errors.Join(reconcileErr, wakeErr), nil, []string{"/codex/archived"}, false, false,
	)
	var retry interface{ WatchRetryBatch() WatchBatch }
	require.ErrorAs(t, err, &retry)
	assert.ElementsMatch(t, []string{"/codex/a.jsonl", "/codex/b.jsonl"}, retry.WatchRetryBatch().Paths)
	assert.Equal(t, []string{"/codex/archived"}, retry.WatchRetryBatch().ReconcileRoots)
	assert.True(t, watchBatchDeferOnlyError(err))
}
