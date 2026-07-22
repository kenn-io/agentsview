package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const watcherTestTimeout = 5 * time.Second

func requireReceiveWithin[T any](t *testing.T, ch <-chan T, timeout time.Duration) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(timeout):
		require.FailNow(t, "timed out waiting for channel value")
		var zero T
		return zero
	}
}

type watcherCall struct {
	paths []string
	at    time.Time
}

type recordingLifecycleGate struct {
	acknowledged chan uint64
}

func (g *recordingLifecycleGate) acknowledgeLifecycle(generation uint64) {
	g.acknowledged <- generation
}

func TestWatcherAcknowledgesLifecycleOnlyAfterSuccessfulReconciliation(t *testing.T) {
	backend := newFakeWatchBackend()
	gate := &recordingLifecycleGate{acknowledged: make(chan uint64, 1)}
	calls := make(chan WatchBatch, 2)
	releaseSuccess := make(chan struct{})
	var attempts atomic.Int32
	w, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			if attempts.Add(1) == 1 {
				return errors.New("reconciliation unavailable")
			}
			<-releaseSuccess
			return nil
		},
		backend, 16, 1_000,
	)
	require.NoError(t, err)
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{
		Path: "/sync", Root: "/sync", Op: backendOpReconcileRootChange,
		Lifecycle: backendLifecycleToken{gate: gate, generation: 7},
	})
	first := requireReceiveWithin(t, calls, time.Second)
	assert.Equal(t, []string{"/sync"}, first.ReconcileRoots)
	select {
	case generation := <-gate.acknowledged:
		require.Fail(t, "failed reconciliation acknowledged lifecycle", generation)
	case <-time.After(50 * time.Millisecond):
	}
	second := requireReceiveWithin(t, calls, time.Second)
	assert.Equal(t, []string{"/sync"}, second.ReconcileRoots)
	close(releaseSuccess)
	assert.Equal(t, uint64(7), requireReceiveWithin(t, gate.acknowledged, time.Second))
}

func TestWatcherLifecycleCollectsBeforeDispatchOpens(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 1)
	w, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			return nil
		},
		backend, 2, 1_000,
	)
	require.NoError(t, err)
	w.StartCollecting()
	t.Cleanup(w.Stop)

	backend.sendEvent(t, "/sessions/one.jsonl")
	backend.sendEvent(t, "/sessions/two.jsonl")
	backend.sendEvent(t, "/sessions/overflow.jsonl")
	assert.Never(t, func() bool {
		select {
		case <-calls:
			return true
		default:
			return false
		}
	}, 50*time.Millisecond, 5*time.Millisecond,
		"collecting must not invoke the callback")

	w.OpenDispatch()
	batch := requireReceiveWithin(t, calls, time.Second)
	assert.True(t, batch.FullSync,
		"bounded startup accumulation must retain an authoritative marker")
}

// TestWatcherQueueRetryBatchDispatchesQueuedRoots pins the startup-gap retry
// handoff: a batch queued during collecting mode dispatches once OpenDispatch
// fires, carrying its reconcile roots into the normal callback (and thus
// retry) machinery, and a full-sync batch queues the authoritative marker.
func TestWatcherQueueRetryBatchDispatchesQueuedRoots(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 1)
	w, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.StartCollecting()
	t.Cleanup(w.Stop)

	w.QueueRetryBatch(WatchBatch{ReconcileRoots: []string{"/gap-root"}})
	assert.Never(t, func() bool {
		select {
		case <-calls:
			return true
		default:
			return false
		}
	}, 50*time.Millisecond, 5*time.Millisecond,
		"collecting must not invoke the callback")

	w.OpenDispatch()
	batch := requireReceiveWithin(t, calls, time.Second)
	assert.Equal(t, []string{"/gap-root"}, batch.ReconcileRoots)
	assert.False(t, batch.FullSync)
	assert.False(t, batch.LostEvents)

	w.QueueRetryBatch(WatchBatch{FullSync: true})
	batch = requireReceiveWithin(t, calls, time.Second)
	assert.True(t, batch.FullSync)
	assert.False(t, batch.LostEvents,
		"a queued ordinary full sync must not clear freshness caches like a "+
			"watcher-overflow full sync")
}

func TestWatcherLifecycleDispatchingDeliversNewEvents(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 1)
	w, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.StartCollecting()
	w.OpenDispatch()
	t.Cleanup(w.Stop)

	backend.sendEvent(t, "/sessions/live.jsonl")
	batch := requireReceiveWithin(t, calls, time.Second)
	assert.Equal(t, []string{"/sessions/live.jsonl"}, batch.Paths)
}

func TestWatcherLifecycleStoppedRejectsCollectionAndDispatch(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 1)
	w, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.Stop()

	w.StartCollecting()
	w.OpenDispatch()
	assert.Zero(t, backend.startCalls.Load())
	select {
	case batch := <-calls:
		require.Fail(t, "stopped watcher dispatched a batch", "%+v", batch)
	case <-time.After(50 * time.Millisecond):
	}
}

type fakeWatchBackend struct {
	events           chan backendEvent
	errors           chan error
	started          chan struct{}
	stopped          chan struct{}
	startErr         error
	recursiveRoots   []string
	recursiveBudgets []int
	shallowRoots     []string
	includeSubtree   func(root, path string) bool

	startOnce  sync.Once
	stopOnce   sync.Once
	startCalls atomic.Int32
	stopCalls  atomic.Int32
}

