package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FSEvents reports a watched root that is moved away, and the watcher marks
// its sources missing. Linux does not report the move in a form the watcher
// acts on, and Windows refuses to rename a watched directory, so this outcome
// is specific to macOS.
func TestIdleWakeReportsRootRemovedDuringPause(t *testing.T) {
	f := newIngestionLifecycleFixture(t)
	pauseAfterStartup(t, f.ingestion)
	require.NoError(t, os.Rename(f.root, filepath.Join(t.TempDir(), "offline")))

	// A running watcher marks the sources of a removed root as missing and
	// keeps their history. A pause must not lose that report.
	require.Eventually(t, func() bool {
		f.ingestion.EnsureFresh(t.Context())
		after, err := f.database.GetSessionFull(t.Context(), "gemini:retained")
		return err == nil && after != nil && after.SourceMissingAt != nil
	}, 20*time.Second, 100*time.Millisecond)
	after, err := f.database.GetSessionFull(t.Context(), "gemini:retained")
	require.NoError(t, err)
	assert.Nil(t, after.DeletedAt, "missing sources retain their archived history")
	messages, err := f.database.GetAllMessages(t.Context(), "gemini:retained")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "hello from retained", messages[0].Content)
}
