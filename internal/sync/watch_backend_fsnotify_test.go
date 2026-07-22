package sync

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testFSNotifyBackend(t *testing.T) *fsnotifyBackend {
	t.Helper()
	backend, err := newFSNotifyBackend(nil)
	require.NoError(t, err)
	t.Cleanup(backend.Stop)
	return backend
}

func TestFSNotifyBackendOverflowRequestsLostEventRecovery(t *testing.T) {
	backend := testFSNotifyBackend(t)
	errorInput := make(chan error, 1)
	backend.errorInput = errorInput
	require.NoError(t, backend.Start())
	errorInput <- fsnotify.ErrEventOverflow

	pending := newPendingWatchBatch(8, 1_000)
	event := requireReceiveWithin(t, backend.Events(), time.Second)
	pending.AddBackendEvent(event)
	batch, ok := pending.Take()

	require.True(t, ok)
	assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true}, batch)
}

func TestFSNotifyBackendOrdinaryErrorRemainsAnError(t *testing.T) {
	backend := testFSNotifyBackend(t)
	errorInput := make(chan error, 1)
	backend.errorInput = errorInput
	require.NoError(t, backend.Start())
	sentinel := errors.New("ordinary fsnotify failure")
	errorInput <- sentinel

	select {
	case event := <-backend.Events():
		assert.Fail(t, "ordinary error emitted a watch event", "event: %+v", event)
	case err := <-backend.Errors():
		require.ErrorIs(t, err, sentinel)
	case <-time.After(time.Second):
		require.FailNow(t, "fsnotify backend did not emit ordinary error")
	}
	assert.Never(t, func() bool {
		select {
		case <-backend.Events():
			return true
		default:
			return false
		}
	}, 50*time.Millisecond, time.Millisecond,
		"ordinary errors must not request lost-event recovery")
}

func TestFSNotifyBackendRemoveShallowRootClearsOwnership(t *testing.T) {
	backend := testFSNotifyBackend(t)
	base := t.TempDir()
	removedRoot := filepath.Join(base, "shallow")
	require.NoError(t, os.Mkdir(removedRoot, 0o755))
	require.NoError(t, backend.AddShallow(removedRoot))
	require.NoError(t, backend.Remove(removedRoot))

	result := backend.AddRecursive(base, math.MaxInt)
	require.NoError(t, result.Err)
	newDir := filepath.Join(removedRoot, "new")
	require.NoError(t, os.Mkdir(newDir, 0o755))
	itemType, excluded := backend.watchCreatedPath(newDir)
	assert.Equal(t, backendItemDirectory, itemType)
	assert.False(t, excluded)
	assert.Contains(t, backend.watcher.WatchList(), newDir,
		"removed shallow ownership must not suppress recursive auto-watch")

	err := backend.Remove(removedRoot)
	require.ErrorIs(t, err, fsnotify.ErrNonExistentWatch)
}

func TestFSNotifyBackendRemoveRecursiveRootRemovesDescendantWatches(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := t.TempDir()
	descendant := filepath.Join(root, "child", "nested")
	require.NoError(t, os.MkdirAll(descendant, 0o755))
	result := backend.AddRecursive(root, math.MaxInt)
	require.NoError(t, result.Err)
	require.Equal(t, 3, result.Watched)

	require.NoError(t, backend.Remove(root))
	assert.Empty(t, backend.watcher.WatchList())
}

func TestFSNotifyBackendRemovePreservesOverlappingRecursiveRoot(t *testing.T) {
	backend := testFSNotifyBackend(t)
	parent := t.TempDir()
	sibling := filepath.Join(parent, "sibling")
	child := filepath.Join(parent, "child")
	nested := filepath.Join(child, "nested")
	for _, path := range []string{sibling, nested} {
		require.NoError(t, os.MkdirAll(path, 0o755))
	}
	require.NoError(t, backend.AddRecursive(parent, math.MaxInt).Err)
	require.NoError(t, backend.AddRecursive(child, math.MaxInt).Err)

	require.NoError(t, backend.Remove(parent))
	watched := backend.watcher.WatchList()
	slices.Sort(watched)
	want := []string{child, nested}
	slices.Sort(want)
	assert.Equal(t, want, watched)
}

