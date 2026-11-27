package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A conversation's saved tool output can be modified after its transcript,
// such as an output written by a long command. When that transcript is
// deleted, a copy in another home whose transcript is newer takes over with
// its turns; the saved output's time does not count as transcript activity.
func TestSyncCodeBuddyDeletedCopyRanksByTranscriptTime(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync func(t *testing.T, e *Engine, paths ...string)
	}{
		{"sync", func(t *testing.T, e *Engine, _ ...string) {
			e.SyncAll(t.Context(), nil)
		}},
		{"change events", func(_ *testing.T, e *Engine, paths ...string) {
			e.SyncPaths(paths)
		}},
		{"watched-folder check", func(t *testing.T, e *Engine, _ ...string) {
			require.NoError(t, e.ReconcileWatchRoots(t.Context(), nil, true))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deletedHome, deletedProject := cbcSyncHome(t)
			survivorHome, survivorProject := cbcSyncHome(t)
			deleted := filepath.Join(deletedProject, "conv1.jsonl")
			survivor := filepath.Join(survivorProject, "conv1.jsonl")
			output := filepath.Join(deletedProject, "conv1", "tool-results", "call-1.txt")
			writeCBCSync(t, deleted, cbcCopyTurns(1, "first")...)
			writeCBCSync(t, output, "saved output")
			now := time.Now()
			transcriptTime := now.Add(-time.Hour)
			outputTime := now.Add(-10 * time.Minute)
			require.NoError(t, os.Chtimes(deleted, transcriptTime, transcriptTime))
			require.NoError(t, os.Chtimes(output, outputTime, outputTime))

			database := openTestDB(t)
			engine := newCBCEngine(t, database, deletedHome, survivorHome)
			engine.SyncAll(t.Context(), nil)
			assertCBCCopyOwner(t, database, deleted, 1, "the only copy owns the conversation")

			// The surviving transcript is newer than the deleted one but
			// older than the deleted copy's saved output.
			writeCBCSync(t, survivor, cbcCopyTurns(3, "third")...)
			survivorTime := now.Add(-30 * time.Minute)
			require.NoError(t, os.Chtimes(survivor, survivorTime, survivorTime))
			require.NoError(t, os.Remove(deleted))
			tc.sync(t, engine, deleted, survivor)
			assertCBCCopyOwner(t, database, survivor, 3,
				"the copy whose transcript is newer takes over with its turns")
		})
	}
}

// A sync limited to recent changes still refreshes a saved tool output
// rewritten after an older transcript.
func TestSyncCodeBuddyRecentSyncFollowsSavedOutput(t *testing.T) {
	home, project := cbcSyncHome(t)
	transcript := filepath.Join(project, "s1.jsonl")
	side := filepath.Join(project, "s1", "tool-results", "c1.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(side), 0o755))
	require.NoError(t, os.WriteFile(side, []byte("first output"), 0o600))
	writeCBCSync(t, transcript, cbcPersistedTurn(side)...)
	since := backdateCBCTrees(t, home)

	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	engine.SyncAll(t.Context(), nil)
	require.Equal(t, "first output", cbcToolResult(t, database, "codebuddy:s1", "c1"))
	requireQuickSyncSkipsUnchanged(t, engine, since)

	// Only the saved output changes after the cutoff.
	require.NoError(t, os.WriteFile(side, []byte("second output"), 0o600))
	later := since.Add(time.Second)
	require.NoError(t, os.Chtimes(side, later, later))
	engine.SyncAllSince(t.Context(), since, nil)
	assert.Equal(t, "second output", cbcToolResult(t, database, "codebuddy:s1", "c1"))
}
