//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"go.kenn.io/agentsview/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkpointSpy is a test double for duckDBMaintenance that records how many
// times checkpointAfterPush is invoked and can be made to fail on demand.
type checkpointSpy struct {
	calls int
	err   error
}

func (s *checkpointSpy) checkpointAfterPush(ctx context.Context, duck *sql.DB) error {
	s.calls++
	return s.err
}

// mutateSessionStatColumns changes only file_size/file_inode — columns the
// mirror carries (see upsertSession) — simulating a resync stat refresh.
// These used to be excluded from the fingerprint payload as "volatile", but
// the mirrored-column invariant on duckSessionFingerprintFields now covers
// them: a mirrored column the fingerprint ignores can never be refreshed by
// an incremental push and stays stale until the next full rebuild.
func mutateSessionStatColumns(t *testing.T, local *db.DB, sessionID string) {
	t.Helper()
	require.NoError(t, local.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE sessions
			 SET file_size = COALESCE(file_size, 0) + 1,
			     file_inode = COALESCE(file_inode, 0) + 1
			 WHERE id = ?`,
			sessionID,
		)
		return err
	}))
}

// TestSyncIncrementalStatOnlyChangeRefreshesMirrorRow asserts the
// mirrored-column fingerprint invariant from the stat side: a stat-only
// change to a candidate session re-pushes it so the mirror's file_size/
// file_inode copies stay current, instead of being skipped as unchanged.
func TestSyncIncrementalStatOnlyChangeRefreshesMirrorRow(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)

	setSessionSignalsTo(t, local, "sess-1", probe.LastPushCutoff)
	mutateSessionStatColumns(t, local, "sess-1")

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total)
	assert.Equal(t, 0, res.Diagnostics.SkippedUnchangedSessions.Total)

	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	var fileSize, fileInode int64
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT file_size, file_inode FROM sessions WHERE id = ?`, "sess-1",
	).Scan(&fileSize, &fileInode))
	assert.Equal(t, int64(1), fileSize,
		"the refreshed stat columns must reach the mirror")
	assert.Equal(t, int64(1), fileInode)
}

// TestPushRepairsSessionDeletedDirectlyFromMirror is the required
// regression for the missing-mirror-row repair contract that replaces the
// old orphan-repair pass: a session brought back into the candidate window
// (marker >= probe.LastPushCutoff) whose mirror row was deleted out of band
// must be treated as changed (a missing row reads back as fingerprint "",
// which never matches) and repaired, even though its local content never
// changed. Bounded incremental push only reconsiders sessions the window
// actually selects; a session that never receives a marker bump after its
// mirror row is corrupted is out of scope for self-healing and instead
// requires 'duckdb push --full'.
func TestPushRepairsSessionDeletedDirectlyFromMirror(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)

	setSessionSignalsTo(t, local, "sess-1", probe.LastPushCutoff)

	conn, err := Open(path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, "sess-1")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	res, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full)
	assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total)
	assertMirrorMessageCount(t, path, "sess-1", 2)
}

// TestSyncCheckpointPolicyRunsOnlyAfterMutatingPush asserts checkpoint only
// runs when a push actually wrote something (pushed a session or applied a
// deletion), not on a push that leaves the mirror untouched.
func TestSyncCheckpointPolicyRunsOnlyAfterMutatingPush(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)

	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	syncer := newTestSync(t, path, local, SyncOptions{})
	spy := &checkpointSpy{}
	syncer.maintenance = spy
	_, err = syncer.runIncrementalPush(ctx, SyncOptions{}, probe, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, spy.calls, "no session changed, no deletions: no checkpoint")
	require.NoError(t, syncer.Close())

	appendMessage(t, local, "sess-1")
	probe, err = ProbeMirror(ctx, path)
	require.NoError(t, err)
	syncer = newTestSync(t, path, local, SyncOptions{})
	spy = &checkpointSpy{}
	syncer.maintenance = spy
	_, err = syncer.runIncrementalPush(ctx, SyncOptions{}, probe, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, spy.calls, "session changed: checkpoint runs once")
}

// TestSyncCheckpointFailureDoesNotAdvanceMirrorMetadata asserts that a push
// whose checkpoint step fails leaves the mirror's cutoff/last-push-at
// metadata untouched, even though the session content it pushed was already
// committed: a retry must see the same window again rather than silently
// skipping it.
func TestSyncCheckpointFailureDoesNotAdvanceMirrorMetadata(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	before, err := ProbeMirror(ctx, path)
	require.NoError(t, err)

	appendMessage(t, local, "sess-1")
	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	syncer := newTestSync(t, path, local, SyncOptions{})
	syncer.maintenance = &checkpointSpy{err: errors.New("checkpoint boom")}

	_, err = syncer.runIncrementalPush(ctx, SyncOptions{}, probe, nil)
	require.ErrorContains(t, err, "checkpoint boom")
	require.NoError(t, syncer.Close())

	assertMirrorMessageCount(t, path, "sess-1", 3)
	after, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	assert.Equal(t, before.LastPushCutoff, after.LastPushCutoff)
	assert.Equal(t, before.LastPushAt, after.LastPushAt)
}

// TestSyncCheckpointFailureAfterHardDeleteDoesNotAdvanceMirrorMetadata is
// the deletion-journal counterpart: a checkpoint failure after a hard
// delete was already applied to the mirror must not advance
// DeletionRevision, so a retry re-derives the same tombstone. Re-deleting an
// already-absent row is a harmless no-op, so this is safe.
func TestSyncCheckpointFailureAfterHardDeleteDoesNotAdvanceMirrorMetadata(
	t *testing.T,
) {
	ctx := context.Background()
	local, path := newPushFixture(t, 2)
	_, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err)
	before, err := ProbeMirror(ctx, path)
	require.NoError(t, err)

	require.NoError(t, local.DeleteSession("sess-1"))
	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	syncer := newTestSync(t, path, local, SyncOptions{})
	syncer.maintenance = &checkpointSpy{err: errors.New("checkpoint boom")}

	_, err = syncer.runIncrementalPush(ctx, SyncOptions{}, probe, nil)
	require.ErrorContains(t, err, "checkpoint boom")
	require.NoError(t, syncer.Close())

	assertMirrorSessionAbsent(t, path, "sess-1")
	after, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	assert.Equal(t, before.LastPushCutoff, after.LastPushCutoff)
	assert.Equal(t, before.DeletionRevision, after.DeletionRevision)
}

// TestDuckCheckpointDecisionRequiresFreeBlockThreshold exercises the pure
// shouldCheckpointDuckDB decision function directly.
func TestDuckCheckpointDecisionRequiresFreeBlockThreshold(t *testing.T) {
	tests := []struct {
		name string
		size duckDBSize
		want bool
	}{
		{"zero block size", duckDBSize{blockSize: 0, freeBlocks: 100}, false},
		{"zero free blocks", duckDBSize{blockSize: 4096, freeBlocks: 0}, false},
		{"below threshold", duckDBSize{blockSize: 4096, freeBlocks: 100}, false},
		{
			name: "at threshold",
			size: duckDBSize{
				blockSize:  4096,
				freeBlocks: (duckCheckpointMinFreeBytes + 4095) / 4096,
			},
			want: true,
		},
		{
			name: "well above threshold",
			size: duckDBSize{blockSize: 4096, freeBlocks: 1 << 20},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shouldCheckpointDuckDB(tt.size))
		})
	}
}