func TestFSNotifyBackendRemoveDoesNotInheritExcludedParentOwnership(t *testing.T) {
	backend, err := newFSNotifyBackend([]string{"venv"})
	require.NoError(t, err)
	t.Cleanup(backend.Stop)
	parent := t.TempDir()
	nestedRoot := filepath.Join(parent, "venv", "project")
	nestedDir := filepath.Join(nestedRoot, "sessions")
	require.NoError(t, os.MkdirAll(nestedDir, 0o755))

	require.NoError(t, backend.AddRecursive(parent, math.MaxInt).Err)
	require.NoError(t, backend.AddRecursive(nestedRoot, math.MaxInt).Err)
	require.NoError(t, backend.Remove(nestedRoot))
	assert.Equal(t, []string{parent}, backend.watcher.WatchList(),
		"excluded parent root must not own explicit nested-root watches")

	require.NoError(t, backend.Start())
	nestedFile := filepath.Join(nestedDir, "session.jsonl")
	require.NoError(t, os.WriteFile(nestedFile, []byte("x"), 0o644))
	assertBackendPathNotEmitted(t, backend.Events(), nestedFile, 100*time.Millisecond)
}

func TestFSNotifyBackendRemoveDoesNotInheritBudgetSkippedParentOwnership(t *testing.T) {
	backend := testFSNotifyBackend(t)
	parent := t.TempDir()
	nestedRoot := filepath.Join(parent, "nested")
	nestedDir := filepath.Join(nestedRoot, "sessions")
	require.NoError(t, os.MkdirAll(nestedDir, 0o755))

	result := backend.AddRecursive(parent, 1)
	require.NoError(t, result.Err)
	require.True(t, result.BudgetExhausted)
	require.NoError(t, backend.AddRecursive(nestedRoot, math.MaxInt).Err)
	require.NoError(t, backend.Remove(nestedRoot))
	assert.Equal(t, []string{parent}, backend.watcher.WatchList(),
		"budget-skipped parent root must not own explicit nested-root watches")
}

type blockingRemoveWatchOps struct {
	watcher       *fsnotify.Watcher
	removeStarted chan struct{}
	allowRemove   chan struct{}
	addCalled     chan struct{}
	removeOnce    sync.Once
	addOnce       sync.Once
}

type failPathWatchOps struct {
	watcher  *fsnotify.Watcher
	failPath string
	err      error
}

func (w *failPathWatchOps) Add(path string) error {
	if filepath.Clean(path) == filepath.Clean(w.failPath) {
		return w.err
	}
	return w.watcher.Add(path)
}

func (w *failPathWatchOps) Remove(path string) error {
	return w.watcher.Remove(path)
}

func TestFSNotifyBackendRuntimeBudgetDegradesExactScopesToPolling(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := t.TempDir()
	syncDir := filepath.Join(root, "logical-sessions")
	polling := make(chan PollingObligation, 1)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{OnPollingRequired: func(obligation PollingObligation) error {
			polling <- obligation
			return nil
		}},
	)
	require.NoError(t, err)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "claude", SyncDir: syncDir}},
	}}, 1)
	require.Len(t, results, 1)
	require.Equal(t, 1, results[0].Watched)
	require.NoError(t, backend.Start())

	created := filepath.Join(root, "created-after-startup")
	require.NoError(t, os.Mkdir(created, 0o755))
	obligation := requireReceiveWithin(t, polling, time.Second)
	assert.NotEmpty(t, obligation.Key)
	assert.Equal(t, []string{syncDir}, obligation.Roots)
	assert.NotContains(t, backend.watcher.WatchList(), created)
}

