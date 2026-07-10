package sync

import (
	"fmt"
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

type watcherCall struct {
	paths []string
	at    time.Time
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

func startTestWatcherWithBatchLimitsNoCleanup(
	t *testing.T,
	onChange func(WatchBatch),
	batchDelay, minInterval time.Duration,
	maxEntries, maxPathBytes int,
) (*Watcher, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := newWatcherWithLimits(
		batchDelay,
		minInterval,
		onChange,
		nil,
		maxEntries,
		maxPathBytes,
	)
	require.NoError(t, err, "newWatcherWithLimits")
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

func receivePaths(t *testing.T, calls <-chan []string) []string {
	t.Helper()
	select {
	case paths := <-calls:
		return paths
	case <-time.After(watcherTestTimeout):
		t.Fatal("timed out waiting for watcher callback")
		return nil
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

// pollUntil dynamically polls a condition to avoid hardcoded sleeps.
func pollUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pollUntil: condition not met within deadline")
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

func TestWatcherContinuesIntakeDuringCallback(t *testing.T) {
	const (
		batchDelay  = 30 * time.Millisecond
		minInterval = 120 * time.Millisecond
	)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
	calls := make(chan []string, 4)
	var callCount atomic.Int32
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32

	w, dir := startTestWatcherWithIntervals(t, func(paths []string) {
		current := concurrent.Add(1)
		updateMax(&maxConcurrent, current)
		defer concurrent.Add(-1)

		if callCount.Add(1) == 1 {
			close(started)
			<-release
		}
		calls <- paths
	}, batchDelay, minInterval)
	t.Cleanup(releaseCallback)

	firstPath := filepath.Join(dir, "first.jsonl")
	require.NoError(t, os.WriteFile(firstPath, []byte("first"), 0o644))
	select {
	case <-started:
	case <-time.After(watcherTestTimeout):
		t.Fatal("timed out waiting for the first callback to start")
	}

	duringCallbackPath := filepath.Join(dir, "during-callback")
	require.NoError(t, os.Mkdir(duringCallbackPath, 0o755))
	require.Eventually(t, func() bool {
		return slices.Contains(w.watcher.WatchList(), duringCallbackPath)
	}, time.Second, 10*time.Millisecond,
		"watcher did not drain the directory event while callback was blocked")
	assert.Never(t, func() bool {
		return callCount.Load() > 1
	}, minInterval+batchDelay, 5*time.Millisecond,
		"a second callback started while the first callback was blocked")
	releaseCallback()

	firstBatch := receivePaths(t, calls)
	secondBatch := receivePaths(t, calls)
	assert.Contains(t, firstBatch, firstPath)
	assert.Contains(t, secondBatch, duringCallbackPath)
	assert.Equal(t, int32(1), maxConcurrent.Load(),
		"watcher callbacks must remain serialized")
}

func TestWatcherOverflowRunsFullSyncThenRetainsLaterBatch(t *testing.T) {
	const (
		batchDelay  = 20 * time.Millisecond
		minInterval = 80 * time.Millisecond
		maxEntries  = 2
	)
	firstRelease := make(chan struct{})
	fullSyncRelease := make(chan struct{})
	var releaseFirstOnce sync.Once
	var releaseFullSyncOnce sync.Once
	releaseFirst := func() { releaseFirstOnce.Do(func() { close(firstRelease) }) }
	releaseFullSync := func() {
		releaseFullSyncOnce.Do(func() { close(fullSyncRelease) })
	}

	calls := make(chan WatchBatch, 4)
	var callCount atomic.Int32
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	w, dir := startTestWatcherWithBatchLimitsNoCleanup(
		t,
		func(batch WatchBatch) {
			current := concurrent.Add(1)
			updateMax(&maxConcurrent, current)
			defer concurrent.Add(-1)

			call := callCount.Add(1)
			calls <- batch
			switch call {
			case 1:
				<-firstRelease
			case 2:
				<-fullSyncRelease
			}
		},
		batchDelay,
		minInterval,
		maxEntries,
		1_000_000,
	)
	t.Cleanup(func() {
		releaseFirst()
		releaseFullSync()
		w.Stop()
	})

	firstPath := filepath.Join(dir, "first")
	require.NoError(t, os.Mkdir(firstPath, 0o755))
	firstBatch := receiveWatchBatch(t, calls)
	assert.False(t, firstBatch.FullSync)
	assert.Equal(t, []string{firstPath}, firstBatch.Paths)

	var overflowPaths []string
	for _, name := range []string{"overflow-a", "overflow-b", "overflow-c"} {
		path := filepath.Join(dir, name)
		overflowPaths = append(overflowPaths, path)
		require.NoError(t, os.Mkdir(path, 0o755))
	}
	require.Eventually(t, func() bool {
		return slices.Contains(w.watcher.WatchList(), overflowPaths[2])
	}, time.Second, 10*time.Millisecond,
		"watcher did not drain the overflowing event stream")
	assert.Equal(t, int32(1), callCount.Load(),
		"a callback started while the first callback was blocked")

	releaseFirst()
	overflowBatch := receiveWatchBatch(t, calls)
	assert.True(t, overflowBatch.FullSync)
	assert.Empty(t, overflowBatch.Paths)

	laterPath := filepath.Join(dir, "after-overflow-dispatch")
	require.NoError(t, os.Mkdir(laterPath, 0o755))
	require.Eventually(t, func() bool {
		return slices.Contains(w.watcher.WatchList(), laterPath)
	}, time.Second, 10*time.Millisecond,
		"watcher did not drain an event while full sync was blocked")
	assert.Equal(t, int32(2), callCount.Load(),
		"a callback started while full sync was blocked")

	releaseFullSync()
	laterBatch := receiveWatchBatch(t, calls)
	assert.False(t, laterBatch.FullSync)
	assert.Equal(t, []string{laterPath}, laterBatch.Paths)
	assert.Equal(t, int32(1), maxConcurrent.Load(),
		"watcher callbacks must remain serialized")
}

func TestWatcherStopCancelsPendingCallback(t *testing.T) {
	const batchDelay = 300 * time.Millisecond
	calls := make(chan []string, 1)
	w, dir := startTestWatcherWithIntervalsNoCleanup(
		t, func(paths []string) { calls <- paths }, batchDelay, time.Second,
	)
	t.Cleanup(w.Stop)

	pendingPath := filepath.Join(dir, "pending")
	require.NoError(t, os.Mkdir(pendingPath, 0o755))
	require.Eventually(t, func() bool {
		return slices.Contains(w.watcher.WatchList(), pendingPath)
	}, time.Second, 10*time.Millisecond,
		"watcher did not process the pending directory event")
	pendingNestedPath := filepath.Join(pendingPath, "nested")
	require.NoError(t, os.Mkdir(pendingNestedPath, 0o755))
	require.Eventually(t, func() bool {
		return slices.Contains(w.watcher.WatchList(), pendingNestedPath)
	}, time.Second, 10*time.Millisecond,
		"watcher did not finish processing the pending directory event")

	w.Stop()
	select {
	case paths := <-calls:
		t.Fatalf("callback ran after Stop with paths %v", paths)
	case <-time.After(batchDelay + 50*time.Millisecond):
	}
}

func TestWatcherStopWaitsForRunningCallbackAndDiscardsPending(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int32

	w, dir := startTestWatcherWithIntervalsNoCleanup(t, func([]string) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
	}, 30*time.Millisecond, 100*time.Millisecond)
	t.Cleanup(func() {
		releaseCallback()
		w.Stop()
	})

	require.NoError(t,
		os.WriteFile(filepath.Join(dir, "running.jsonl"), []byte("running"), 0o644))
	select {
	case <-started:
	case <-time.After(watcherTestTimeout):
		t.Fatal("timed out waiting for the first callback to start")
	}

	queuedPath := filepath.Join(dir, "queued")
	require.NoError(t, os.Mkdir(queuedPath, 0o755))
	require.Eventually(t, func() bool {
		return slices.Contains(w.watcher.WatchList(), queuedPath)
	}, time.Second, 10*time.Millisecond,
		"watcher did not retain the second event while callback was blocked")
	queuedNestedPath := filepath.Join(queuedPath, "nested")
	require.NoError(t, os.Mkdir(queuedNestedPath, 0o755))
	require.Eventually(t, func() bool {
		return slices.Contains(w.watcher.WatchList(), queuedNestedPath)
	}, time.Second, 10*time.Millisecond,
		"watcher did not finish retaining the second event")

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

	w, dir := startTestWatcher(t, func(paths []string) {
		pathsCh <- paths
	})

	subdir := filepath.Join(dir, "newdir")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	// Wait for fsnotify to process the mkdir and add the watch
	pollUntil(t, func() bool {
		return slices.Contains(w.watcher.WatchList(), subdir)
	})

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
	w, err := NewWatcher(time.Second, func(WatchBatch) {}, []string{".git", "node_modules"})
	require.NoError(t, err, "NewWatcher")
	w.Start()
	t.Cleanup(func() { w.Stop() })

	root := t.TempDir()
	included := filepath.Join(root, "project", "src")
	excludedGit := filepath.Join(root, "project", ".git", "objects")
	excludedModules := filepath.Join(root, "project", "node_modules", "pkg")
	for _, p := range []string{included, excludedGit, excludedModules} {
		require.NoError(t, os.MkdirAll(p, 0o755), "MkdirAll(%s)", p)
	}

	_, _, err = w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")

	got := w.watcher.WatchList()
	assert.NotContains(t, got, filepath.Join(root, "project", ".git"),
		".git should be excluded from watch list")
	assert.NotContains(t, got, filepath.Join(root, "project", "node_modules"),
		"node_modules should be excluded from watch list")
	assert.Contains(t, got, included, "expected included dir in watch list")
}

func TestWatchRecursiveBudget_DegradesWhenBudgetExhausted(t *testing.T) {
	root := t.TempDir()
	for i := range 5 {
		require.NoError(t, os.MkdirAll(filepath.Join(root, fmt.Sprintf("dir-%d", i)), 0o755))
	}

	w, err := NewWatcher(time.Second, func(WatchBatch) {}, nil)
	require.NoError(t, err, "NewWatcher")
	w.Start()
	t.Cleanup(func() { w.Stop() })

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

	time.Sleep(100 * time.Millisecond)
	assert.NotContains(t, w.watcher.WatchList(), gitDir,
		"newly created excluded dir should not be watched")

	fileInGit := filepath.Join(gitDir, "config")
	require.NoError(t, os.WriteFile(fileInGit, []byte("x"), 0o644))

	select {
	case paths := <-pathsCh:
		assert.NotContains(t, paths, fileInGit,
			"changes inside excluded dir should not trigger onChange")
	case <-time.After(200 * time.Millisecond):
		// no events from excluded dir; expected
	}
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
	assert.NotContains(t, w.watcher.WatchList(), localDir,
		"shallow root descendant should not be auto-watched")
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
	pollUntil(t, func() bool {
		return slices.Contains(w.watcher.WatchList(), dateDir)
	})

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
	assert.NotContains(t, w.watcher.WatchList(), logDir,
		"sibling dir under the shallow parent must not be auto-watched")
}

func TestWatchRecursive_RootUnderExcludedAncestorStillWatchesDescendants(t *testing.T) {
	w, err := NewWatcher(time.Second, func(WatchBatch) {}, []string{"venv"})
	require.NoError(t, err, "NewWatcher")
	w.Start()
	t.Cleanup(func() { w.Stop() })

	base := t.TempDir()
	root := filepath.Join(base, "venv", "project")
	included := filepath.Join(root, "src")
	require.NoError(t, os.MkdirAll(included, 0o755), "MkdirAll(%s)", included)

	_, _, err = w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")

	got := w.watcher.WatchList()
	assert.Contains(t, got, root, "expected root in watch list")
	assert.Contains(t, got, included, "expected included dir in watch list")
}

func TestWatchRecursive_ExcludesSlashPatternRelativeToRoot(t *testing.T) {
	w, err := NewWatcher(time.Second, func(WatchBatch) {}, []string{"foo/bar"})
	require.NoError(t, err, "NewWatcher")
	w.Start()
	t.Cleanup(func() { w.Stop() })

	root := t.TempDir()
	excluded := filepath.Join(root, "foo", "bar")
	includedSibling := filepath.Join(root, "foo", "baz")
	for _, p := range []string{excluded, includedSibling} {
		require.NoError(t, os.MkdirAll(p, 0o755), "MkdirAll(%s)", p)
	}

	_, _, err = w.WatchRecursive(root)
	require.NoError(t, err, "WatchRecursive")

	got := w.watcher.WatchList()
	assert.NotContains(t, got, excluded, "expected %s to be excluded", excluded)
	assert.Contains(t, got, includedSibling, "expected %s to be included", includedSibling)
}

func TestWatchRecursive_OverlappingRoots_UsesMostSpecificRoot(t *testing.T) {
	w, err := NewWatcher(time.Second, func(WatchBatch) {}, []string{"venv"})
	require.NoError(t, err, "NewWatcher")
	w.Start()
	t.Cleanup(func() { w.Stop() })

	base := t.TempDir()
	parentRoot := filepath.Join(base, "workspace")
	nestedRoot := filepath.Join(parentRoot, "venv", "project")
	included := filepath.Join(nestedRoot, "src")
	for _, p := range []string{parentRoot, included} {
		require.NoError(t, os.MkdirAll(p, 0o755), "MkdirAll(%s)", p)
	}

	_, _, err = w.WatchRecursive(parentRoot)
	require.NoError(t, err, "WatchRecursive(parent)")
	_, _, err = w.WatchRecursive(nestedRoot)
	require.NoError(t, err, "WatchRecursive(nested)")

	got := w.watcher.WatchList()
	assert.Contains(t, got, nestedRoot, "expected nested root in watch list")
	assert.Contains(t, got, included, "expected included dir in watch list")
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
