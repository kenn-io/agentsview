package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamDirectoryTreeContinuesAfterUnreadableSubtree(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a-nested")
	require.NoError(t, os.Mkdir(nested, 0o755))
	healthy := filepath.Join(root, "z-healthy.jsonl")
	require.NoError(t, os.WriteFile(healthy, []byte("{}\n"), 0o600))
	injected := errors.New("subtree unavailable")
	ctx := withStreamingDirectoryReader(t.Context(), func(
		ctx context.Context, dir string, yield func(os.DirEntry) error,
	) error {
		if samePath(dir, nested) {
			return injected
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := yield(entry); err != nil {
				return err
			}
		}
		return nil
	})
	var yielded []string

	err := streamDirectoryTree(ctx, root, func(path string, _ os.DirEntry) error {
		yielded = append(yielded, path)
		return nil
	})

	assert.ErrorIs(t, err, injected)
	var incomplete DiscoveryIncompleteError
	assert.ErrorAs(t, err, &incomplete)
	assert.Equal(t, []string{healthy}, yielded)
}

func TestStreamDirectoryTreeYieldErrorAbortsImmediately(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.jsonl", "b.jsonl"} {
		require.NoError(t, os.WriteFile(
			filepath.Join(root, name), []byte("{}\n"), 0o600,
		))
	}
	injected := errors.New("stop consumer")
	calls := 0

	err := streamDirectoryTree(t.Context(), root, func(
		string, os.DirEntry,
	) error {
		calls++
		return injected
	})

	assert.ErrorIs(t, err, injected)
	assert.Equal(t, 1, calls)
}

func TestStreamDirectoryEntriesBoundsUnderlyingReadBatch(t *testing.T) {
	dir := t.TempDir()
	for i := range 300 {
		path := filepath.Join(dir, fmt.Sprintf("entry-%03d", i))
		require.NoError(t, os.WriteFile(path, nil, 0o600))
	}
	maxBuffered := 0
	ctx := WithStreamingDiscoveryBufferObserver(
		t.Context(),
		func(buffered int) { maxBuffered = max(maxBuffered, buffered) },
	)
	count := 0

	err := streamDirectoryEntries(ctx, dir, func(os.DirEntry) error {
		count++
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 300, count)
	assert.Positive(t, maxBuffered)
	assert.LessOrEqual(t, maxBuffered, streamingDirectoryBatchSize)
}

func TestStreamDirectoryEntriesStopsAfterMidTraversalCancellation(t *testing.T) {
	dir := t.TempDir()
	for i := range streamingDirectoryBatchSize * 3 {
		path := filepath.Join(dir, fmt.Sprintf("entry-%03d", i))
		require.NoError(t, os.WriteFile(path, nil, 0o600))
	}
	ctx, cancel := context.WithCancel(t.Context())
	count := 0

	err := streamDirectoryEntries(ctx, dir, func(os.DirEntry) error {
		count++
		if count == streamingDirectoryBatchSize+1 {
			cancel()
		}
		return nil
	})

	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, streamingDirectoryBatchSize+1, count)
}

func TestStreamDirectoryEntriesRejectsReplacementWhilePaused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	require.NoError(t, os.Mkdir(dir, 0o700))
	for _, name := range []string{"a.jsonl", "b.jsonl"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	paused := make(chan struct{})
	resume := make(chan struct{})
	first := true
	ctx := WithRawCaptureDiscoveryProgress(t.Context(), func() error {
		if first {
			first = false
			close(paused)
			<-resume
		}
		return nil
	})
	ctx = withRawCaptureStreamingTraversal(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- streamDirectoryEntries(ctx, dir, func(os.DirEntry) error {
			return nil
		})
	}()

	<-paused
	moved := dir + "-moved"
	require.NoError(t, os.Rename(dir, moved))
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "replacement.jsonl"), nil, 0o600))
	close(resume)

	assert.ErrorIs(t, <-errCh, errStreamingDirectoryChanged)
}
