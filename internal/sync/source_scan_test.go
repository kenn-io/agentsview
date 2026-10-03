package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSourceScanAcknowledgesOnlySuccessfulWork(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("one\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("ignored"), 0o600))
	scanner := newSourceScanner([]WatchRoot{{Path: root, Recursive: true, SourceFileGlobs: []string{"*.jsonl"}}}, nil)
	var batches []WatchBatch
	emit := func(_ context.Context, batch WatchBatch) error { batches = append(batches, batch); return nil }
	require.NoError(t, scanner.scan(t.Context(), emit))
	require.Len(t, batches, 1)
	assert.Equal(t, []string{root}, batches[0].ReconcileRoots)
	batches = nil
	require.NoError(t, scanner.scan(t.Context(), emit))
	assert.Empty(t, batches, "unchanged sources need no archive work")
	require.NoError(t, os.WriteFile(path, []byte("two more\n"), 0o600))
	failure := errors.New("archive unavailable")
	require.ErrorIs(t, scanner.scan(t.Context(), func(_ context.Context, batch WatchBatch) error {
		assert.Equal(t, []string{path}, batch.Paths)
		return failure
	}), failure)
	require.NoError(t, scanner.scan(t.Context(), emit))
	require.Len(t, batches, 1)
	assert.Equal(t, []string{path}, batches[0].Paths, "failed observations must be retried")
	batches = nil
	require.NoError(t, scanner.scan(t.Context(), emit))
	assert.Empty(t, batches)
}

func TestSourceScanDefersMissingRootAndReconcilesDisappearance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	scanner := newSourceScanner([]WatchRoot{{Path: root, Recursive: true, SourceFileGlobs: []string{"*.jsonl"}}}, nil)
	var batches []WatchBatch
	emit := func(_ context.Context, batch WatchBatch) error { batches = append(batches, batch); return nil }
	require.NoError(t, scanner.scan(t.Context(), emit))
	assert.Empty(t, batches)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "nested"), 0o700))
	path := filepath.Join(root, "nested", "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("one"), 0o600))
	require.NoError(t, scanner.scan(t.Context(), emit))
	require.Len(t, batches, 1)
	batches = nil
	require.NoError(t, os.Rename(root, root+"-away"))
	require.NoError(t, scanner.scan(t.Context(), emit))
	assert.Empty(t, batches, "a missing root is not deletion evidence")
	require.NoError(t, os.Rename(root+"-away", root))
	require.NoError(t, os.Remove(path))
	require.NoError(t, scanner.scan(t.Context(), emit))
	require.Len(t, batches, 1)
	assert.Equal(t, []string{root}, batches[0].ReconcileRoots)
}

func TestSourceScanCacheCapacityDoesNotHideChanges(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one.jsonl", "two.jsonl"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("one"), 0o600))
	}
	scanner := newSourceScanner([]WatchRoot{{Path: root, SourceFileGlobs: []string{"*.jsonl"}}}, nil)
	scanner.maxEntries = 1
	emit := func(context.Context, WatchBatch) error { return nil }
	require.NoError(t, scanner.scan(t.Context(), emit))
	var paths []string
	require.NoError(t, scanner.scan(t.Context(), func(_ context.Context, batch WatchBatch) error {
		paths = append(paths, batch.Paths...)
		return nil
	}))
	assert.Len(t, paths, 1, "uncached sources still reach archive freshness checks")
	assert.Equal(t, uint64(1), scanner.stats().CachedFiles)
	assert.True(t, scanner.stats().Saturated)
}

func TestWatcherSourceScanRetryAndCancellation(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "session.jsonl"), []byte("one"), 0o600))
	calls := make(chan WatchBatch, 4)
	attempts := 0
	watcher, err := newWatcherWithBackend(0, 0, func(_ context.Context, batch WatchBatch) error {
		calls <- batch
		attempts++
		if attempts == 1 {
			return errors.New("temporary archive failure")
		}
		return nil
	}, newFakeWatchBackend(), 8192, 2<<20)
	require.NoError(t, err)
	watcher.RegisterRoots([]WatchRoot{{Path: root, Exists: true, SourceFileGlobs: []string{"*.jsonl"}}}, 0)
	t.Cleanup(watcher.Stop)
	require.NoError(t, watcher.Start())
	for range 2 {
		select {
		case batch := <-calls:
			assert.Equal(t, []string{root}, batch.ReconcileRoots)
		case <-t.Context().Done():
			require.FailNow(t, "watcher did not retry scan")
		}
	}
	watcher.Stop()
	assert.GreaterOrEqual(t, watcher.SourceScanStats().Passes, uint64(1))
}

func TestSourceScanDetectsEqualStatReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("one"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	scanner := newSourceScanner([]WatchRoot{{Path: root, SourceFileGlobs: []string{"*.jsonl"}}}, nil)
	require.NoError(t, scanner.scan(t.Context(), func(context.Context, WatchBatch) error { return nil }))
	replacement := filepath.Join(root, "replacement")
	require.NoError(t, os.WriteFile(replacement, []byte("two"), 0o600))
	require.NoError(t, os.Chtimes(replacement, info.ModTime(), info.ModTime()))
	require.NoError(t, os.Rename(replacement, path))
	var paths []string
	require.NoError(t, scanner.scan(t.Context(), func(_ context.Context, batch WatchBatch) error { paths = append(paths, batch.Paths...); return nil }))
	assert.Equal(t, []string{path}, paths)
}

func TestSourceScanFollowsDeclaredProjectSymlinks(t *testing.T) {
	root := t.TempDir()
	project := t.TempDir()
	path := filepath.Join(project, "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("one"), 0o600))
	require.NoError(t, os.Symlink(project, filepath.Join(root, "linked-project")))
	scanner := newSourceScanner([]WatchRoot{{Path: root, Recursive: true, FollowChildDirectorySymlinks: true, SourceFileGlobs: []string{"*.jsonl"}}}, nil)
	require.NoError(t, scanner.scan(t.Context(), func(context.Context, WatchBatch) error { return nil }))
	require.NoError(t, os.WriteFile(path, []byte("appended"), 0o600))
	var paths []string
	require.NoError(t, scanner.scan(t.Context(), func(_ context.Context, batch WatchBatch) error { paths = append(paths, batch.Paths...); return nil }))
	assert.Equal(t, []string{filepath.Join(root, "linked-project", "session.jsonl")}, paths)
}

func TestSourceScanSaturationStillReconcilesUncachedDeletion(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one.jsonl", "two.jsonl"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), nil, 0o600))
	}
	scanner := newSourceScanner([]WatchRoot{{Path: root, SourceFileGlobs: []string{"*.jsonl"}}}, nil)
	scanner.maxEntries = 1
	require.NoError(t, scanner.scan(t.Context(), func(context.Context, WatchBatch) error { return nil }))
	require.NoError(t, os.Remove(filepath.Join(root, "two.jsonl")))
	var roots []string
	require.NoError(t, scanner.scan(t.Context(), func(_ context.Context, batch WatchBatch) error {
		roots = append(roots, batch.ReconcileRoots...)
		return nil
	}))
	assert.Equal(t, []string{root}, roots)
}

func TestSourceScanSidecarDoesNotReconcileMissingTranscriptScope(t *testing.T) {
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	require.NoError(t, os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte("{}\n"), 0o600))
	scanner := newSourceScanner([]WatchRoot{{Path: home, SourceFileGlobs: []string{"session_index.jsonl"}, SourceScanScopes: []WatchScope{{Agent: "codex", SyncDir: sessions}}}}, nil)
	var batches []WatchBatch
	require.NoError(t, scanner.scan(t.Context(), func(_ context.Context, batch WatchBatch) error { batches = append(batches, batch); return nil }))
	assert.Empty(t, batches, "the present companion directory does not prove transcript availability")
	require.NoError(t, os.Mkdir(sessions, 0o700))
	require.NoError(t, scanner.scan(t.Context(), func(_ context.Context, batch WatchBatch) error { batches = append(batches, batch); return nil }))
	require.Len(t, batches, 1)
	assert.Equal(t, []string{sessions}, batches[0].ReconcileRoots)
}