func newFakeWatchBackend() *fakeWatchBackend {
	return &fakeWatchBackend{
		events:  make(chan backendEvent),
		errors:  make(chan error),
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

func (b *fakeWatchBackend) Events() <-chan backendEvent { return b.events }
func (b *fakeWatchBackend) Errors() <-chan error        { return b.errors }
func (b *fakeWatchBackend) AddRecursive(root string, budget int) RecursiveWatchResult {
	b.recursiveRoots = append(b.recursiveRoots, root)
	b.recursiveBudgets = append(b.recursiveBudgets, budget)
	return RecursiveWatchResult{Watched: 1}
}
func (b *fakeWatchBackend) AddShallow(root string) error {
	b.shallowRoots = append(b.shallowRoots, root)
	return nil
}
func (b *fakeWatchBackend) Remove(string) error { return nil }
func (b *fakeWatchBackend) Start() error {
	b.startCalls.Add(1)
	b.startOnce.Do(func() { close(b.started) })
	return b.startErr
}
func (b *fakeWatchBackend) Stop() {
	b.stopCalls.Add(1)
	b.stopOnce.Do(func() { close(b.stopped) })
}
func (b *fakeWatchBackend) Name() string { return "fake" }

func (b *fakeWatchBackend) includeCreatedSubtreePath(root, path string) bool {
	return b.includeSubtree == nil || b.includeSubtree(root, path)
}

func (b *fakeWatchBackend) shouldEnumerateCreatedSubtree(_, _ string) bool {
	return true
}

func TestWatcherRegisterRootsPreservesCompleteLogicalPlanBeforeStart(t *testing.T) {
	backend := newFakeWatchBackend()
	w, err := newWatcherWithBackend(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	t.Cleanup(w.Stop)

	results := w.RegisterRoots([]WatchRoot{
		{
			Path: "/shared", Recursive: true, Exists: true,
			Scopes: []WatchScope{
				{Agent: "codex", SyncDir: "/sessions"},
				{Agent: "claude", SyncDir: "/projects"},
			},
		},
		{
			Path: "/missing", Recursive: true, Exists: false,
			Scopes: []WatchScope{{Agent: "devin", SyncDir: "/state"}},
		},
		{
			Path: "/shallow", Recursive: false, Exists: true,
			Scopes: []WatchScope{{Agent: "gemini", SyncDir: "/metadata"}},
		},
	}, 7)

	assert.Zero(t, backend.startCalls.Load(), "roots must register before backend start")
	assert.Equal(t, []string{"/shared"}, backend.recursiveRoots)
	assert.Equal(t, []int{7}, backend.recursiveBudgets)
	assert.Equal(t, []string{"/shallow"}, backend.shallowRoots)
	assert.Equal(t, []string{"claude", "codex"}, w.agentsForRoot("/shared"))
	assert.Equal(t, []string{"devin"}, w.agentsForRoot("/missing"),
		"missing logical roots must retain rename ownership")
	require.Len(t, results, 3)
	assert.Equal(t, 1, results[0].Watched)
	assert.Zero(t, results[1].Watched, "missing root must not activate current backend")
	assert.Equal(t, 1, results[2].Watched)
}

func (b *fakeWatchBackend) sendEvent(t *testing.T, path string) {
	t.Helper()
	b.sendBackendEvent(t, backendEvent{Path: path, Op: backendOpWrite})
}

func (b *fakeWatchBackend) sendBackendEvent(t *testing.T, event backendEvent) {
	t.Helper()
	select {
	case b.events <- event:
	case <-time.After(watcherTestTimeout):
		t.Fatalf("scheduler did not drain backend event for %q", event.Path)
	}
}

func (b *fakeWatchBackend) sendError(t *testing.T, err error) {
	t.Helper()
	select {
	case b.errors <- err:
	case <-time.After(watcherTestTimeout):
		t.Fatalf("scheduler did not drain backend error %q", err)
	}
}

func startFakeWatcherWithLimits(
	t *testing.T,
	backend *fakeWatchBackend,
	onChange func(WatchBatch),
	maxEntries, maxPathBytes int,
) *Watcher {
	t.Helper()
	w, err := newWatcherWithBackend(
		0,
		0,
		func(_ context.Context, batch WatchBatch) error {
			onChange(batch)
			return nil
		},
		backend,
		maxEntries,
		maxPathBytes,
	)
	require.NoError(t, err, "newWatcherWithBackend")
	w.Start()
	select {
	case <-backend.started:
	case <-time.After(watcherTestTimeout):
		t.Fatal("watch backend did not start")
	}
	return w
}

func startTestWatcherWithIntervalsNoCleanup(
	t *testing.T, onChange func([]string), batchDelay, minInterval time.Duration,
) (*Watcher, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := NewWatcherWithInterval(
		batchDelay,
		minInterval,
		func(batch WatchBatch) { onChange(batch.Paths) },
		nil,
	)
	require.NoError(t, err, "NewWatcherWithInterval")
	_, _, err = w.WatchRecursive(dir)
	require.NoError(t, err, "WatchRecursive")
	w.Start()
	return w, dir
}

func startTestWatcherWithIntervals(
	t *testing.T, onChange func([]string), batchDelay, minInterval time.Duration,
) (*Watcher, string) {
	t.Helper()
	w, dir := startTestWatcherWithIntervalsNoCleanup(
		t, onChange, batchDelay, minInterval,
	)
	t.Cleanup(w.Stop)
	return w, dir
}

// startTestWatcherNoCleanup sets up a watcher without registering
// t.Cleanup(w.Stop), for tests that explicitly exercise Stop().
func startTestWatcherNoCleanup(
	t *testing.T, onChange func([]string), delay time.Duration,
) (*Watcher, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := NewWatcher(
		delay,
		func(batch WatchBatch) { onChange(batch.Paths) },
		nil,
	)
	require.NoError(t, err, "NewWatcher")
	_, _, err = w.WatchRecursive(dir)
	require.NoError(t, err, "WatchRecursive")
	w.Start()
	return w, dir
}

func receiveWatcherCall(t *testing.T, calls <-chan watcherCall) watcherCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(watcherTestTimeout):
		t.Fatal("timed out waiting for watcher callback")
		return watcherCall{}
	}
}

func waitForPath(t *testing.T, calls <-chan []string, path string) {
	t.Helper()
	deadline := time.NewTimer(watcherTestTimeout)
	defer deadline.Stop()
	for {
		select {
		case paths := <-calls:
			if slices.Contains(paths, path) {
				return
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for watcher path %q", path)
		}
	}
}

func assertPathNotEmitted(
	t *testing.T, calls <-chan []string, path string, duration time.Duration,
) {
	t.Helper()
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	for {
		select {
		case paths := <-calls:
			if !assert.NotContains(t, paths, path) {
				return
			}
		case <-deadline.C:
			return
		}
	}
}

func receiveWatchBatch(t *testing.T, calls <-chan WatchBatch) WatchBatch {
	t.Helper()
	select {
	case batch := <-calls:
		return batch
	case <-time.After(watcherTestTimeout):
		t.Fatal("timed out waiting for watcher callback")
		return WatchBatch{}
	}
}

func updateMax(maximum *atomic.Int32, value int32) {
	for {
		previous := maximum.Load()
		if value <= previous || maximum.CompareAndSwap(previous, value) {
			return
		}
	}
}

// startTestWatcher encapsulates watcher setup and lifecycle.
func startTestWatcher(
	t *testing.T, onChange func([]string),
) (*Watcher, string) {
	t.Helper()
	w, dir := startTestWatcherNoCleanup(t, onChange, 50*time.Millisecond)
	t.Cleanup(func() { w.Stop() })
	return w, dir
}

func TestPendingWatchBatchOverflowsByEntryCount(t *testing.T) {
	pending := newPendingWatchBatch(2, 1_000)

	pending.Add("/sessions/a.jsonl")
	pending.Add("/sessions/b.jsonl")
	pending.Add("/sessions/c.jsonl")

	batch, ok := pending.Take()
	require.True(t, ok)
	assert.True(t, batch.FullSync)
	assert.Empty(t, batch.Paths)
}

func TestPendingWatchBatchOverflowsByPathBytes(t *testing.T) {
	pending := newPendingWatchBatch(10, len("/sessions/a.jsonl"))

	pending.Add("/sessions/a.jsonl")
	pending.Add("/sessions/b.jsonl")

	batch, ok := pending.Take()
	require.True(t, ok)
	assert.True(t, batch.FullSync)
	assert.Empty(t, batch.Paths)
}

func TestPendingWatchBatchCountsDuplicateOnce(t *testing.T) {
	path := "/sessions/a.jsonl"
	pending := newPendingWatchBatch(1, len(path))

	pending.Add(path)
	pending.Add(path)

	batch, ok := pending.Take()
	require.True(t, ok)
	assert.False(t, batch.FullSync)
	assert.Equal(t, []string{path}, batch.Paths)
}

func TestPendingWatchBatchTakeResetsBounds(t *testing.T) {
	pending := newPendingWatchBatch(1, 1_000)
	pending.Add("/sessions/a.jsonl")

	first, ok := pending.Take()
	require.True(t, ok)
	assert.Equal(t, []string{"/sessions/a.jsonl"}, first.Paths)
	_, ok = pending.Take()
	assert.False(t, ok, "taking an empty accumulator must not dispatch")

	pending.Add("/sessions/b.jsonl")
	second, ok := pending.Take()
	require.True(t, ok)
	assert.False(t, second.FullSync)
	assert.Equal(t, []string{"/sessions/b.jsonl"}, second.Paths)
}

func TestPendingWatchBatchBoundsAllRetainedMetadata(t *testing.T) {
	path := "/sessions/a.jsonl"
	root := "/sessions"
	pending := newPendingWatchBatch(3, 10_000)

	pending.Add(path)
	pending.AddRename(WatchRename{
		Path:     "/sessions/b.jsonl",
		Root:     root,
		ItemType: ItemIsFile,
	})
	pending.AddReconcileRoot("/other")

	batch, ok := pending.Take()
	require.True(t, ok)
	assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true}, batch,
		"overflow must discard every retained string")
}

func TestPendingWatchBatchBoundsMixedMetadataBytesIndependently(t *testing.T) {
	path := "/sessions/a.jsonl"
	renamePath := "/sessions/b.jsonl"
	root := "/sessions"
	otherRoot := "/other"
	agent := "codex"
	pending := newPendingWatchBatch(
		10, len(path)+len(renamePath)+len(root)+len(agent)+len(otherRoot)-1,
	)

	pending.Add(path)
	pending.AddRename(WatchRename{
		Path: renamePath, Root: root, Agent: agent, ItemType: ItemIsFile,
	})
	pending.AddReconcileRoot(otherRoot)

	batch, ok := pending.Take()
	require.True(t, ok)
	assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true}, batch,
		"byte overflow must discard every retained string")
}

