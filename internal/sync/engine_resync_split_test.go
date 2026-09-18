package sync

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// newResyncSplitEngine builds an engine over a fresh archive with three synced
// Claude sessions. It returns the engine, its database, and the source root so
// callers can delete a source file and drive a resync.
func newResyncSplitEngine(t *testing.T) (*Engine, *db.DB, string) {
	t.Helper()
	root := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)

	for _, name := range []string{"keep0", "keep1", "orphan"} {
		path := filepath.Join(root, "project", name+".jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		content := testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-01-01T00:00:00Z", "hello "+name).
			AddClaudeAssistant("2026-01-01T00:00:01Z", "hi "+name).
			String()
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	require.Equal(t, 3, engine.SyncAll(t.Context(), nil).Synced)
	return engine, database, root
}

// TestResyncBuildThenSwapMatchesResyncAll drives the split resync path
// end-to-end: build the replacement, swap it in, reset caches. It must preserve
// an orphan session whose source file was deleted, clean up the temp file, and
// hand off warm skip state so an immediate sync is a no-op.
func TestResyncBuildThenSwapMatchesResyncAll(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	e, database, root := newResyncSplitEngine(t)
	require.NoError(os.Remove(filepath.Join(root, "project", "orphan.jsonl")))

	tempPath, stats, err := e.ResyncBuild(t.Context(), nil)
	require.NoError(err)
	require.False(stats.Aborted)
	require.FileExists(tempPath)

	installed, err := e.SwapResyncDatabase(tempPath)
	require.NoError(err)
	assert.True(installed)
	require.NoError(e.ResetCachesAfterSwap())

	assert.False(database.NeedsResync())
	orphan, err := database.GetSession(t.Context(), "orphan")
	require.NoError(err)
	require.NotNil(orphan, "orphan sessions must survive the split resync")
	assert.Positive(stats.TotalSessions)
	assert.NoFileExists(tempPath)

	warm := e.SyncAll(t.Context(), nil)
	assert.Zero(warm.Synced, "persisted skip state must survive the swap")
}

// TestSwapWindowRejectsDirectWrites proves the write barrier: with the writer
// closed a direct star write is rejected with ErrWriterClosed, and the rejected
// write is absent from the rebuilt archive after the swap.
func TestSwapWindowRejectsDirectWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	e, database, _ := newResyncSplitEngine(t)

	require.NoError(database.CloseWriter())
	_, starErr := database.StarSession("keep0")
	assert.ErrorIs(starErr, db.ErrWriterClosed,
		"a direct write during the barrier window must be rejected")

	// The build reads the original and writes only the replacement, so it
	// proceeds while the writer is closed. The swap reopens the writer.
	tempPath, _, err := e.ResyncBuild(t.Context(), nil)
	require.NoError(err)
	installed, err := e.SwapResyncDatabase(tempPath)
	require.NoError(err)
	assert.True(installed)
	require.NoError(e.ResetCachesAfterSwap())

	starred, err := database.ListStarredSessionIDs(t.Context())
	require.NoError(err)
	assert.NotContains(starred, "keep0",
		"a rejected write must not appear in the swapped archive")

	// The writer is usable again after the swap reopened it.
	ok, err := database.StarSession("keep0")
	require.NoError(err)
	assert.True(ok)
}

// TestResyncAbortsWhenBarrierCannotBeEstablished pins the CloseWriter failure
// posture: a resync that cannot establish a clean write barrier must abort
// before building instead of proceeding toward an unsafe swap, and the original
// archive must be left untouched.
func TestResyncAbortsWhenBarrierCannotBeEstablished(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	e, database, _ := newResyncSplitEngine(t)

	prev := closeWriterForResyncBarrier
	closeWriterForResyncBarrier = func(*db.DB) error {
		return errors.New("barrier boom")
	}
	defer func() { closeWriterForResyncBarrier = prev }()

	stats := e.ResyncAll(t.Context(), nil)
	assert.True(stats.Aborted, "resync must abort without a clean barrier")
	require.NotEmpty(stats.Warnings)
	assert.Contains(stats.Warnings[0], "close writer for barrier")
	assert.Zero(stats.Synced, "no build may run without the barrier")

	page, err := database.ListSessions(t.Context(), db.SessionFilter{})
	require.NoError(err)
	assert.Len(page.Sessions, 3, "original archive must be untouched")
	assert.NoFileExists(e.ResyncTempPath(),
		"no replacement build may be left behind")
}

