package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two configured homes can hold one conversation, such as a backup beside
// the live home. When CodeBuddy deletes the newer transcript, the archive
// keeps its turns and records the source as missing; the older backup takes
// over only once it is modified after the archived transcript was.
func TestSyncCodeBuddyDeletedNewerCopyKeepsArchivedTurns(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sync        func(t *testing.T, e *Engine, paths ...string)
		wantMissing bool
	}{
		{"sync", func(t *testing.T, e *Engine, _ ...string) {
			e.SyncAll(t.Context(), nil)
		}, false},
		{"change events", func(_ *testing.T, e *Engine, paths ...string) {
			e.SyncPaths(paths)
		}, true},
		{"watched-folder check", func(t *testing.T, e *Engine, _ ...string) {
			require.NoError(t, e.ReconcileWatchRoots(t.Context(), nil, true))
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newerHome, newerProject := cbcSyncHome(t)
			olderHome, olderProject := cbcSyncHome(t)
			newer := filepath.Join(newerProject, "conv1.jsonl")
			older := filepath.Join(olderProject, "conv1.jsonl")
			writeCBCSync(t, newer, cbcCopyTurns(3, "third")...)
			writeCBCSync(t, older, cbcCopyTurns(1, "first")...)
			archived := time.Now().Add(-30 * time.Minute)
			stale := time.Now().Add(-time.Hour)
			require.NoError(t, os.Chtimes(newer, archived, archived))
			require.NoError(t, os.Chtimes(older, stale, stale))

			database := openTestDB(t)
			engine := newCBCEngine(t, database, newerHome, olderHome)
			engine.SyncAll(t.Context(), nil)
			assertCBCCopyOwner(t, database, newer, 3, "the newer copy owns the conversation")

			require.NoError(t, os.Remove(newer))
			tc.sync(t, engine, newer, older)
			assertCBCCopyOwner(t, database, newer, 3,
				"the older backup does not replace the deleted transcript's turns")
			if tc.wantMissing {
				sess, err := database.GetSessionFull(t.Context(), "codebuddy:conv1")
				require.NoError(t, err)
				assertSourceMissingState(t, sess)
			}

			writeCBCSync(t, older, cbcCopyTurns(4, "resumed")...)
			resumed := time.Now()
			require.NoError(t, os.Chtimes(older, resumed, resumed))
			tc.sync(t, engine, older)
			assertCBCCopyOwner(t, database, older, 4,
				"a backup modified after the archived transcript takes over")
		})
	}
}

// writeCodeBuddyHistory writes a 4.x conversation with one user turn under
// a home's history folder and returns its manifest path.
func writeCodeBuddyHistory(t *testing.T, home, id, text string) string {
	t.Helper()
	convDir := filepath.Join(home, "history", "ws", id)
	require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
	manifest := filepath.Join(convDir, "index.json")
	require.NoError(t, os.WriteFile(manifest,
		[]byte(`{"messages":[{"id":"u1","role":"user"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "u1.json"),
		[]byte(`{"role":"user","message":{"content":[{"type":"text","text":"`+text+`"}]}}`), 0o600))
	return manifest
}

// A configured CodeBuddy home can hold CLI/5.x transcripts beside 4.x
// history. A watched-folder scan of that home reconciles both: it saves a
// new 4.x conversation and a new transcript, and records a removed 4.x
// conversation as missing.
func TestSyncCodeBuddyHomeScanCoversBothLayouts(t *testing.T) {
	for _, full := range []bool{false, true} {
		name := "scoped scan"
		if full {
			name = "full recovery scan"
		}
		t.Run(name, func(t *testing.T) {
			home, project := cbcSyncHome(t)
			writeCBCSync(t, filepath.Join(project, "cli1.jsonl"), cbcCopyTurns(1, "cli")...)
			removed := writeCodeBuddyHistory(t, home, "old4", "old")
			database := openTestDB(t)
			engine := newCBCEngine(t, database, home)
			engine.SyncAll(t.Context(), nil)
			for _, id := range []string{"codebuddy:cli1", "codebuddy:old4"} {
				sess, err := database.GetSessionFull(t.Context(), id)
				require.NoError(t, err)
				require.NotNil(t, sess, id)
			}

			added := writeCodeBuddyHistory(t, home, "new4", "new")
			addedCLI := filepath.Join(project, "cli2.jsonl")
			writeCBCSync(t, addedCLI, cbcCopyTurns(1, "cli two")...)
			require.NoError(t, os.RemoveAll(filepath.Dir(removed)))
			require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{home}, full))

			sess, err := database.GetSessionFull(t.Context(), "codebuddy:new4")
			require.NoError(t, err)
			require.NotNil(t, sess, "the scan saves a new 4.x conversation")
			require.NotNil(t, sess.FilePath)
			assert.Equal(t, added, *sess.FilePath)
			sess, err = database.GetSessionFull(t.Context(), "codebuddy:cli2")
			require.NoError(t, err)
			assert.NotNil(t, sess, "the scan saves a new transcript")
			sess, err = database.GetSessionFull(t.Context(), "codebuddy:old4")
			require.NoError(t, err)
			assertSourceMissingState(t, sess)
		})
	}
}