func TestPendingWatchBatchDeduplicatesRenameAndRootMetadata(t *testing.T) {
	rename := WatchRename{
		Path:     "/sessions/a.jsonl",
		Root:     "/sessions",
		ItemType: ItemIsFile,
	}
	pending := newPendingWatchBatch(3, len(rename.Path)+len(rename.Root))

	pending.AddRename(rename)
	pending.AddRename(rename)
	pending.AddReconcileRoot(rename.Root)
	pending.AddReconcileRoot(rename.Root)

	batch, ok := pending.Take()
	require.True(t, ok)
	assert.False(t, batch.FullSync)
	assert.Equal(t, []WatchRename{rename}, batch.Renames)
	assert.Equal(t, []string{rename.Root}, batch.ReconcileRoots)
}

func TestPendingWatchBatchMetadataByteOverflowClearsStrings(t *testing.T) {
	rename := WatchRename{
		Path:     "/sessions/用户.jsonl",
		Root:     "/sessions",
		ItemType: ItemIsUnknown,
	}
	pending := newPendingWatchBatch(10, len(rename.Path)+len(rename.Root)-1)

	pending.AddRename(rename)

	batch, ok := pending.Take()
	require.True(t, ok)
	assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true}, batch)
}

func TestPendingWatchBatchRootCountOverflowEmitsOnlyFullSync(t *testing.T) {
	pending := newPendingWatchBatch(1, 1_000)

	pending.AddReconcileRoot("/sessions-a")
	pending.AddReconcileRoot("/sessions-b")

	batch, ok := pending.Take()
	require.True(t, ok)
	assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true}, batch)
}

func TestWatchEventSinkCountsOnlyCapacityOverflowAsOverflow(t *testing.T) {
	sink := newWatchEventSink(1, 1024)
	sink.Add(backendEvent{Op: backendOpFullSync})
	assert.Zero(t, sink.overflows.Load(), "authoritative reconciliation is not overflow")
	_, ok := sink.Take(nil)
	require.True(t, ok)

	sink.Add(backendEvent{Path: "first", Op: backendOpWrite})
	sink.Add(backendEvent{Path: "second", Op: backendOpWrite})
	assert.Equal(t, uint32(1), sink.overflows.Load())
}

func TestPendingWatchBatchFullSyncPreservesLifecycleMarkers(t *testing.T) {
	gate := &recordingLifecycleGate{acknowledged: make(chan uint64, 1)}
	token := backendLifecycleToken{gate: gate, generation: 9}
	pending := newPendingWatchBatch(2, 1024)
	pending.AddBackendEvent(backendEvent{
		Root: "/sync", Op: backendOpReconcileRootChange, Lifecycle: token,
	})

	pending.AddFullSync()
	pending.AddFullSync()
	batch, ok := pending.Take()

	require.True(t, ok)
	assert.True(t, batch.FullSync)
	assert.Equal(t, []backendLifecycleToken{token}, batch.lifecycleTokens)
}

func TestPendingWatchBatchFullSyncRetainsLaterLifecycleMarkers(t *testing.T) {
	gate := &recordingLifecycleGate{acknowledged: make(chan uint64, 1)}
	token := backendLifecycleToken{gate: gate, generation: 10}
	pending := newPendingWatchBatch(2, 1024)
	pending.AddFullSync()

	pending.AddBackendEvent(backendEvent{
		Root: "/sync", Op: backendOpReconcileRootChange, Lifecycle: token,
	})
	batch, ok := pending.Take()

	require.True(t, ok)
	assert.True(t, batch.FullSync)
	assert.Equal(t, []backendLifecycleToken{token}, batch.lifecycleTokens,
		"a lifecycle gate must remain acknowledgeable after full reconciliation")
}

func TestWatchEventSinkRetainsConcurrentAuthoritativeLifecycleMarkers(t *testing.T) {
	firstGate := &recordingLifecycleGate{acknowledged: make(chan uint64, 1)}
	secondGate := &recordingLifecycleGate{acknowledged: make(chan uint64, 1)}
	first := backendLifecycleToken{gate: firstGate, generation: 11}
	second := backendLifecycleToken{gate: secondGate, generation: 12}
	sink := newWatchEventSink(2, 1024)

	sink.RetainAuthoritative(first)
	sink.RetainAuthoritative(second)
	batch, ok := sink.Take(nil)

	require.True(t, ok)
	assert.True(t, batch.FullSync)
	assert.ElementsMatch(t, []backendLifecycleToken{first, second}, batch.lifecycleTokens,
		"separate lifecycle gates must not overwrite one another")
}

func TestWatcherBatchesPathsAndEnforcesDispatchFloor(t *testing.T) {
	const (
		batchDelay  = 50 * time.Millisecond
		minInterval = 200 * time.Millisecond
	)
	calls := make(chan watcherCall, 4)
	_, dir := startTestWatcherWithIntervals(t, func(paths []string) {
		calls <- watcherCall{paths: paths, at: time.Now()}
	}, batchDelay, minInterval)

	firstPath := filepath.Join(dir, "a.jsonl")
	secondPath := filepath.Join(dir, "b.jsonl")
	require.NoError(t, os.WriteFile(firstPath, []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(secondPath, []byte("b"), 0o644))

	first := receiveWatcherCall(t, calls)
	assert.Equal(t, []string{firstPath, secondPath}, first.paths,
		"one write burst should produce one unique path batch")

	laterPath := filepath.Join(dir, "c.jsonl")
	require.NoError(t, os.WriteFile(laterPath, []byte("c"), 0o644))
	second := receiveWatcherCall(t, calls)
	assert.GreaterOrEqual(t, second.at.Sub(first.at), minInterval,
		"callbacks started less than the configured minimum interval apart")
	assert.Contains(t, second.paths, laterPath)
}

func TestWatcherSustainedWritesProgress(t *testing.T) {
	const (
		batchDelay        = 50 * time.Millisecond
		minInterval       = 300 * time.Millisecond
		writeEvery        = 10 * time.Millisecond
		dispatchTolerance = 250 * time.Millisecond
	)
	calls := make(chan watcherCall, 4)
	_, dir := startTestWatcherWithIntervals(
		t, func(paths []string) {
			calls <- watcherCall{paths: paths, at: time.Now()}
		}, batchDelay, minInterval,
	)
	path := filepath.Join(dir, "active.jsonl")

	require.NoError(t, os.WriteFile(path, []byte("initial"), 0o644))
	stopWrites := make(chan struct{})
	writesDone := make(chan struct{})
	writeErr := make(chan error, 1)
	go func() {
		defer close(writesDone)
		writeTicker := time.NewTicker(writeEvery)
		defer writeTicker.Stop()
		for {
			select {
			case <-stopWrites:
				return
			case <-writeTicker.C:
				if err := os.WriteFile(path, []byte("update"), 0o644); err != nil {
					writeErr <- err
					return
				}
			}
		}
	}()
	var stopWriterOnce sync.Once
	stopWriter := func() {
		stopWriterOnce.Do(func() {
			close(stopWrites)
			<-writesDone
		})
	}
	t.Cleanup(stopWriter)

	receiveCall := func() watcherCall {
		t.Helper()
		select {
		case call := <-calls:
			return call
		case err := <-writeErr:
			require.NoError(t, err)
			return watcherCall{}
		case <-time.After(minInterval + dispatchTolerance):
			t.Fatal("continuous writes starved the watcher callback")
			return watcherCall{}
		}
	}

	first := receiveCall()
	second := receiveCall()
	stopWriter()
	select {
	case err := <-writeErr:
		require.NoError(t, err)
	default:
	}

	assert.Contains(t, first.paths, path)
	assert.Contains(t, second.paths, path)
	spacing := second.at.Sub(first.at)
	assert.GreaterOrEqual(t, spacing, minInterval,
		"sustained-write callbacks started too close together")
	assert.LessOrEqual(t, spacing, minInterval+dispatchTolerance,
		"sustained writes did not make bounded progress")
}

func TestWatcherSchedulerContinuesIntakeWithOnePendingAccumulator(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
	calls := make(chan WatchBatch, 4)
	var callCount atomic.Int32
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32

	backend := newFakeWatchBackend()
	w := startFakeWatcherWithLimits(t, backend, func(batch WatchBatch) {
		current := concurrent.Add(1)
		updateMax(&maxConcurrent, current)
		defer concurrent.Add(-1)

		if callCount.Add(1) == 1 {
			close(started)
			<-release
		}
		calls <- batch
	}, 8, 1_000)
	t.Cleanup(func() {
		releaseCallback()
		w.Stop()
	})

	backend.sendEvent(t, "/sessions/first.jsonl")
	select {
	case <-started:
	case <-time.After(watcherTestTimeout):
		t.Fatal("timed out waiting for the first callback to start")
	}

	backend.sendEvent(t, "/sessions/during-callback.jsonl")
	assert.Never(t, func() bool {
		return callCount.Load() > 1
	}, 50*time.Millisecond, 5*time.Millisecond,
		"a second callback started while the first callback was blocked")
	releaseCallback()

	firstBatch := receiveWatchBatch(t, calls)
	secondBatch := receiveWatchBatch(t, calls)
	assert.Equal(t, []string{"/sessions/first.jsonl"}, firstBatch.Paths)
	assert.Equal(t, []string{"/sessions/during-callback.jsonl"}, secondBatch.Paths)
	assert.Equal(t, int32(1), maxConcurrent.Load(),
		"watcher callbacks must remain serialized")
}

func TestWatcherOverflowCollapsesPathAndByteLimitsToOneFullSync(t *testing.T) {
	tests := []struct {
		name         string
		maxEntries   int
		maxPathBytes int
		paths        []string
	}{
		{
			name:         "path count",
			maxEntries:   2,
			maxPathBytes: 1_000,
			paths:        []string{"/sessions/a", "/sessions/b", "/sessions/c"},
		},
		{
			name:         "path bytes",
			maxEntries:   10,
			maxPathBytes: len("/sessions/a"),
			paths:        []string{"/sessions/a", "/sessions/b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			firstRelease := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(firstRelease) }) }
			calls := make(chan WatchBatch, 4)
			var callCount atomic.Int32
			backend := newFakeWatchBackend()
			w := startFakeWatcherWithLimits(t, backend, func(batch WatchBatch) {
				calls <- batch
				if callCount.Add(1) == 1 {
					<-firstRelease
				}
			}, tt.maxEntries, tt.maxPathBytes)
			t.Cleanup(func() {
				release()
				w.Stop()
			})

			backend.sendEvent(t, "/r")
			first := receiveWatchBatch(t, calls)
			assert.Equal(t, []string{"/r"}, first.Paths)

			for _, path := range tt.paths {
				backend.sendEvent(t, path)
			}
			assert.Equal(t, int32(1), callCount.Load(),
				"worker handoff must not queue another callback while one executes")

			release()
			overflow := receiveWatchBatch(t, calls)
			assert.True(t, overflow.FullSync)
			assert.Empty(t, overflow.Paths)
			assert.Equal(t, int32(2), callCount.Load(),
				"one overflow marker should replace all retained paths")
		})
	}
}

func TestWatcherCarriesRenameAndRootMetadata(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 2)
	w, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.SetRootAgents("/sessions", []string{"codex", "claude"})
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{
		Path:     "/sessions/renamed",
		Root:     "/sessions",
		Op:       backendOpRename,
		ItemType: backendItemDirectory,
	})
	renameBatch := receiveWatchBatch(t, calls)
	assert.Equal(t, []WatchRename{
		{
			Path:     "/sessions/renamed",
			Root:     "/sessions",
			Agent:    "claude",
			ItemType: ItemIsDir,
		},
		{
			Path:     "/sessions/renamed",
			Root:     "/sessions",
			Agent:    "codex",
			ItemType: ItemIsDir,
		},
	}, renameBatch.Renames)
	assert.Empty(t, renameBatch.Paths)

	backend.sendBackendEvent(t, backendEvent{
		Path: "/sessions", Root: "/sessions", Op: backendOpReconcileRootChange,
	})
	reconcileBatch := receiveWatchBatch(t, calls)
	assert.Equal(t, []string{"/sessions"}, reconcileBatch.ReconcileRoots)
	assert.Empty(t, reconcileBatch.Paths)
}

