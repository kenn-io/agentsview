//go:build !(windows && arm64)

package duckdb

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPushHeldSpareRejectsUnusableSnapshots(t *testing.T) {
	for _, mismatch := range []string{"scope", "archive"} {
		t.Run(mismatch, func(t *testing.T) {
			skipReopenTestOnWindows(t)
			ctx := t.Context()
			local, path := newPushFixture(t, 1)
			_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
			require.NoError(t, err)
			require.NoError(t, copyMirrorFile(ctx, path, sparePath(path)))
			conn, err := Open(ctx, sparePath(path))
			require.NoError(t, err)
			key := pushScopeMetadataKey
			if mismatch == "archive" {
				key = sourceArchiveIDMetadataKey
			}
			require.NoError(t, recordMetadataKey(ctx, conn, key, "other"))
			require.NoError(t, conn.Close())
			unusableFile, err := os.Open(sparePath(path))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, unusableFile.Close()) })
			unusable, err := unusableFile.Stat()
			require.NoError(t, err)
			store, err := NewStore(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			appendFrustrationPrompt(t, local, "sess-1")
			_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
			require.NoError(t, err)
			store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
			resp, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
			require.NoError(t, err)
			assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
			published, err := os.Stat(path)
			require.NoError(t, err)
			assert.False(t, os.SameFile(unusable, published), "an incompatible standby must be replaced")
			assert.NoFileExists(t, sparePath(path)+".wal")
		})
	}
}

func TestPushHeldSpareLifecycle(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "in_place", true: "full"}[full], func(t *testing.T) {
			ctx := t.Context()
			local, path := newPushFixture(t, 1)
			_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
			require.NoError(t, err)
			require.NoError(t, copyMirrorFile(ctx, path, sparePath(path)))
			_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, full, nil)
			require.NoError(t, err)
			if full {
				assert.NoFileExists(t, sparePath(path))
			} else {
				assert.FileExists(t, sparePath(path))
			}
		})
	}
}

func TestPushHeldSpareRetriesAfterSessionUpdateError(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	require.NoError(t, copyMirrorFile(ctx, path, sparePath(path)))
	held, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, held.Close()) })
	before, err := os.Stat(path)
	require.NoError(t, err)
	spare, err := os.Stat(sparePath(path))
	require.NoError(t, err)
	appendFrustrationPrompt(t, local, "sess-1")
	require.NoError(t, local.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE messages SET timestamp = 'invalid' WHERE session_id = 'sess-1' AND ordinal = 2`)
		return err
	}))
	res, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.Error(t, err)
	assert.Equal(t, 1, res.Errors)
	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, SameMirrorFile(before, after))
	require.NoError(t, local.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE messages SET timestamp = '2026-01-03T00:00:00Z' WHERE session_id = 'sess-1' AND ordinal = 2`)
		return err
	}))
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	live, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(spare, live), "retry must resume the standby")
	require.NoError(t, held.Close())
	assertMirrorMessageCount(t, path, "sess-1", 3)
}