func TestFSNotifyBackendRuntimeDirectoryChurnReclaimsWatchBudget(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := t.TempDir()
	result := backend.AddRecursive(root, 2)
	require.NoError(t, result.Err)
	require.Equal(t, 1, result.Watched)
	require.NoError(t, backend.Start())

	created := filepath.Join(root, "recreated")
	for iteration := range 2 {
		require.NoError(t, os.Mkdir(created, 0o755))
		waitForBackendEvent(t, backend.Events(), created, backendOpCreate)
		assert.Contains(t, backend.watcher.WatchList(), created)

		require.NoError(t, os.Remove(created))
		waitForBackendEvent(t, backend.Events(), created, backendOpRemove|backendOpRename)
		assert.NotContains(t, backend.watcher.WatchList(), created)
		backend.watchMu.Lock()
		_, retained := backend.watchOwners[created]
		budget := backend.runtimeBudget
		backend.watchMu.Unlock()
		assert.False(t, retained, "removed directory ownership must be pruned")
		assert.Equal(t, 1, budget,
			"removed native watch must return its runtime budget slot")
		if iteration == 1 {
			break
		}
	}
}

func TestFSNotifyBackendRootLossTransfersExactScopeToPolling(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := t.TempDir()
	child := filepath.Join(root, "ordinary-child")
	require.NoError(t, os.Mkdir(child, 0o755))
	syncDir := filepath.Join(root, "logical-sessions")
	polling := make(chan PollingObligation, 1)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{OnPollingRequired: func(obligation PollingObligation) error {
			polling <- obligation
			return nil
		}},
	)
	require.NoError(t, err)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "claude", SyncDir: syncDir}},
	}}, 4)
	require.Len(t, results, 1)
	require.Equal(t, 2, results[0].Watched)

	childEvent, relevant := backend.translateEvent(fsnotify.Event{
		Name: child, Op: fsnotify.Remove,
	})
	require.True(t, relevant)
	assert.Equal(t, backendItemDirectory, childEvent.ItemType)
	select {
	case obligation := <-polling:
		assert.Fail(t, "ordinary descendant loss must not degrade its configured root",
			"unexpected obligation: %+v", obligation)
	default:
	}

	event, relevant := backend.translateEvent(fsnotify.Event{
		Name: root, Op: fsnotify.Remove,
	})
	require.True(t, relevant)
	assert.Equal(t, backendItemDirectory, event.ItemType)
	obligation := requireReceiveWithin(t, polling, time.Second)
	assert.Equal(t, "fsnotify-runtime:"+root, obligation.Key)
	assert.Equal(t, []string{syncDir}, obligation.Roots)
	assert.Empty(t, backend.watcher.WatchList())
}

func waitForBackendEvent(
	t *testing.T,
	events <-chan backendEvent,
	path string,
	wantOp backendOp,
) backendEvent {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if filepath.Clean(event.Path) == filepath.Clean(path) && event.Op&wantOp != 0 {
				return event
			}
		case <-deadline.C:
			t.Fatalf("backend event %v for %s was not observed", wantOp, path)
		}
	}
}

func TestFSNotifyBackendRuntimeAddFailureDegradesExactScopesToPolling(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := t.TempDir()
	syncDir := filepath.Join(root, "logical-sessions")
	polling := make(chan PollingObligation, 1)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{OnPollingRequired: func(obligation PollingObligation) error {
			polling <- obligation
			return nil
		}},
	)
	require.NoError(t, err)
	results := watcher.RegisterRoots([]WatchRoot{{
		Path: root, Recursive: true, Exists: true,
		Scopes: []WatchScope{{Agent: "claude", SyncDir: syncDir}},
	}}, 4)
	require.Len(t, results, 1)
	require.Equal(t, 1, results[0].Watched)

	created := filepath.Join(root, "unwatchable")
	require.NoError(t, os.Mkdir(created, 0o755))
	backend.watchOps = &failPathWatchOps{
		watcher: backend.watcher, failPath: created, err: syscall.ENOSPC,
	}
	itemType, excluded := backend.watchCreatedPath(created)
	assert.Equal(t, backendItemDirectory, itemType)
	assert.False(t, excluded)
	obligation := requireReceiveWithin(t, polling, time.Second)
	assert.Equal(t, []string{syncDir}, obligation.Roots)
	assert.NotContains(t, backend.watcher.WatchList(), created)
}

