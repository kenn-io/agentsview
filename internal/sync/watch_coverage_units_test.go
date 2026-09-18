package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterRootsChargesSharedNativeWatchesOnce pins the budget accounting a
// shallow container unit plus a recursive sibling depends on: a directory an
// earlier root already watches natively costs nothing to share, so the shared
// budget must fund the union of the roots rather than the sum of their walks.
func TestRegisterRootsChargesSharedNativeWatchesOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	backend := testFSNotifyBackend(t)
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested")
	require.NoError(os.Mkdir(nested, 0o755))
	sibling := filepath.Join(parent, "sibling")
	require.NoError(os.Mkdir(sibling, 0o755))

	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000, WatcherOptions{},
	)
	require.NoError(err)

	// parent, nested, and sibling are three distinct directories, so three
	// native watches cover both roots even though the walks visit four.
	results := watcher.RegisterRoots([]WatchRoot{
		{Path: parent, Recursive: true, Exists: true},
		{Path: nested, Recursive: true, Exists: true},
	}, 3)
	require.Len(results, 2)

	assert.Equal(3, results[0].Allocated)
	assert.Equal(3, results[0].Watched)
	assert.Zero(results[1].Allocated,
		"the nested root reuses the watch the parent root installed")
	assert.Equal(1, results[1].Watched,
		"reuse still reports the directory as covered")
	assert.False(results[1].BudgetExhausted,
		"a root that installs nothing cannot exhaust the budget")
	assert.NoError(results[1].Err)
	assert.Contains(backend.watcher.WatchList(), nested)
}

// TestRegisterRootsReusesNativeWatchesAfterTheBudgetIsSpent is the starvation
// case: the shared budget is already gone when the second root registers, and
// every directory it needs is one the first root watches. Refusing it would
// report an uncovered root while the kernel is already delivering its events.
func TestRegisterRootsReusesNativeWatchesAfterTheBudgetIsSpent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	backend := testFSNotifyBackend(t)
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested")
	require.NoError(os.Mkdir(nested, 0o755))

	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000, WatcherOptions{},
	)
	require.NoError(err)

	results := watcher.RegisterRoots([]WatchRoot{
		{Path: parent, Recursive: true, Exists: true},
		{Path: nested, Recursive: true, Exists: true},
	}, 2)
	require.Len(results, 2)
	require.Zero(backend.runtimeBudget)

	assert.False(results[1].BudgetExhausted)
	assert.Zero(results[1].Unwatched)
	assert.Equal(1, results[1].Watched)
	assert.Contains(backend.watchOwners[nested], nested,
		"the reusing root must own the shared watch, or removing the first "+
			"root would drop coverage the second still needs")
}

// TestRegisterRootsAlwaysWatchesARecursiveRootItself covers the coverage a
// shallow unit loses when the plan merges it into a recursive unit at the same
// path. Two providers can name one directory, one shallowly and one
// recursively, and the daemon keeps a single root for it. A shallow watch is
// installed unconditionally, so refusing the merged root once the budget is
// spent would silently drop coverage the shallow unit guaranteed.
func TestRegisterRootsAlwaysWatchesARecursiveRootItself(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	backend := testFSNotifyBackend(t)
	first := t.TempDir()
	require.NoError(os.Mkdir(filepath.Join(first, "child"), 0o755))
	shared := t.TempDir()
	sharedChild := filepath.Join(shared, "child")
	require.NoError(os.Mkdir(sharedChild, 0o755))

	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000, WatcherOptions{},
	)
	require.NoError(err)

	// The first root consumes the whole budget, so the merged root registers
	// with nothing left to spend.
	results := watcher.RegisterRoots([]WatchRoot{
		{Path: first, Recursive: true, Exists: true},
		{Path: shared, Recursive: true, Exists: true},
	}, 2)
	require.Len(results, 2)
	require.Equal(2, results[0].Allocated,
		"the fixture must actually spend the budget, or the guard is vacuous")
	require.False(results[0].BudgetExhausted)
	require.Zero(backend.runtimeBudget)

	assert.Contains(backend.watcher.WatchList(), shared,
		"a recursive root must still watch its own directory")
	assert.True(results[1].BudgetExhausted,
		"the subtree below it stays discretionary and hands off to polling")
	assert.NotContains(backend.watcher.WatchList(), sharedChild)

	// The mandatory watch is off the books like a shallow root's, so removing
	// it must not hand back a slot the process never spent.
	assert.Zero(results[1].Allocated)
	require.NoError(backend.Remove(shared))
	assert.Zero(backend.runtimeBudget,
		"an uncharged watch cannot refund budget above the cap")
}