func TestWatcherDiscoversPrepopulatedCreatedDirectory(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 1)
	root := t.TempDir()
	created := filepath.Join(root, "imported")
	nested := filepath.Join(created, "nested")
	restored := filepath.Join(nested, "session.jsonl")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	require.NoError(t, os.WriteFile(restored, []byte("restored"), 0o600))
	w, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{
		Path:     created,
		Root:     root,
		Op:       backendOpCreate,
		ItemType: backendItemDirectory,
	})

	batch := receiveWatchBatch(t, calls)
	assert.Equal(t, []string{created, restored}, batch.Paths)
	assert.Empty(t, batch.ReconcileRoots)
}

func TestWatcherCreatedDirectoryDiscoveryHonorsBackendExclusions(t *testing.T) {
	backend := newFakeWatchBackend()
	backend.includeSubtree = func(root, path string) bool {
		return !shouldExcludeForRoot([]string{"node_modules"}, path, root)
	}
	calls := make(chan WatchBatch, 1)
	root := t.TempDir()
	created := filepath.Join(root, "imported")
	included := filepath.Join(created, "session.jsonl")
	excluded := filepath.Join(created, "node_modules", "cached.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(excluded), 0o755))
	require.NoError(t, os.WriteFile(included, []byte("included"), 0o600))
	require.NoError(t, os.WriteFile(excluded, []byte("excluded"), 0o600))
	w, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{
		Path: created, Root: root,
		Op:       backendOpCreate,
		ItemType: backendItemDirectory,
	})

	batch := receiveWatchBatch(t, calls)
	assert.Equal(t, []string{created, included}, batch.Paths)
	assert.NotContains(t, batch.Paths, excluded)
}

func TestWatcherCreatedDirectoryDiscoveryOverflowReconcilesOwningRoot(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 1)
	root := t.TempDir()
	created := filepath.Join(root, "imported")
	require.NoError(t, os.MkdirAll(created, 0o755))
	for i := range 4 {
		path := filepath.Join(created, fmt.Sprintf("session-%d.jsonl", i))
		require.NoError(t, os.WriteFile(path, []byte("restored"), 0o600))
	}
	w, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			return nil
		},
		backend, 3, 10_000,
	)
	require.NoError(t, err)
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{
		Path: created, Root: root,
		Op:       backendOpCreate,
		ItemType: backendItemDirectory,
	})

	batch := receiveWatchBatch(t, calls)
	assert.False(t, batch.FullSync)
	assert.Equal(t, []string{root}, batch.ReconcileRoots)
	assert.LessOrEqual(t, len(batch.Paths), 2)
}

func TestWatcherCreatedDirectoryVisitOverflowReconcilesOwningRoot(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 1)
	root := t.TempDir()
	created := filepath.Join(root, "imported")
	require.NoError(t, os.MkdirAll(created, 0o755))
	for i := range 4 {
		require.NoError(t, os.Mkdir(
			filepath.Join(created, fmt.Sprintf("directory-%d", i)), 0o755,
		))
	}
	w, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			return nil
		},
		backend, 3, 10_000,
	)
	require.NoError(t, err)
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{
		Path: created, Root: root,
		Op:       backendOpCreate,
		ItemType: backendItemDirectory,
	})

	batch := receiveWatchBatch(t, calls)
	assert.False(t, batch.FullSync)
	assert.Equal(t, []string{created}, batch.Paths)
	assert.Equal(t, []string{root}, batch.ReconcileRoots)
}

func TestWatcherStopInterruptsCreatedDirectoryTraversal(t *testing.T) {
	backend := newFakeWatchBackend()
	root := t.TempDir()
	created := filepath.Join(root, "imported")
	require.NoError(t, os.MkdirAll(created, 0o755))
	for i := range 200 {
		require.NoError(t, os.Mkdir(
			filepath.Join(created, fmt.Sprintf("directory-%03d", i)), 0o755,
		))
	}
	firstVisit := make(chan struct{})
	releaseFirstVisit := make(chan struct{})
	secondVisit := make(chan struct{})
	releaseSecondVisit := make(chan struct{})
	var visits atomic.Int32
	backend.includeSubtree = func(_, _ string) bool {
		switch visits.Add(1) {
		case 1:
			close(firstVisit)
			<-releaseFirstVisit
		case 2:
			close(secondVisit)
			<-releaseSecondVisit
		}
		return true
	}
	w, err := newWatcherWithBackend(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 512, 100_000,
	)
	require.NoError(t, err)
	w.Start()

	backend.sendBackendEvent(t, backendEvent{
		Path: created, Root: root,
		Op:       backendOpCreate,
		ItemType: backendItemDirectory,
	})
	select {
	case <-firstVisit:
	case <-time.After(watcherTestTimeout):
		require.FailNow(t, "created-directory traversal did not start")
	}

	stopped := make(chan struct{})
	go func() {
		w.Stop()
		close(stopped)
	}()
	select {
	case <-backend.stopped:
	case <-time.After(watcherTestTimeout):
		require.FailNow(t, "watcher backend did not receive Stop")
	}
	close(releaseFirstVisit)

	traversalContinued := false
	select {
	case <-stopped:
	case <-secondVisit:
		traversalContinued = true
		close(releaseSecondVisit)
		<-stopped
	case <-time.After(watcherTestTimeout):
		close(releaseSecondVisit)
		require.FailNow(t, "watcher Stop did not finish")
	}
	assert.False(t, traversalContinued,
		"created subtree traversal visited another entry after Stop")
}