// TestResyncBarrierCloseFailureRestoresWriter pins recovery after the abort in
// TestResyncAbortsWhenBarrierCannotBeEstablished: CloseWriter's failure posture
// leaves the writer closed (barrier active, undrained pool retained), and the
// aborted resync must reopen it so the daemon keeps serving writes instead of
// returning ErrWriterClosed until restart. Reopening is safe — this process
// never released write ownership.
func TestResyncBarrierCloseFailureRestoresWriter(t *testing.T) {
	e, database, _ := newResyncSplitEngine(t)

	prev := closeWriterForResyncBarrier
	closeWriterForResyncBarrier = func(d *db.DB) error {
		// Reproduce the drain-timeout posture: the writer really closes,
		// then the failure is reported.
		if err := d.CloseWriter(); err != nil {
			return err
		}
		return errors.New("barrier boom")
	}
	defer func() { closeWriterForResyncBarrier = prev }()

	stats := e.ResyncAll(t.Context(), nil)
	require.True(t, stats.Aborted, "resync must abort without a clean barrier")

	ok, err := database.StarSession("keep0")
	require.NoError(t, err,
		"writes must recover after the aborted resync without a restart")
	assert.True(t, ok)
}

func TestResyncMappingFailureKeepsOriginalArchive(t *testing.T) {
	for _, failure := range []string{"discovery", "apply"} {
		t.Run(failure, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			ctx := t.Context()
			root := t.TempDir()
			worktreePrefix := filepath.Join(t.TempDir(), "worktrees")
			sessionCwd := filepath.Join(worktreePrefix, "feature")
			sourcePath := filepath.Join(root, "source", "mapped.jsonl")
			require.NoError(os.MkdirAll(filepath.Dir(sourcePath), 0o755))
			require.NoError(os.WriteFile(sourcePath, []byte(
				testjsonl.NewSessionBuilder().
					AddClaudeUser(
						"2026-01-01T00:00:00Z", "mapped session", sessionCwd,
					).
					AddClaudeAssistant(
						"2026-01-01T00:00:01Z", "mapped reply",
					).
					String(),
			), 0o644))

			database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
			require.NoError(err)
			t.Cleanup(func() { require.NoError(database.Close()) })
			_, err = database.CreateWorktreeProjectMapping(
				ctx, db.WorktreeProjectMapping{
					Machine: "local", PathPrefix: worktreePrefix,
					Layout:  db.WorktreeMappingLayoutExplicit,
					Project: "canonical_project", Enabled: true,
				},
			)
			require.NoError(err)
			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentClaude: {root},
				},
				Machine: "local",
			})
			t.Cleanup(engine.Close)
			require.Equal(1, engine.SyncAll(ctx, nil).Synced)
			before, err := database.GetSession(ctx, "mapped")
			require.NoError(err)
			require.NotNil(before)
			require.Equal("canonical_project", before.Project)

			sentinel := errors.New("mapping " + failure + " failed")
			operations := rebuildOperations{}
			appliedMachines := []string{}
			switch failure {
			case "discovery":
				operations.listActiveWorktreeMappingMachines = func(
					context.Context, *db.DB,
				) ([]string, error) {
					return nil, sentinel
				}
			case "apply":
				operations.applyWorktreeMappings = func(
					_ context.Context, _ *db.DB, machine string,
				) (db.ApplyWorktreeProjectMappingsResult, error) {
					appliedMachines = append(appliedMachines, machine)
					return db.ApplyWorktreeProjectMappingsResult{}, sentinel
				}
			}

			stats, err := engine.resyncAllWithOptionsAndOperations(
				ctx, nil, RebuildOptions{}, operations,
			)
			require.ErrorIs(err, sentinel)
			assert.True(stats.Aborted)
			require.NotEmpty(stats.Warnings)
			assert.Contains(stats.Warnings[len(stats.Warnings)-1],
				"aborting swap")
			if failure == "apply" {
				assert.Equal([]string{"local"}, appliedMachines)
			}

			after, err := database.GetSession(ctx, "mapped")
			require.NoError(err)
			require.NotNil(after)
			assert.Equal("canonical_project", after.Project,
				"failed mapping reconciliation must retain the active archive")
			starred, err := database.StarSession("mapped")
			require.NoError(err,
				"the original archive must remain writable after the abort")
			assert.True(starred)
			assert.NoFileExists(engine.ResyncTempPath())
		})
	}
}