func TestFSNotifyBackendRuntimeDegradationPreservesOverlappingRootScopes(t *testing.T) {
	backend := testFSNotifyBackend(t)
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested")
	require.NoError(t, os.Mkdir(nested, 0o755))
	parentScope := filepath.Join(parent, "parent-sessions")
	nestedScope := filepath.Join(parent, "nested-sessions")
	polling := make(chan PollingObligation, 2)
	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{OnPollingRequired: func(obligation PollingObligation) error {
			polling <- obligation
			return nil
		}},
	)
	require.NoError(t, err)
	results := watcher.RegisterRoots([]WatchRoot{
		{
			Path: parent, Recursive: true, Exists: true,
			Scopes: []WatchScope{{Agent: "claude", SyncDir: parentScope}},
		},
		{
			Path: nested, Recursive: true, Exists: true,
			Scopes: []WatchScope{{Agent: "cursor", SyncDir: nestedScope}},
		},
	}, 3)
	require.Len(t, results, 2)
	require.Zero(t, backend.runtimeBudget)

	created := filepath.Join(nested, "created-after-startup")
	require.NoError(t, os.Mkdir(created, 0o755))
	itemType, excluded := backend.watchCreatedPath(created)
	assert.Equal(t, backendItemDirectory, itemType)
	assert.False(t, excluded)
	first := requireReceiveWithin(t, polling, time.Second)
	second := requireReceiveWithin(t, polling, time.Second)
	obligations := map[string][]string{
		first.Key:  first.Roots,
		second.Key: second.Roots,
	}
	assert.Equal(t, []string{parentScope}, obligations["fsnotify-runtime:"+parent])
	assert.Equal(t, []string{nestedScope}, obligations["fsnotify-runtime:"+nested])
}

func TestFSNotifyBackendRuntimeCreateRecursivelyWatchesMovedSubtree(t *testing.T) {
	backend := testFSNotifyBackend(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "watched")
	source := filepath.Join(parent, "incoming")
	deepSource := filepath.Join(source, "nested")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(deepSource, 0o755))
	result := backend.AddRecursive(root, 8)
	require.NoError(t, result.Err)
	require.Equal(t, 1, result.Watched)

	moved := filepath.Join(root, "moved")
	require.NoError(t, os.Rename(source, moved))
	itemType, excluded := backend.watchCreatedPath(moved)
	assert.Equal(t, backendItemDirectory, itemType)
	assert.False(t, excluded)
	require.NoError(t, backend.Start())

	deepFile := filepath.Join(moved, "nested", "session.jsonl")
	require.NoError(t, os.WriteFile(deepFile, []byte("changed"), 0o644))
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-backend.Events():
			if filepath.Clean(event.Path) == filepath.Clean(deepFile) {
				assert.NotEqual(t, backendOpUnknown, event.Op)
				return
			}
		case <-deadline.C:
			t.Fatal("deep write under moved subtree was not observed")
		}
	}
}

func (w *blockingRemoveWatchOps) Add(path string) error {
	w.addOnce.Do(func() { close(w.addCalled) })
	return w.watcher.Add(path)
}

func (w *blockingRemoveWatchOps) Remove(path string) error {
	w.removeOnce.Do(func() { close(w.removeStarted) })
	<-w.allowRemove
	return w.watcher.Remove(path)
}