func TestWatcherRetriesFailedReconciliationMarker(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 2)
	var attempts atomic.Int32
	w, err := newWatcherWithBackend(
		0, 10*time.Millisecond,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			if attempts.Add(1) == 1 {
				return errors.New("discovery incomplete")
			}
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{
		Path: "/sessions", Root: "/sessions", Op: backendOpReconcileRootChange,
	})
	first := receiveWatchBatch(t, calls)
	second := receiveWatchBatch(t, calls)
	assert.Equal(t, []string{"/sessions"}, first.ReconcileRoots)
	assert.Equal(t, first, second, "failed reconciliation must retain one retry marker")
}

func TestRetriedFullSyncPreservesChangesArrivingDuringCallback(t *testing.T) {
	pending := newPendingWatchBatch(8, 1_000)
	pending.Add("/sessions/new.jsonl")

	retainWatchRetry(pending, WatchBatch{FullSync: true})
	full, ok := pending.Take()
	require.True(t, ok)
	assert.Equal(t, WatchBatch{FullSync: true}, full)
	changed, ok := pending.Take()
	require.True(t, ok)
	assert.Equal(t, []string{"/sessions/new.jsonl"}, changed.Paths)
}

func TestRetriedLostEventReconciliationPreservesRecoveryMode(t *testing.T) {
	pending := newPendingWatchBatch(8, 1_000)

	retainWatchRetry(pending, WatchBatch{
		ReconcileRoots: []string{"/sessions"},
		LostEvents:     true,
	})
	batch, ok := pending.Take()

	require.True(t, ok)
	assert.Equal(t, WatchBatch{
		Paths:          []string{},
		Renames:        []WatchRename{},
		ReconcileRoots: []string{"/sessions"},
		LostEvents:     true,
	}, batch)
}

func TestWatcherRetryDelayUsesBoundedExponentialBackoff(t *testing.T) {
	assert.Equal(t, 5*time.Second, watcherRetryDelay(5*time.Second, 1))
	assert.Equal(t, 10*time.Second, watcherRetryDelay(5*time.Second, 2))
	assert.Equal(t, watcherRetryMaxDelay, watcherRetryDelay(5*time.Second, 20))
	assert.Equal(t, time.Millisecond, watcherRetryDelay(0, 1))
	assert.Zero(t, watcherRetryDelay(time.Second, 0))
}

func TestWatcherRetryFloorStartsWhenFailedCallbackCompletes(t *testing.T) {
	backend := newFakeWatchBackend()
	const retryFloor = 40 * time.Millisecond
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstCompleted := make(chan time.Time, 1)
	secondStarted := make(chan time.Time, 1)
	var attempts atomic.Int32
	w, err := newWatcherWithBackend(
		0, retryFloor,
		func(_ context.Context, _ WatchBatch) error {
			if attempts.Add(1) == 1 {
				close(firstStarted)
				<-releaseFirst
				firstCompleted <- time.Now()
				return retryScopedWatchError{retry: WatchBatch{
					ReconcileRoots: []string{"/sessions"},
				}}
			}
			secondStarted <- time.Now()
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{
		Path: "/sessions", Root: "/sessions", Op: backendOpReconcileRootChange,
	})
	requireReceiveWithin(t, firstStarted, time.Second)
	time.Sleep(retryFloor + 10*time.Millisecond)
	close(releaseFirst)
	completedAt := requireReceiveWithin(t, firstCompleted, time.Second)
	retriedAt := requireReceiveWithin(t, secondStarted, time.Second)

	assert.GreaterOrEqual(t, retriedAt.Sub(completedAt), retryFloor,
		"a slow failure must not make its retry immediately eligible")
}

func TestWatcherDoesNotReplayOrdinaryBatchOnCallbackError(t *testing.T) {
	backend := newFakeWatchBackend()
	var attempts atomic.Int32
	w, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, _ WatchBatch) error {
			attempts.Add(1)
			return errors.New("ordinary sync error")
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendEvent(t, "/sessions/a.jsonl")
	require.Eventually(t, func() bool { return attempts.Load() == 1 },
		watcherTestTimeout, 5*time.Millisecond)
	assert.Never(t, func() bool { return attempts.Load() > 1 },
		50*time.Millisecond, 5*time.Millisecond)
}

func TestWatcherDoesNotReplayKnownFileRenameOnCallbackError(t *testing.T) {
	backend := newFakeWatchBackend()
	var attempts atomic.Int32
	w, err := newWatcherWithBackend(
		0, 0,
		func(_ context.Context, _ WatchBatch) error {
			attempts.Add(1)
			return errors.New("changed-path sync error")
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.SetRootAgents("/sessions", []string{"codex"})
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{
		Path: "/sessions/a.jsonl", Root: "/sessions",
		Op: backendOpRename, ItemType: backendItemFile,
	})
	require.Eventually(t, func() bool { return attempts.Load() == 1 },
		watcherTestTimeout, 5*time.Millisecond)
	assert.Never(t, func() bool { return attempts.Load() > 1 },
		50*time.Millisecond, 5*time.Millisecond)
}

func TestWatcherMixedFileRenameErrorRetriesOnlyRootMarker(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 2)
	var attempts atomic.Int32
	w, err := newWatcherWithBackend(
		0, 10*time.Millisecond,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			if attempts.Add(1) == 1 {
				return errors.New("root discovery incomplete")
			}
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.SetRootAgents("/sessions", []string{"codex"})
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{
		Path: "/sessions/a.jsonl", Root: "/sessions",
		Op:       backendOpRename | backendOpReconcileRootChange,
		ItemType: backendItemFile,
	})
	first := receiveWatchBatch(t, calls)
	second := receiveWatchBatch(t, calls)
	require.Len(t, first.Renames, 1)
	assert.Equal(t, ItemIsFile, first.Renames[0].ItemType)
	assert.Equal(t, []string{"/sessions"}, first.ReconcileRoots)
	assert.False(t, second.FullSync)
	assert.Empty(t, second.Paths)
	assert.Empty(t, second.Renames)
	assert.Equal(t, []string{"/sessions"}, second.ReconcileRoots)
}

func TestWatcherDirectoryRenameErrorRetriesFullSync(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 2)
	var attempts atomic.Int32
	w, err := newWatcherWithBackend(
		0, 10*time.Millisecond,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			if attempts.Add(1) == 1 {
				return errors.New("full discovery incomplete")
			}
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.SetRootAgents("/sessions", []string{"codex"})
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{
		Path: "/sessions/renamed", Root: "/sessions",
		Op: backendOpRename, ItemType: backendItemDirectory,
	})
	first := receiveWatchBatch(t, calls)
	second := receiveWatchBatch(t, calls)
	require.Len(t, first.Renames, 1)
	assert.Equal(t, ItemIsDir, first.Renames[0].ItemType)
	assert.Equal(t, WatchBatch{FullSync: true}, second)
}

type retryScopedWatchError struct {
	retry WatchBatch
}

func (e retryScopedWatchError) Error() string { return "authoritative reconciliation failed" }

func (e retryScopedWatchError) WatchRetryBatch() WatchBatch { return e.retry }

func TestWatcherPreservesLostEventsWhenFullRetryNarrowsToRoots(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 2)
	var attempts atomic.Int32
	w, err := newWatcherWithBackend(
		0, 10*time.Millisecond,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			if attempts.Add(1) == 1 {
				return retryScopedWatchError{retry: WatchBatch{
					ReconcileRoots: []string{"/sessions"},
					LostEvents:     true,
				}}
			}
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{Op: backendOpFullSync})
	first := receiveWatchBatch(t, calls)
	second := receiveWatchBatch(t, calls)

	assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true}, first)
	assert.Empty(t, second.Paths)
	assert.Empty(t, second.Renames)
	assert.Equal(t, []string{"/sessions"}, second.ReconcileRoots)
	assert.True(t, second.LostEvents)
}