func TestPushHeldSparePublishesNonSessionChanges(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = local.StarSession(ctx, "sess-1")
	require.NoError(t, err)
	res, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
	require.NoError(t, err)
	assert.Zero(t, res.SessionsPushed)
	store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
	var count int
	require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT count(*) FROM starred_sessions WHERE session_id = 'sess-1'`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestPushHeldSpareBootstrapsRecoveredWAL(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	appendFrustrationPrompt(t, local, "sess-1")
	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	s := newTestSync(t, path, local, storage.MirrorPushOptions{})
	s.machine = "m"
	s.maintenance = &checkpointSpy{}
	_, err = s.runIncrementalPush(ctx, storage.MirrorPushOptions{}, probe, nil)
	require.NoError(t, err)
	// Copy the committed WAL before Close checkpoints it, as a crashed push leaves it.
	main, err := os.ReadFile(path)
	require.NoError(t, err)
	wal, err := os.ReadFile(path + ".wal")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, os.WriteFile(path, main, 0o600))
	require.NoError(t, os.WriteFile(path+".wal", wal, 0o600))
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.Equal(t, 3, mirrorMessageCountViaStore(t, store, "sess-1"))
	// A second session triggers publication. Recovering the copied WAL must
	// preserve the first session's cursor so only the new session is pushed.
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{Session: syncSession("new-session", "alpha", "new", "2026-01-02T00:00:00Z", 1), Messages: []db.Message{syncMessage("new-session", 0, "user", "new", "2026-01-02T00:00:00Z")}, DataVersion: 1, ReplaceMessages: true}})
	require.NoError(t, err)
	appendFrustrationPrompt(t, local, "new-session")
	res, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsPushed, "the recovered WAL must prevent replaying sess-1 from SQLite")
	assert.NoFileExists(t, sparePath(path)+".wal")
	store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
	assert.Equal(t, 3, mirrorMessageCountViaStore(t, store, "sess-1"))
	assert.Equal(t, 2, mirrorMessageCountViaStore(t, store, "new-session"))
	// Reuse the retired main and recover its discarded WAL changes from SQLite.
	appendFrustrationPrompt(t, local, "sess-1")
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
	require.NoError(t, err)
	store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
	assert.Equal(t, 4, mirrorMessageCountViaStore(t, store, "sess-1"))
	assert.Equal(t, 2, mirrorMessageCountViaStore(t, store, "new-session"))
	require.NoError(t, store.Close())
	assert.NoFileExists(t, path+".wal")
	assertMirrorMessageCount(t, path, "sess-1", 4)
}

func TestPushHeldSpareRetriesAfterReaderReleases(t *testing.T) {
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	opts := storage.MirrorPushOptions{Automatic: true}
	_, err := Push(ctx, path, local, "m", opts, false, nil)
	require.NoError(t, err)
	require.NoError(t, copyMirrorFile(ctx, path, sparePath(path)))
	held, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, held.Close()) })
	heldSpare, err := OpenReadOnly(ctx, sparePath(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, heldSpare.Close()) })
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	spareBefore, err := os.ReadFile(sparePath(path))
	require.NoError(t, err)
	appendFrustrationPrompt(t, local, "sess-1")
	_, err = Push(ctx, path, local, "m", opts, false, nil)
	require.Error(t, err)
	assert.True(t, isMirrorHeldError(err))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	spareAfter, err := os.ReadFile(sparePath(path))
	require.NoError(t, err)
	assert.Equal(t, spareBefore, spareAfter)
	require.NoError(t, heldSpare.Close())
	_, err = Push(ctx, path, local, "m", opts, false, nil)
	require.NoError(t, err)
	require.NoError(t, held.Close())
	assertMirrorMessageCount(t, path, "sess-1", 3)
}

func TestPushHeldSpareRetiresWALBeforeReplacingMain(t *testing.T) {
	skipReopenTestOnWindows(t)
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	appendFrustrationPrompt(t, local, "sess-1")
	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	s := newTestSync(t, path, local, storage.MirrorPushOptions{})
	s.machine = "m"
	s.maintenance = &checkpointSpy{}
	_, err = s.runIncrementalPush(ctx, storage.MirrorPushOptions{}, probe, nil)
	require.NoError(t, err)
	main, err := os.ReadFile(path)
	require.NoError(t, err)
	wal, err := os.ReadFile(path + ".wal")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, os.WriteFile(path, main, 0o600))
	require.NoError(t, os.WriteFile(path+".wal", wal, 0o600))
	held, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, held.Close()) })

	// Leave the work directory writable, but prevent retiring the live WAL.
	// Linking the old main into the work directory can still succeed.
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0o500))
	defer func() { require.NoError(t, os.Chmod(dir, info.Mode().Perm())) }()
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
	require.ErrorIs(t, err, os.ErrPermission)
	var removeErr *os.PathError
	require.ErrorAs(t, err, &removeErr)
	assert.Equal(t, "remove", removeErr.Op)
	assert.Equal(t, path+".wal", removeErr.Path, "WAL retirement must fail before installing the new main")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, main, after)
	afterWAL, err := os.ReadFile(path + ".wal")
	require.NoError(t, err)
	assert.Equal(t, wal, afterWAL)
	require.NoError(t, held.Close())
	assertMirrorMessageCount(t, path, "sess-1", 3)
}