func TestWatcherSourceScanFailureDoesNotBlockLaterRoots(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for _, root := range []string{first, second} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "session.jsonl"), nil, 0o600))
	}
	calls := make(chan WatchBatch, 8)
	watcher, err := newWatcherWithBackend(0, 0, func(_ context.Context, batch WatchBatch) error {
		calls <- batch
		return errors.New("archive failure")
	}, newFakeWatchBackend(), 8192, 2<<20)
	require.NoError(t, err)
	watcher.RegisterRoots([]WatchRoot{{Path: first, SourceFileGlobs: []string{"*.jsonl"}}, {Path: second, SourceFileGlobs: []string{"*.jsonl"}}}, 0)
	t.Cleanup(watcher.Stop)
	require.NoError(t, watcher.Start())
	found := false
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !found {
		select {
		case batch := <-calls:
			found = slices.Contains(batch.ReconcileRoots, second)
		case <-deadline.C:
			require.FailNow(t, "a failed root blocked scanning its sibling")
		}
	}
	watcher.Stop()
}

func TestSourceScanCancellationDuringPageDispatch(t *testing.T) {
	root := t.TempDir()
	for i := range 300 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("session-%03d.jsonl", i)), nil, 0o600))
	}
	scanner := newSourceScanner([]WatchRoot{{Path: root, SourceFileGlobs: []string{"*.jsonl"}}}, nil)
	require.NoError(t, scanner.scan(t.Context(), func(context.Context, WatchBatch) error { return nil }))
	for i := range 300 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("session-%03d.jsonl", i)), []byte("changed"), 0o600))
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	err := scanner.scan(ctx, func(_ context.Context, batch WatchBatch) error {
		calls++
		assert.Len(t, batch.Paths, 256)
		cancel()
		return context.Canceled
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls)
}

func TestSourceScanSidecarContinuesWhenArchiveRootIsMissing(t *testing.T) {
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	require.NoError(t, os.Mkdir(sessions, 0o700))
	index := filepath.Join(home, "session_index.jsonl")
	require.NoError(t, os.WriteFile(index, []byte("one"), 0o600))
	scanner := newSourceScanner([]WatchRoot{{Path: home, SourceFileGlobs: []string{"session_index.jsonl"}, SourceScanScopes: []WatchScope{{Agent: "codex", SyncDir: sessions}, {Agent: "codex", SyncDir: filepath.Join(home, "archived_sessions")}}}}, nil)
	var batches []WatchBatch
	emit := func(_ context.Context, batch WatchBatch) error { batches = append(batches, batch); return nil }
	require.NoError(t, scanner.scan(t.Context(), emit))
	require.Len(t, batches, 1)
	assert.Equal(t, []string{sessions}, batches[0].ReconcileRoots)
	batches = nil
	require.NoError(t, os.WriteFile(index, []byte("renamed"), 0o600))
	require.NoError(t, scanner.scan(t.Context(), emit))
	require.Len(t, batches, 1)
	assert.Equal(t, []string{index}, batches[0].Paths)
}

func TestWatcherSourceScanPagesDoNotPayNativeDispatchFloor(t *testing.T) {
	root := t.TempDir()
	for i := range 300 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("session-%03d.jsonl", i)), nil, 0o600))
	}
	calls := make(chan WatchBatch, 4)
	watcher, err := newWatcherWithBackend(0, time.Hour, func(_ context.Context, batch WatchBatch) error { calls <- batch; return nil }, newFakeWatchBackend(), 8192, 2<<20)
	require.NoError(t, err)
	watcher.RegisterRoots([]WatchRoot{{Path: root, SourceFileGlobs: []string{"*.jsonl"}}}, 0)
	// A warm scanner with uncached sources must stream multiple bounded pages.
	watcher.scanner.roots[0].initialized = true
	t.Cleanup(watcher.Stop)
	require.NoError(t, watcher.Start())
	paths := 0
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for paths < 300 {
		select {
		case batch := <-calls:
			paths += len(batch.Paths)
		case <-timer.C:
			require.FailNow(t, "metadata pages waited on the native event dispatch floor")
		}
	}
	watcher.Stop()
}
