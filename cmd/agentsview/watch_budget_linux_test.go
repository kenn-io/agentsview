//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestRecursiveWatchBudgetWatchesRootsPastEightThousandFolders(t *testing.T) {
	root := t.TempDir()
	for i := range 8200 {
		require.NoError(t, os.MkdirAll(filepath.Join(root, fmt.Sprintf("folder-%d", i)), 0o700))
	}

	w, err := syncpkg.NewWatcherWithCallback(watcherBatchDelay, watcherSyncMinInterval,
		func(context.Context, syncpkg.WatchBatch) error { return nil }, nil, syncpkg.WatcherOptions{})
	require.NoError(t, err)
	t.Cleanup(w.Stop)

	results := w.RegisterRoots([]syncpkg.WatchRoot{
		{Path: root, Recursive: true, Exists: true},
	}, recursiveWatchBudget)
	require.Len(t, results, 1)
	assert.False(t, results[0].BudgetExhausted)
}