// TestResyncAbortsWhenReplacementCloseFails pins the replacement-close failure
// posture: when the freshly built temp database cannot drain its connections,
// committed rows may still sit uncheckpointed in the temp WAL. The build must
// surface the close error and abort the swap — renaming only the main file and
// deleting the temp WAL would discard those rows from the installed archive.
// The original archive stays in place and keeps serving reads and writes.
func TestResyncAbortsWhenReplacementCloseFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	e, database, _ := newResyncSplitEngine(t)
	restore := db.SetCloseDrainTimeoutForTest(100 * time.Millisecond)
	defer restore()

	// Pin a connection on the replacement from inside the build (the FTS
	// rebuild hook is the last ops seam that sees the open temp DB), so the
	// build's final Close cannot drain.
	var pinned *sql.Rows
	stats, err := e.resyncAllWithOptionsAndOperations(
		t.Context(), nil, RebuildOptions{}, rebuildOperations{
			rebuildFTS: func(newDB *db.DB) error {
				if err := newDB.RebuildFTS(); err != nil {
					return err
				}
				rows, qerr := newDB.Reader().Query("SELECT 1")
				if qerr != nil {
					return qerr
				}
				pinned = rows
				return nil
			},
		},
	)
	require.Error(err,
		"an undrained replacement close must abort the resync with an error")
	assert.True(stats.Aborted)
	require.NotEmpty(stats.Warnings)
	assert.Contains(stats.Warnings[len(stats.Warnings)-1], "aborting swap")

	// The original archive was not swapped and still serves reads and writes.
	page, listErr := database.ListSessions(t.Context(), db.SessionFilter{})
	require.NoError(listErr)
	assert.Len(page.Sessions, 3, "original archive must be untouched")
	ok, starErr := database.StarSession("keep0")
	require.NoError(starErr,
		"writes must recover after the aborted resync without a restart")
	assert.True(ok)

	require.NotNil(pinned, "the FTS hook must have pinned a connection")
	require.NoError(pinned.Close())
}

// newResyncSwapFailureEngine builds an engine over a Claude root with two
// synced sessions plus an empty Kimi root. The Kimi root lets failed-swap
// tests stage a source whose skip-cache identity is purely path+mtime (Kimi
// uses neither hash-keyed skip entries nor hash-based freshness healing), so a
// wrongly retained replacement-build skip entry is observable.
func newResyncSwapFailureEngine(t *testing.T) (*Engine, *db.DB, string) {
	t.Helper()
	claudeRoot := t.TempDir()
	kimiRoot := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeRoot},
			parser.AgentKimi:   {kimiRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	for _, name := range []string{"keep0", "keep1"} {
		path := filepath.Join(claudeRoot, "project", name+".jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		content := testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-01-01T00:00:00Z", "hello "+name).
			AddClaudeAssistant("2026-01-01T00:00:01Z", "hi "+name).
			String()
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	return engine, database, kimiRoot
}

// writeKimiGhost stages a Kimi wire.jsonl that parses to no sessions, so the
// next resync build records a fresh in-memory skip entry for it. It returns
// the transcript path, the session ID a real session at that path would get,
// and the file mtime the skip entry will be keyed on.
func writeKimiGhost(t *testing.T, kimiRoot string) (string, string, time.Time) {
	t.Helper()
	workdir := "wd_kimi-code_057f5c09ee3f"
	sessionDir := "session_cf2c3d74-c9d2-4ae4-95b7-d1d817298382"
	wirePath := filepath.Join(
		kimiRoot, workdir, sessionDir, "agents", "main", "wire.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(wirePath), 0o755))
	require.NoError(t, os.WriteFile(wirePath, []byte(
		`{"type": "metadata", "protocol_version": "1.3"}`+"\n",
	), 0o644))
	info, err := os.Stat(wirePath)
	require.NoError(t, err)
	sessionID := "kimi:" + workdir + ":main:" + sessionDir
	return wirePath, sessionID, info.ModTime()
}

// requireGhostSyncsAfterFailedSwap rewrites the ghost with a real session at
// the pre-resync mtime and requires the next pass to sync it. The discarded
// replacement's skip entry (path at that mtime) was never persisted in the
// original archive, so it must not survive the failed swap and suppress the
// source.
func requireGhostSyncsAfterFailedSwap(
	t *testing.T, e *Engine, database *db.DB,
	wirePath, sessionID string, mtime time.Time,
) {
	t.Helper()
	session := `{"type": "metadata", "protocol_version": "1.3"}` + "\n" +
		`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", ` +
		`"payload": {"user_input": [{"type": "text", "text": "Hello Kimi"}]}}}` + "\n" +
		`{"timestamp": 1704067202.0, "message": {"type": "TurnEnd", "payload": {}}}` + "\n"
	require.NoError(t, os.WriteFile(wirePath, []byte(session), 0o644))
	require.NoError(t, os.Chtimes(wirePath, mtime, mtime))

	synced := e.SyncAll(t.Context(), nil)
	require.False(t, synced.Aborted)
	sess, err := database.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess,
		"a skip entry from the discarded replacement build must not "+
			"suppress syncing this source into the original archive")
}

// TestResyncSwapRenameFailureRestoresSkipCache pins the rename-failure posture
// of the in-process swap: when the replacement cannot be renamed into place,
// the engine reopens the original archive, and the in-memory skip cache must
// return to its pre-build state. Keeping the replacement build's entries would
// suppress sources whose data never reached the original.
func TestResyncSwapRenameFailureRestoresSkipCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	e, database, kimiRoot := newResyncSwapFailureEngine(t)
	wirePath, sessionID, mtime := writeKimiGhost(t, kimiRoot)

	var removed atomic.Bool
	stats := e.ResyncAll(t.Context(), func(p Progress) {
		if p.Phase == PhaseSwappingDatabase &&
			removed.CompareAndSwap(false, true) {
			// Deleting the staged replacement makes the swap's os.Rename
			// fail after the original database is already closed.
			require.NoError(os.Remove(e.ResyncTempPath()))
		}
	})
	require.True(removed.Load(), "the swap phase must have been reached")
	require.True(stats.Aborted, "a failed rename must abort the resync")
	require.NotEmpty(stats.Warnings)
	assert.Contains(stats.Warnings[len(stats.Warnings)-1],
		"resync swap failed")

	// The original archive is still in place and serving.
	page, err := database.ListSessions(t.Context(), db.SessionFilter{})
	require.NoError(err)
	assert.Len(page.Sessions, 2, "original archive must be untouched")

	requireGhostSyncsAfterFailedSwap(t, e, database, wirePath, sessionID, mtime)
}

