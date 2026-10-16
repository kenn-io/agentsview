//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// resumeSessionCount is one more than a session batch, so a rebuild killed
// after its first batch commits has exactly one session left to push.
const resumeSessionCount = duckSessionPushBatchSize + 1

// seedResumeSessions writes resumeSessionCount sessions, session-000 the
// oldest and the last one the newest.
func seedResumeSessions(t *testing.T, local *db.DB) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writes := make([]db.SessionBatchWrite, 0, resumeSessionCount)
	for i := range resumeSessionCount {
		id := fmt.Sprintf("session-%03d", i)
		ts := base.Add(time.Duration(i) * time.Minute).Format("2006-01-02T15:04:05.000Z")
		writes = append(writes, db.SessionBatchWrite{
			Session:         syncSession(id, "alpha", id, ts, 1),
			Messages:        []db.Message{syncMessage(id, 0, "user", id, ts)},
			DataVersion:     1,
			ReplaceMessages: true,
		})
	}
	_, err := local.WriteSessionBatchAtomic(t.Context(), writes)
	require.NoError(t, err)
}

// killedRebuild runs a push that builds a new mirror and is killed once its
// first session batch has committed, the way a restart kills a long rebuild.
func killedRebuild(t *testing.T, path string, local *db.DB, opts storage.MirrorPushOptions) {
	t.Helper()
	ctx, kill := context.WithCancel(t.Context())
	defer kill()
	_, err := Push(ctx, path, local, "test-machine", opts, false, func(p storage.MirrorPushProgress) {
		if p.SessionsDone == duckSessionPushBatchSize {
			kill()
		}
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NoFileExists(t, path, "a killed rebuild never touches the mirror path")
}

func mirrorSessionIDs(t *testing.T, path string) map[string]bool {
	t.Helper()
	conn, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	rows, err := conn.QueryContext(t.Context(), `SELECT id FROM sessions`)
	require.NoError(t, err)
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids[id] = true
	}
	require.NoError(t, rows.Err())
	return ids
}

// A rebuild killed partway keeps the newest sessions it committed. The next
// push continues it, pushes only what is missing, and finishes the mirror.
func TestRebuildResumesAfterInterruption(t *testing.T) {
	local := newLocalDB(t)
	seedResumeSessions(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")

	killedRebuild(t, path, local, storage.MirrorPushOptions{})
	committed := mirrorSessionIDs(t, rebuildFilePath(path))
	assert.Len(t, committed, duckSessionPushBatchSize)
	assert.False(t, committed["session-000"], "the oldest session goes last")
	assert.True(t, committed["session-100"], "the newest session goes first")

	res, err := Push(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "resuming an unfinished rebuild, started because: missing file", res.Diagnostics.RebuildReason)
	assert.Equal(t, 1, res.SessionsPushed)
	assert.Equal(t, duckSessionPushBatchSize, res.Diagnostics.SkippedUnchangedSessions.Total)
	assert.Len(t, mirrorSessionIDs(t, path), resumeSessionCount)
	assert.NoFileExists(t, rebuildFilePath(path))

	next, err := Push(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	assert.Empty(t, next.Diagnostics.RebuildReason, "the resumed rebuild left a complete mirror")
}

// A session the interrupted rebuild committed and the archive then deleted
// does not reach the mirror.
func TestRebuildResumeDropsSessionsDeletedSinceInterruption(t *testing.T) {
	local := newLocalDB(t)
	seedResumeSessions(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")

	killedRebuild(t, path, local, storage.MirrorPushOptions{})
	require.True(t, mirrorSessionIDs(t, rebuildFilePath(path))["session-100"])
	require.NoError(t, local.DeleteSession(t.Context(), "session-100"))

	res, err := Push(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Diagnostics.DeletedStaleSessions)
	ids := mirrorSessionIDs(t, path)
	assert.Len(t, ids, resumeSessionCount-1)
	assert.False(t, ids["session-100"])
}

// A rebuild file built for another push scope is discarded, and the rebuild
// starts over.
func TestRebuildRestartsWhenScopeChangedSinceInterruption(t *testing.T) {
	local := newLocalDB(t)
	seedResumeSessions(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")

	killedRebuild(t, path, local, storage.MirrorPushOptions{})
	res, err := Push(t.Context(), path, local, "test-machine",
		storage.MirrorPushOptions{Projects: []string{"alpha"}}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "missing file", res.Diagnostics.RebuildReason)
	assert.Equal(t, resumeSessionCount, res.SessionsPushed)
	assert.Zero(t, res.Diagnostics.SkippedUnchangedSessions.Total)
}

// While another process has the rebuild file open, a push fails instead of
// rebuilding into the same file.
func TestPushFailsWhileRebuildFileIsHeld(t *testing.T) {
	local := newLocalDB(t)
	seedResumeSessions(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	killedRebuild(t, path, local, storage.MirrorPushOptions{})

	// A read-only holder in this process trips the same check another
	// process's open would: DuckDB refuses the push's read-write open.
	holder, err := OpenReadOnly(t.Context(), rebuildFilePath(path))
	require.NoError(t, err)
	defer func() { require.NoError(t, holder.Close()) }()

	_, err = Push(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, false, nil)
	require.ErrorContains(t, err, "another push is already rebuilding")
	assert.FileExists(t, rebuildFilePath(path), "the running rebuild's file survives")
}

// While another push holds the rebuild lock, a manual push fails and an
// automatic one defers, and neither touches the running rebuild's file.
func TestPushWaitsForRebuildLock(t *testing.T) {
	local := newLocalDB(t)
	seedResumeSessions(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	killedRebuild(t, path, local, storage.MirrorPushOptions{})

	holder := flock.New(rebuildFilePath(path) + ".lock")
	locked, err := holder.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	defer func() { require.NoError(t, holder.Unlock()) }()

	_, err = Push(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, false, nil)
	require.ErrorContains(t, err, "another push is already rebuilding")

	res, err := Push(t.Context(), path, local, "test-machine",
		storage.MirrorPushOptions{Automatic: true}, false, nil)
	require.NoError(t, err)
	assert.True(t, res.Diagnostics.Deferred)
	assert.Len(t, mirrorSessionIDs(t, rebuildFilePath(path)), duckSessionPushBatchSize)
}

// Cancelling a push while it reads the rebuild file keeps the file, so the
// next push still resumes it.
func TestPendingRebuildKeepsFileWhenCancelled(t *testing.T) {
	local := newLocalDB(t)
	seedResumeSessions(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	killedRebuild(t, path, local, storage.MirrorPushOptions{})
	run, err := newRebuildRun(t.Context(), local, "test-machine", storage.MirrorPushOptions{}, "missing file")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = pendingRebuild(ctx, path, run)
	require.Error(t, err)
	assert.FileExists(t, rebuildFilePath(path))
}

// --full starts a rebuild over instead of resuming an unfinished one, since
// the unfinished file's fingerprints cannot see sessions edited in place.
func TestFullPushDiscardsUnfinishedRebuild(t *testing.T) {
	local := newLocalDB(t)
	seedResumeSessions(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	killedRebuild(t, path, local, storage.MirrorPushOptions{})

	res, err := Push(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)
	assert.Equal(t, "--full requested", res.Diagnostics.RebuildReason)
	assert.Equal(t, resumeSessionCount, res.SessionsPushed)
	assert.Zero(t, res.Diagnostics.SkippedUnchangedSessions.Total)
}

// writeFailingSession adds a session whose message timestamp DuckDB rejects,
// so every push of it fails.
func writeFailingSession(t *testing.T, local *db.DB) {
	t.Helper()
	id := "session-bad"
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:         syncSession(id, "alpha", id, "2026-01-01T00:00:00.000Z", 1),
		Messages:        []db.Message{syncMessage(id, 0, "user", id, "not-a-timestamp")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
}

// A rebuild that ended with session errors resumes while the mirror still
// needs a rebuild.
func TestErroredRebuildResumesWhileRebuildRequired(t *testing.T) {
	local := newLocalDB(t)
	seedResumeSessions(t, local)
	writeFailingSession(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")

	_, err := Push(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, false, nil)
	require.ErrorContains(t, err, "session push errors")

	res, err := Push(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, false, nil)
	require.ErrorContains(t, err, "session push errors")
	assert.Equal(t, "resuming an unfinished rebuild, started because: missing file", res.Diagnostics.RebuildReason)
	assert.Equal(t, resumeSessionCount, res.Diagnostics.SkippedUnchangedSessions.Total)
}

// A rebuild that ended with session errors is dropped when the mirror needs
// no rebuild, so later pushes go back to incremental instead of retrying the
// failing rebuild forever.
func TestErroredRebuildDroppedWhenMirrorNeedsNoRebuild(t *testing.T) {
	local := newLocalDB(t)
	seedResumeSessions(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err := Push(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	writeFailingSession(t, local)

	_, err = Push(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, true, nil)
	require.ErrorContains(t, err, "session push errors")
	require.FileExists(t, rebuildFilePath(path))

	res, err := Push(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	assert.Empty(t, res.Diagnostics.RebuildReason)
	assert.Equal(t, 1, res.Errors)
	assert.NoFileExists(t, rebuildFilePath(path))
}
