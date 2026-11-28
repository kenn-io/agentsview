//go:build !darwin

package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