func TestWatcherPreservesLostEventsWhenAmbiguousRenameRetryPromotesToFullSync(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 3)
	releaseFirst := make(chan struct{})
	var attempts atomic.Int32
	w, err := newWatcherWithBackend(
		0, 100*time.Millisecond,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			switch attempts.Add(1) {
			case 1:
				<-releaseFirst
				return retryScopedWatchError{retry: WatchBatch{
					ReconcileRoots: []string{"/sessions"},
					LostEvents:     true,
				}}
			case 2:
				return errors.New("rename classification failed")
			default:
				return nil
			}
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.SetRootAgents("/sessions", []string{"codex"})
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendBackendEvent(t, backendEvent{Op: backendOpFullSync})
	first := receiveWatchBatch(t, calls)
	backend.sendBackendEvent(t, backendEvent{
		Path: "/sessions/renamed", Root: "/sessions",
		Op: backendOpRename, ItemType: backendItemUnknown,
	})
	close(releaseFirst)
	second := receiveWatchBatch(t, calls)
	third := receiveWatchBatch(t, calls)

	assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true}, first)
	require.Len(t, second.Renames, 1)
	assert.Equal(t, ItemIsUnknown, second.Renames[0].ItemType)
	assert.Equal(t, []string{"/sessions"}, second.ReconcileRoots)
	assert.True(t, second.LostEvents)
	assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true}, third)
}

func TestWatcherUsesCallbackReconciliationScopeForRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		retry WatchBatch
	}{
		{
			name:  "classified ordinary rename retries roots",
			retry: WatchBatch{ReconcileRoots: []string{"/sessions", "/sessions"}},
		},
		{
			name:  "classified authoritative rename retries full sync",
			retry: WatchBatch{FullSync: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := newFakeWatchBackend()
			calls := make(chan WatchBatch, 2)
			var attempts atomic.Int32
			w, err := newWatcherWithBackend(
				0, 10*time.Millisecond,
				func(_ context.Context, batch WatchBatch) error {
					calls <- batch
					if attempts.Add(1) == 1 {
						return retryScopedWatchError{retry: tc.retry}
					}
					return nil
				},
				backend, 8, 1_000,
			)
			require.NoError(t, err)
			w.SetRootAgents("/sessions", []string{"codex"})
			w.Start()
			t.Cleanup(w.Stop)

			backend.sendBackendEvent(t, backendEvent{
				Path: "/sessions/renamed", Root: "/sessions",
				Op:       backendOpRename | backendOpReconcileRootChange,
				ItemType: backendItemUnknown,
			})
			first := receiveWatchBatch(t, calls)
			second := receiveWatchBatch(t, calls)
			require.Len(t, first.Renames, 1)
			assert.Equal(t, ItemIsUnknown, first.Renames[0].ItemType)
			if tc.retry.FullSync {
				assert.Equal(t, WatchBatch{FullSync: true}, second)
			} else {
				assert.False(t, second.FullSync)
				assert.Empty(t, second.Paths)
				assert.Empty(t, second.Renames)
				assert.Equal(t, []string{"/sessions"}, second.ReconcileRoots)
			}
		})
	}
}

func TestWatcherUsesCallbackChangedPathsForRetry(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 2)
	var attempts atomic.Int32
	w, err := newWatcherWithBackend(
		0, 10*time.Millisecond,
		func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			if attempts.Add(1) == 1 {
				return retryScopedWatchError{retry: WatchBatch{Paths: []string{
					"/sessions/changed.jsonl",
					"/sessions/changed.jsonl",
				}}}
			}
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.Start()
	t.Cleanup(w.Stop)

	backend.sendEvent(t, "/sessions/changed.jsonl")
	first := receiveWatchBatch(t, calls)
	second := receiveWatchBatch(t, calls)

	assert.Equal(t, []string{"/sessions/changed.jsonl"}, first.Paths)
	assert.Equal(t, []string{"/sessions/changed.jsonl"}, second.Paths,
		"a failed changed-path sync must retry the exact bounded path")
	assert.False(t, second.FullSync)
}

func TestRetainWatchRetryPathOverflowPromotesFullSync(t *testing.T) {
	pending := newPendingWatchBatch(1, 1_000)

	retainWatchRetry(pending, WatchBatch{Paths: []string{
		"/sessions/one.jsonl",
		"/sessions/two.jsonl",
	}})
	batch, ok := pending.Take()

	require.True(t, ok)
	assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true}, batch,
		"retry paths use the normal bounded accumulator")
}

func TestWatcherStopCancelsPendingCallback(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 1)
	w, err := newWatcherWithBackend(
		300*time.Millisecond, time.Second, func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			return nil
		},
		backend, 8, 1_000,
	)
	require.NoError(t, err)
	w.Start()
	backend.sendEvent(t, "/sessions/pending.jsonl")

	w.Stop()
	select {
	case <-backend.stopped:
	case <-time.After(watcherTestTimeout):
		t.Fatal("watch backend was not stopped")
	}
	select {
	case batch := <-calls:
		t.Fatalf("callback ran after Stop with batch %+v", batch)
	case <-time.After(350 * time.Millisecond):
	}
}

func TestWatcherStopWaitsForRunningCallbackAndDiscardsPending(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int32

	backend := newFakeWatchBackend()
	w := startFakeWatcherWithLimits(t, backend, func(WatchBatch) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
	}, 8, 1_000)
	t.Cleanup(func() {
		releaseCallback()
		w.Stop()
	})

	backend.sendEvent(t, "/sessions/running.jsonl")
	select {
	case <-started:
	case <-time.After(watcherTestTimeout):
		t.Fatal("timed out waiting for the first callback to start")
	}

	backend.sendEvent(t, "/sessions/queued.jsonl")

	stopStarted := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		close(stopStarted)
		w.Stop()
		close(stopped)
	}()
	<-stopStarted
	select {
	case <-stopped:
		t.Fatal("Stop returned before the running callback completed")
	case <-time.After(50 * time.Millisecond):
	}

	releaseCallback()
	select {
	case <-stopped:
	case <-time.After(watcherTestTimeout):
		t.Fatal("Stop did not return after the running callback completed")
	}
	assert.Equal(t, int32(1), calls.Load(),
		"Stop must discard the queued second callback")
}

func TestWatcherSchedulerBackendErrorsRemainDrainable(t *testing.T) {
	previousLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(previousLogOutput) })

	backend := newFakeWatchBackend()
	var calls atomic.Int32
	w := startFakeWatcherWithLimits(t, backend, func(WatchBatch) {
		calls.Add(1)
	}, 8, 1_000)
	t.Cleanup(w.Stop)

	for i := range 1_000 {
		backend.sendError(t, fmt.Errorf("backend error %d", i))
	}

	assert.Zero(t, calls.Load(), "backend errors must not schedule callbacks")
}

func TestWatcherCallsOnChange(t *testing.T) {
	pathsCh := make(chan []string, 1)

	_, dir := startTestWatcher(t, func(paths []string) {
		select {
		case pathsCh <- paths:
		default:
		}
	})

	path := filepath.Join(dir, "test.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

	select {
	case gotPaths := <-pathsCh:
		require.NotEmpty(t, gotPaths, "onChange called with empty paths")
		require.True(t, slices.Contains(gotPaths, path),
			"onChange did not contain expected path %s, got %v", path, gotPaths)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for onChange callback")
	}
}

func TestWatcherAutoWatchesNewDirs(t *testing.T) {
	pathsCh := make(chan []string, 10)

	_, dir := startTestWatcher(t, func(paths []string) {
		pathsCh <- paths
	})

	subdir := filepath.Join(dir, "newdir")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	// Delivery of the directory create means the backend has completed its
	// auto-watch decision before handing the event to the scheduler.
	waitForPath(t, pathsCh, subdir)

	nestedPath := filepath.Join(subdir, "nested.jsonl")
	require.NoError(t, os.WriteFile(nestedPath, []byte("nested"), 0o644))

	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		select {
		case paths := <-pathsCh:
			if slices.Contains(paths, nestedPath) {
				found = true
			}
		case <-time.After(50 * time.Millisecond):
		}
	}

	require.True(t, found, "timed out waiting for nested file change")
}