// TestResyncSwapCloseFailureRestoresSkipCache pins the close-failure posture
// of the in-process swap: when the original database cannot drain its
// connections before the rename, the swap aborts pre-install and the original
// is reopened. As with the rename failure, the replacement build's skip cache
// must not survive against the original archive.
func TestResyncSwapCloseFailureRestoresSkipCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	e, database, kimiRoot := newResyncSwapFailureEngine(t)
	wirePath, sessionID, mtime := writeKimiGhost(t, kimiRoot)

	restore := db.SetCloseDrainTimeoutForTest(100 * time.Millisecond)
	defer restore()

	// Pin a reader connection on the original so its pre-swap
	// CloseConnections cannot drain and the swap aborts before the rename.
	pinned, err := database.Reader().Query("SELECT 1")
	require.NoError(err)
	pinnedOpen := true
	defer func() {
		if pinnedOpen {
			require.NoError(pinned.Close())
		}
	}()

	stats := e.ResyncAll(t.Context(), nil)
	require.True(stats.Aborted,
		"a failed close before the swap must abort the resync")
	require.NotEmpty(stats.Warnings)
	assert.Contains(stats.Warnings[len(stats.Warnings)-1],
		"close before swap failed")

	require.NoError(pinned.Close())
	pinnedOpen = false

	// The original archive is still in place and serving.
	page, err := database.ListSessions(t.Context(), db.SessionFilter{})
	require.NoError(err)
	assert.Len(page.Sessions, 2, "original archive must be untouched")

	requireGhostSyncsAfterFailedSwap(t, e, database, wirePath, sessionID, mtime)
}

// TestInProcessResyncRejectsConcurrentDirectWrite is the regression for the
// in-process fallback barrier. It fires a direct write (StarSession) during the
// reclassify phase, which runs after every preserved-state copy and before the
// swap's rename. Without the barrier that write lands in the original and is
// discarded by the swap (silently lost); with it the write is rejected with
// ErrWriterClosed. The progress hook blocks the resync until the write returns,
// so the write is guaranteed to land inside that window.
func TestInProcessResyncRejectsConcurrentDirectWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	e, database, _ := newResyncSplitEngine(t)

	var starErr error
	var fired atomic.Bool
	done := make(chan struct{})
	onProgress := func(p Progress) {
		if p.Phase == PhaseReclassifying && fired.CompareAndSwap(false, true) {
			go func() {
				_, starErr = database.StarSession("keep0")
				close(done)
			}()
			<-done
		}
	}

	stats := e.ResyncAll(t.Context(), onProgress)
	require.False(stats.Aborted)
	require.True(fired.Load(), "the concurrent write must have fired mid-resync")

	assert.ErrorIs(starErr, db.ErrWriterClosed,
		"a direct write in the copy-to-swap window must be rejected, not lost")
	starred, err := database.ListStarredSessionIDs(t.Context())
	require.NoError(err)
	assert.NotContains(starred, "keep0",
		"a rejected write must not silently land in the swapped archive")
}