func TestFSNotifyBackendConcurrentAddWaitsForRemoveOwnershipDecision(t *testing.T) {
	backend := testFSNotifyBackend(t)
	root := t.TempDir()
	require.NoError(t, backend.AddShallow(root))
	barrier := &blockingRemoveWatchOps{
		watcher:       backend.watcher,
		removeStarted: make(chan struct{}),
		allowRemove:   make(chan struct{}),
		addCalled:     make(chan struct{}),
	}
	backend.watchOps = barrier

	removeErr := make(chan error, 1)
	go func() { removeErr <- backend.Remove(root) }()
	select {
	case <-barrier.removeStarted:
	case <-time.After(time.Second):
		t.Fatal("Remove did not reach native-watch barrier")
	}

	addErr := make(chan error, 1)
	go func() { addErr <- backend.AddShallow(root) }()
	addReachedWhileRemoveBlocked := false
	select {
	case <-barrier.addCalled:
		addReachedWhileRemoveBlocked = true
	case <-time.After(50 * time.Millisecond):
	}
	close(barrier.allowRemove)
	require.NoError(t, <-removeErr)
	require.NoError(t, <-addErr)
	assert.False(t, addReachedWhileRemoveBlocked,
		"native Add must wait for Remove's ownership decision")
	assert.Contains(t, backend.watcher.WatchList(), root,
		"the newly retained logical root must keep its native watch")
}

func assertBackendPathNotEmitted(
	t *testing.T, events <-chan backendEvent, path string, duration time.Duration,
) {
	t.Helper()
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if !assert.NotEqual(t, path, event.Path) {
				return
			}
		case <-deadline.C:
			return
		}
	}
}

func TestFSNotifyBackendLifecycleStopBeforeStartReturns(t *testing.T) {
	backend := testFSNotifyBackend(t)
	stopped := make(chan struct{})
	go func() {
		backend.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("fsnotify backend Stop blocked before Start")
	}
	require.NoError(t, backend.Start())
	backend.Stop()
}

func TestFSNotifyBackendLifecycleRepeatedStartAndStop(t *testing.T) {
	backend := testFSNotifyBackend(t)
	require.NoError(t, backend.Start())
	require.NoError(t, backend.Start())
	backend.Stop()
	backend.Stop()
}

func TestFSNotifyBackendLifecycleStartStopRace(t *testing.T) {
	for range 100 {
		backend := testFSNotifyBackend(t)
		start := make(chan struct{})
		startErr := make(chan error, 1)
		var calls sync.WaitGroup
		calls.Add(2)
		go func() {
			defer calls.Done()
			<-start
			startErr <- backend.Start()
		}()
		go func() {
			defer calls.Done()
			<-start
			backend.Stop()
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
			t.Fatal("fsnotify backend concurrent Start and Stop deadlocked")
		}
		require.NoError(t, <-startErr)
		backend.Stop()
	}
}

// alwaysAddWatchOps accepts every watch so a test can isolate traversal
// failures from watch-registration failures.
type alwaysAddWatchOps struct{}

func (alwaysAddWatchOps) Add(string) error    { return nil }
func (alwaysAddWatchOps) Remove(string) error { return nil }

// An unreadable directory leaves its descendants without native watches even
// though its own watch registered. Startup traversal must report that as
// degraded coverage so the owning logical root gains a polling obligation
// instead of appearing fully watched.
func TestFSNotifyBackendAddRecursiveCountsUnreadableSubtreeAsUnwatched(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("directory read permissions are not enforced on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	backend := testFSNotifyBackend(t)
	backend.watchOps = alwaysAddWatchOps{}
	root := t.TempDir()
	unreadable := filepath.Join(root, "unreadable")
	require.NoError(t, os.MkdirAll(filepath.Join(unreadable, "hidden"), 0o755))
	require.NoError(t, os.Chmod(unreadable, 0o000))
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	result := backend.AddRecursive(root, 100)

	require.NoError(t, result.Err)
	assert.Positive(t, result.Unwatched,
		"an unenumerable subtree must count as degraded coverage")
}