func TestWatcherStopIsClean(t *testing.T) {
	w, _ := startTestWatcherNoCleanup(t, func(_ []string) {}, 50*time.Millisecond)

	stopped := make(chan struct{})
	go func() {
		w.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return in time")
	}
}

func TestWatcherLifecycleStopBeforeStartReturns(t *testing.T) {
	backend := newFakeWatchBackend()
	w, err := newWatcherWithBackend(
		0, 0, func(context.Context, WatchBatch) error { return nil }, backend, 8, 1_000,
	)
	require.NoError(t, err)

	stopped := make(chan struct{})
	go func() {
		w.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Stop blocked before Start")
	}
	w.Start()
	w.Stop()
	assert.Zero(t, backend.startCalls.Load(), "Start after Stop must be a no-op")
	assert.Equal(t, int32(1), backend.stopCalls.Load())
}

func TestWatcherLifecycleStartFailureReturnsErrorAndDegradesRegisteredScopes(t *testing.T) {
	backend := newFakeWatchBackend()
	startErr := errors.New("start failed")
	backend.startErr = startErr
	degraded := make(chan []string, 1)
	w, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil }, backend, 8, 1_000,
		WatcherOptions{OnCoverageDegraded: func(roots []string) error {
			degraded <- append([]string(nil), roots...)
			return nil
		}},
	)
	require.NoError(t, err)
	w.RegisterRoots([]WatchRoot{
		{Path: "/watch-b", Scopes: []WatchScope{{SyncDir: "/scope-b"}}},
		{Path: "/watch-a", Scopes: []WatchScope{
			{SyncDir: "/scope-b"}, {SyncDir: "/scope-a"},
		}},
	}, 8)

	err = w.Start()
	assert.ErrorIs(t, err, startErr)
	assert.Equal(t, []string{"/scope-a", "/scope-b"},
		requireReceiveWithin(t, degraded, time.Second))
	select {
	case <-backend.stopped:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("failed Start did not stop its backend")
	}
	w.Stop()
	assert.Error(t, w.Start(), "a stopped watcher must keep surfacing startup failure")
	assert.Equal(t, int32(1), backend.startCalls.Load())
	assert.Equal(t, int32(1), backend.stopCalls.Load())
}

func TestWatcherLifecycleRepeatedStartStartsOnce(t *testing.T) {
	backend := newFakeWatchBackend()
	w, err := newWatcherWithBackend(
		0, 0, func(context.Context, WatchBatch) error { return nil }, backend, 8, 1_000,
	)
	require.NoError(t, err)

	w.Start()
	w.Start()
	require.Equal(t, int32(1), backend.startCalls.Load())
	w.Stop()
	assert.Equal(t, int32(1), backend.stopCalls.Load())
}

func TestWatcherLifecycleStartStopRace(t *testing.T) {
	for range 100 {
		backend := newFakeWatchBackend()
		w, err := newWatcherWithBackend(
			0, 0, func(context.Context, WatchBatch) error { return nil }, backend, 8, 1_000,
		)
		require.NoError(t, err)

		start := make(chan struct{})
		var calls sync.WaitGroup
		calls.Add(2)
		go func() {
			defer calls.Done()
			<-start
			w.Start()
		}()
		go func() {
			defer calls.Done()
			<-start
			w.Stop()
		}()
		close(start)

		done := make(chan struct{})
		go func() {
			calls.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("concurrent Start and Stop deadlocked")
		}
		w.Start()
		w.Stop()
		assert.LessOrEqual(t, backend.startCalls.Load(), int32(1))
		assert.Equal(t, int32(1), backend.stopCalls.Load())
	}
}

func TestWatcherStopIdempotency(t *testing.T) {
	w, _ := startTestWatcherNoCleanup(t, func(_ []string) {}, 50*time.Millisecond)

	// 1. Sequential double stop
	w.Stop()
	w.Stop()

	// 2. Concurrent stop attempts
	pathsCh2 := make(chan []string, 10)
	w2, dir2 := startTestWatcherNoCleanup(
		t, func(paths []string) {
			pathsCh2 <- paths
		}, 50*time.Millisecond,
	)

	stressPath := filepath.Join(dir2, "stress.txt")
	require.NoError(t, os.WriteFile(stressPath, []byte("data"), 0o644), "stress write")

	// Wait for fsnotify to process it before concurrent stop
	select {
	case <-pathsCh2:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stress file to be processed")
	}

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			w2.Stop()
		})
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Stop() timed out")
	}
}

func TestWatcherIgnoresNonWriteCreate(t *testing.T) {
	pathsCh := make(chan []string, 10)
	w, dir := startTestWatcherNoCleanup(t, func(paths []string) {
		pathsCh <- paths
	}, 10*time.Millisecond)
	t.Cleanup(func() { w.Stop() })

	path := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	// Wait for the initial write event to clear
	select {
	case <-pathsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial write event")
	}

	// Now do a chmod (should be ignored)
	require.NoError(t, os.Chmod(path, 0o666))

	// Wait beyond the configured batch delay to prove chmod did not schedule a
	// callback.
	select {
	case <-pathsCh:
		t.Fatal("onChange called for chmod event, expected it to be ignored")
	case <-time.After(100 * time.Millisecond):
		// Success
	}
}

func TestWatcherHandlesRemoveAndRename(t *testing.T) {
	dir := t.TempDir()
	removePath := filepath.Join(dir, "remove.jsonl")
	renamePath := filepath.Join(dir, "rename.jsonl")
	renamedPath := filepath.Join(dir, "renamed.jsonl")
	require.NoError(t, os.WriteFile(removePath, []byte("remove"), 0o644))
	require.NoError(t, os.WriteFile(renamePath, []byte("rename"), 0o644))

	pathsCh := make(chan []string, 4)
	w, err := NewWatcherWithInterval(
		30*time.Millisecond,
		30*time.Millisecond,
		func(batch WatchBatch) { pathsCh <- batch.Paths },
		nil,
	)
	require.NoError(t, err, "NewWatcherWithInterval")
	_, _, err = w.WatchRecursive(dir)
	require.NoError(t, err, "WatchRecursive")
	w.Start()
	t.Cleanup(w.Stop)

	require.NoError(t, os.Remove(removePath))
	require.NoError(t, os.Rename(renamePath, renamedPath))

	var got []string
	deadline := time.NewTimer(watcherTestTimeout)
	defer deadline.Stop()
	for !slices.Contains(got, removePath) || !slices.Contains(got, renamePath) {
		select {
		case paths := <-pathsCh:
			got = append(got, paths...)
		case <-deadline.C:
			t.Fatalf("remove and rename paths not delivered; got %v", got)
		}
	}
	assert.Contains(t, got, removePath)
	assert.Contains(t, got, renamePath)
}

func TestWatchRecursive_ExcludesDirectoryNames(t *testing.T) {
	w := newFSNotifyTestWatcher(t, []string{".git", "node_modules"})

	root := t.TempDir()
	included := filepath.Join(root, "project", "src")
	excludedGit := filepath.Join(root, "project", ".git", "objects")
	excludedModules := filepath.Join(root, "project", "node_modules", "pkg")
	for _, p := range []string{included, excludedGit, excludedModules} {
		require.NoError(t, os.MkdirAll(p, 0o755), "MkdirAll(%s)", p)
	}

	watched, unwatched, err := w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")
	assert.Equal(t, 3, watched, "only root, project, and src should be watched")
	assert.Zero(t, unwatched)
}

func TestWatchRecursiveBudget_DegradesWhenBudgetExhausted(t *testing.T) {
	root := t.TempDir()
	for i := range 5 {
		require.NoError(t, os.MkdirAll(filepath.Join(root, fmt.Sprintf("dir-%d", i)), 0o755))
	}

	w := newFSNotifyTestWatcher(t, nil)

	result := w.WatchRecursiveBudgeted(root, 3)
	assert.Equal(t, 3, result.Watched)
	assert.True(t, result.BudgetExhausted, "BudgetExhausted = false, want true")
}

