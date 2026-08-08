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
	backend := testFSNotifyBackend(t)
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested")
	require.NoError(t, os.Mkdir(nested, 0o755))
	sibling := filepath.Join(parent, "sibling")
	require.NoError(t, os.Mkdir(sibling, 0o755))

	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000, WatcherOptions{},
	)
	require.NoError(t, err)

	// parent, nested, and sibling are three distinct directories, so three
	// native watches cover both roots even though the walks visit four.
	results := watcher.RegisterRoots([]WatchRoot{
		{Path: parent, Recursive: true, Exists: true},
		{Path: nested, Recursive: true, Exists: true},
	}, 3)
	require.Len(t, results, 2)

	assert.Equal(t, 3, results[0].Allocated)
	assert.Equal(t, 3, results[0].Watched)
	assert.Zero(t, results[1].Allocated,
		"the nested root reuses the watch the parent root installed")
	assert.Equal(t, 1, results[1].Watched,
		"reuse still reports the directory as covered")
	assert.False(t, results[1].BudgetExhausted,
		"a root that installs nothing cannot exhaust the budget")
	assert.NoError(t, results[1].Err)
	assert.Contains(t, backend.watcher.WatchList(), nested)
}

// TestRegisterRootsReusesNativeWatchesAfterTheBudgetIsSpent is the starvation
// case: the shared budget is already gone when the second root registers, and
// every directory it needs is one the first root watches. Refusing it would
// report an uncovered root while the kernel is already delivering its events.
func TestRegisterRootsReusesNativeWatchesAfterTheBudgetIsSpent(t *testing.T) {
	backend := testFSNotifyBackend(t)
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested")
	require.NoError(t, os.Mkdir(nested, 0o755))

	watcher, err := newWatcherWithBackendOptions(
		0, 0, func(context.Context, WatchBatch) error { return nil },
		backend, 8, 1_000, WatcherOptions{},
	)
	require.NoError(t, err)

	results := watcher.RegisterRoots([]WatchRoot{
		{Path: parent, Recursive: true, Exists: true},
		{Path: nested, Recursive: true, Exists: true},
	}, 2)
	require.Len(t, results, 2)
	require.Zero(t, backend.runtimeBudget)

	assert.False(t, results[1].BudgetExhausted)
	assert.Zero(t, results[1].Unwatched)
	assert.Equal(t, 1, results[1].Watched)
	assert.Contains(t, backend.watchOwners[nested], nested,
		"the reusing root must own the shared watch, or removing the first "+
			"root would drop coverage the second still needs")
}
