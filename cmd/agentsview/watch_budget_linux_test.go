//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestRecursiveWatchBudgetWatchesRootsPastEightThousandFolders(t *testing.T) {
	limitBytes, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	require.NoError(t, err)
	limit, err := strconv.Atoi(strings.TrimSpace(string(limitBytes)))
	require.NoError(t, err)
	if limit < 32768 {
		t.Skipf("inotify max_user_watches=%d is below the 32768 needed for this large-tree test", limit)
	}

	dir := t.TempDir()
	rootA := filepath.Join(dir, "root-a")
	rootB := filepath.Join(dir, "root-b")
	for i := range 8200 {
		require.NoError(t, os.MkdirAll(filepath.Join(rootA, fmt.Sprintf("folder-%d", i)), 0o700))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(rootB, "folder"), 0o700))

	w, err := syncpkg.NewWatcherWithCallback(watcherBatchDelay, watcherSyncMinInterval,
		func(context.Context, syncpkg.WatchBatch) error { return nil }, nil, syncpkg.WatcherOptions{})
	require.NoError(t, err)
	t.Cleanup(w.Stop)

	results := w.RegisterRoots([]syncpkg.WatchRoot{
		{Path: rootA, Recursive: true, Exists: true},
		{Path: rootB, Recursive: true, Exists: true},
	}, recursiveWatchBudget)
	require.Len(t, results, 2)
	for _, result := range results {
		require.NoError(t, result.Err)
		require.False(t, result.BudgetExhausted)
		require.False(t, result.ResourceExhausted)
	}
	assert.Equal(t, 8201, results[0].Watched)
	assert.Equal(t, 2, results[1].Watched)
	assert.Equal(t, 2, results[1].Allocated)
}