func TestIsWatchResourceExhaustion(t *testing.T) {
	assert.True(t, isWatchResourceExhaustion(syscall.EMFILE), "EMFILE should be resource exhaustion")
	assert.True(t, isWatchResourceExhaustion(syscall.ENOSPC), "ENOSPC should be resource exhaustion")
	assert.False(t, isWatchResourceExhaustion(os.ErrNotExist), "ErrNotExist should not be resource exhaustion")
}

func TestWatcherAutoWatchesNewDirs_RespectsExcludes(t *testing.T) {
	pathsCh := make(chan []string, 10)
	w, err := NewWatcher(20*time.Millisecond, func(batch WatchBatch) {
		pathsCh <- batch.Paths
	}, []string{".git"})
	require.NoError(t, err, "NewWatcher")
	t.Cleanup(func() { w.Stop() })

	root := t.TempDir()
	_, _, err = w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")
	w.Start()

	gitDir := filepath.Join(root, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755), "Mkdir(.git)")
	barrier := filepath.Join(root, "included-barrier")
	require.NoError(t, os.Mkdir(barrier, 0o755), "Mkdir(barrier)")
	waitForPath(t, pathsCh, barrier)

	fileInGit := filepath.Join(gitDir, "config")
	require.NoError(t, os.WriteFile(fileInGit, []byte("x"), 0o644))

	assertPathNotEmitted(t, pathsCh, fileInGit, 200*time.Millisecond)
}

func TestWatcherShallowRootDoesNotAutoWatchNewDirs(t *testing.T) {
	pathsCh := make(chan []string, 10)
	w, err := NewWatcher(20*time.Millisecond, func(batch WatchBatch) {
		pathsCh <- batch.Paths
	}, nil)
	require.NoError(t, err, "NewWatcher")
	t.Cleanup(func() { w.Stop() })

	root := t.TempDir()
	require.True(t, w.WatchShallow(root), "WatchShallow")
	w.Start()

	localDir := filepath.Join(root, "local_session")
	require.NoError(t, os.Mkdir(localDir, 0o755), "Mkdir(local)")

	select {
	case paths := <-pathsCh:
		assert.Contains(t, paths, localDir,
			"root-level create should still trigger onChange")
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for shallow root create event")
	}

	nested := filepath.Join(localDir, "nested.jsonl")
	require.NoError(t, os.WriteFile(nested, []byte("x"), 0o644))
	assertPathNotEmitted(t, pathsCh, nested, 100*time.Millisecond)
}

// A shallow parent root (e.g. Codex's .codex) must not shadow a recursive
// child root (.codex/sessions) nested inside it: newly created directories
// under the recursive child still need to be auto-watched so new sessions in
// new date directories live-sync.
func TestWatcherShallowParentDoesNotShadowRecursiveChild(t *testing.T) {
	pathsCh := make(chan []string, 10)
	w, err := NewWatcher(20*time.Millisecond, func(batch WatchBatch) {
		pathsCh <- batch.Paths
	}, nil)
	require.NoError(t, err, "NewWatcher")
	t.Cleanup(func() { w.Stop() })

	parent := t.TempDir()
	child := filepath.Join(parent, "sessions")
	require.NoError(t, os.Mkdir(child, 0o755), "Mkdir(child)")

	require.True(t, w.WatchShallow(parent), "WatchShallow(parent)")
	_, _, err = w.WatchRecursive(child)
	require.NoError(t, err, "WatchRecursive(child)")
	w.Start()

	// A sibling write directly under the shallow parent is still seen but its
	// directory is not auto-watched.
	logDir := filepath.Join(parent, "log")
	require.NoError(t, os.Mkdir(logDir, 0o755), "Mkdir(log)")

	// A new directory created under the recursive child must be auto-watched
	// even though it also sits inside the shallow parent root.
	dateDir := filepath.Join(child, "2026-06-16")
	require.NoError(t, os.Mkdir(dateDir, 0o755), "Mkdir(dateDir)")
	waitForPath(t, pathsCh, dateDir)

	sessionFile := filepath.Join(dateDir, "rollout.jsonl")
	require.NoError(t, os.WriteFile(sessionFile, []byte("x"), 0o644))

	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		select {
		case paths := <-pathsCh:
			if slices.Contains(paths, sessionFile) {
				found = true
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	require.True(t, found,
		"file in a new date dir under the recursive child must trigger onChange")
}

func TestWatchRecursive_RootUnderExcludedAncestorStillWatchesDescendants(t *testing.T) {
	w := newFSNotifyTestWatcher(t, []string{"venv"})

	base := t.TempDir()
	root := filepath.Join(base, "venv", "project")
	included := filepath.Join(root, "src")
	require.NoError(t, os.MkdirAll(included, 0o755), "MkdirAll(%s)", included)

	watched, unwatched, err := w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")
	assert.Equal(t, 2, watched, "root and descendant should both be watched")
	assert.Zero(t, unwatched)
}

func TestWatchRecursive_ExcludesSlashPatternRelativeToRoot(t *testing.T) {
	w := newFSNotifyTestWatcher(t, []string{"foo/bar"})

	root := t.TempDir()
	excluded := filepath.Join(root, "foo", "bar")
	includedSibling := filepath.Join(root, "foo", "baz")
	for _, p := range []string{excluded, includedSibling} {
		require.NoError(t, os.MkdirAll(p, 0o755), "MkdirAll(%s)", p)
	}

	watched, unwatched, err := w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")
	assert.Equal(t, 3, watched, "root, foo, and foo/baz should be watched")
	assert.Zero(t, unwatched)
}

func TestWatchRecursive_OverlappingRoots_UsesMostSpecificRoot(t *testing.T) {
	w := newFSNotifyTestWatcher(t, []string{"venv"})

	base := t.TempDir()
	parentRoot := filepath.Join(base, "workspace")
	nestedRoot := filepath.Join(parentRoot, "venv", "project")
	included := filepath.Join(nestedRoot, "src")
	for _, p := range []string{parentRoot, included} {
		require.NoError(t, os.MkdirAll(p, 0o755), "MkdirAll(%s)", p)
	}

	parentWatched, parentUnwatched, err := w.WatchRecursive(parentRoot)
	require.NoError(t, err, "WatchRecursive(parent)")
	assert.Equal(t, 1, parentWatched,
		"the excluded venv subtree should be skipped from the parent root")
	assert.Zero(t, parentUnwatched)

	nestedWatched, nestedUnwatched, err := w.WatchRecursive(nestedRoot)
	require.NoError(t, err, "WatchRecursive(nested)")
	assert.Equal(t, 2, nestedWatched,
		"the nested root and descendant should use the nested exclusion scope")
	assert.Zero(t, nestedUnwatched)
}

func TestWatcherExcludedCreateDir_DoesNotTriggerOnChange(t *testing.T) {
	pathsCh := make(chan []string, 10)
	w, err := NewWatcher(20*time.Millisecond, func(batch WatchBatch) {
		pathsCh <- batch.Paths
	}, []string{".git"})
	require.NoError(t, err, "NewWatcher")
	t.Cleanup(func() { w.Stop() })

	root := t.TempDir()
	_, _, err = w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")
	w.Start()

	gitDir := filepath.Join(root, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755), "Mkdir(.git)")

	select {
	case paths := <-pathsCh:
		assert.NotContains(t, paths, gitDir,
			"excluded directory create should not trigger onChange")
	case <-time.After(250 * time.Millisecond):
		// Expected: no callback for excluded dir creation.
	}
}

func TestNewWatcher_NilOnChange(t *testing.T) {
	_, err := NewWatcher(time.Second, nil, nil)
	require.Error(t, err, "NewWatcher(nil) should return error")
	require.ErrorIs(t, err, os.ErrInvalid)

	expectedMsg := "onChange callback is nil"
	assert.Equal(t, expectedMsg+": "+os.ErrInvalid.Error(), err.Error())
}

func newFSNotifyTestWatcher(t *testing.T, excludes []string) *Watcher {
	t.Helper()
	backend, err := newFSNotifyBackend(excludes)
	require.NoError(t, err)
	w, err := newWatcherWithBackend(
		time.Second,
		time.Second,
		func(context.Context, WatchBatch) error { return nil },
		backend,
		defaultWatchBatchMaxEntries,
		defaultWatchBatchMaxPathBytes,
	)
	require.NoError(t, err)
	w.Start()
	t.Cleanup(w.Stop)
	return w
}
