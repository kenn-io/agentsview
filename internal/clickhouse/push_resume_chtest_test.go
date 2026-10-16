//go:build chtest

package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// pushedTables are the tables whose content a push determines.
var pushedTables = []string{
	"sessions", "messages", "tool_calls", "tool_result_events", "usage_events",
	"secret_findings", "pinned_messages", "starred_sessions", "usage_messages",
	"terminal_event_snapshots", usageSessionSnapshotTable,
}

// Each process is killed while writing its second session batch, the way a
// restart kills a long full push. Every restart must keep the sessions the
// earlier processes finished, so the push converges, and the result must
// match an uninterrupted push.
func TestFullPushResumesAcrossRestarts(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	conn := chtest.Open(t, target.URL, target.Database)

	var committed []string
	var final storage.PushResult
	for process := 1; ; process++ {
		require.LessOrEqual(t, process, 3, "each process finishes one batch, so three processes push all three sessions")
		s := newTestSync(t, local, target, storage.PusherOptions{})
		killCtx, kill := context.WithCancel(ctx)
		batches := 0
		s.sessionBatchSize = 1
		s.hooks = &pushHooks{
			beforeSessionRows: func(batch []db.Session) error {
				batches++
				if batches == 2 {
					kill()
					return killCtx.Err()
				}
				committed = append(committed, batch[0].ID)
				return nil
			},
		}
		res, err := s.Push(killCtx, false, nil)
		kill()
		if err == nil {
			final = res
			break
		}
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, process, chtest.Count(t, conn, "sessions", ""),
			"process %d kept the sessions earlier processes pushed", process)
	}

	assert.Equal(t, []string{fixtureBetaID, fixtureChildID, fixtureAlphaID}, committed,
		"sessions go newest first and none is pushed twice")
	assert.True(t, final.Full)
	assert.Equal(t, "resuming an unfinished full push, started because: first push from this archive", final.FullReason)
	assert.Equal(t, 1, final.SessionsPushed)
	assert.Equal(t, 2, final.SkippedResumed)
	assert.Zero(t, final.Errors)

	refDSN, refDatabase := chtest.FreshDatabase(t)
	refTarget := Target{URL: refDSN, Database: refDatabase}
	ref, err := newTestSync(t, local, refTarget, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, ref.Errors)
	refConn := chtest.Open(t, refDSN, refDatabase)
	for _, table := range pushedTables {
		assert.Equal(t, tableContent(t, refConn, table), tableContent(t, conn, table), table)
	}

	next, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, next.Full, "the resumed push recorded its completion")
	assert.Zero(t, next.SessionsPushed)
}

// A run recorded against an earlier archive generation is abandoned: rows it
// left may predate the rebuild, so every session is pushed again.
func TestFullPushRestartsWhenArchiveChangedSinceInterruption(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	killCtx, kill := context.WithCancel(ctx)
	batches := 0
	s.sessionBatchSize = 1
	s.hooks = &pushHooks{
		beforeSessionRows: func([]db.Session) error {
			batches++
			if batches == 2 {
				kill()
				return killCtx.Err()
			}
			return nil
		},
	}
	_, err := s.Push(killCtx, false, nil)
	kill()
	require.ErrorIs(t, err, context.Canceled)

	conn := chtest.Open(t, target.URL, target.Database)
	key := s.archiveKey(fullPushRunKeyBase)
	meta, err := readMetadata(ctx, conn, key)
	require.NoError(t, err)
	run, err := parseFullPushRun(meta[key])
	require.NoError(t, err)
	run.DatabaseID = "previous-generation"
	encoded, err := run.encode()
	require.NoError(t, err)
	require.NoError(t, writeMetadata(ctx, conn, map[string]string{key: encoded}))

	res, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "first push from this archive", res.FullReason)
	assert.Equal(t, 3, res.SessionsPushed)
}

// A full push killed during its vector phase resumes there: the sessions and
// the vectors it already pushed are not sent again.
func TestFullPushResumesVectorPhase(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	source := newFixtureVectorSource()
	opts := storage.PusherOptions{VectorSource: source}
	killCtx, kill := context.WithCancel(ctx)
	var exported []string
	source.onSessionDocs = func(id string) error {
		if len(exported) == 1 {
			kill()
			return killCtx.Err()
		}
		exported = append(exported, id)
		return nil
	}
	// One session per vector batch, so the first session's vectors commit
	// before the kill.
	interruptedSync := newTestSync(t, local, target, opts)
	interruptedSync.vectorBatchSessions = 1
	_, err := interruptedSync.Push(killCtx, false, nil)
	kill()
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, exported, 1)
	interrupted := exported[0]

	exported = nil
	source.onSessionDocs = func(id string) error {
		exported = append(exported, id)
		return nil
	}
	res, err := newTestSync(t, local, target, opts).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "resuming an unfinished full push, started because: first push from this archive", res.FullReason)
	assert.Zero(t, res.SessionsPushed)
	assert.Equal(t, 3, res.SkippedResumed)
	assert.Equal(t, 1, res.Vectors.SessionsPushed)
	assert.Equal(t, 1, res.Vectors.SessionsUnchanged)
	assert.Len(t, exported, 1)
	assert.NotContains(t, exported, interrupted)
}

// pushTimeColumns are columns besides push_version that take their value
// from the time of the push rather than from the archive.
var pushTimeColumns = map[string]string{
	"usage_messages":           ", revision",
	"terminal_event_snapshots": ", revision",
	usageSessionSnapshotTable:  ", revision, usage_events",
	"starred_sessions":         ", created_at",
}

// tableContent returns every row of table, sorted, without the columns a
// push stamps with its own time. Snapshot usage events carry a push version
// in each element and are covered by the usage_events table.
func tableContent(t *testing.T, conn *sql.DB, table string) []string {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(),
		"SELECT toString(tuple(* EXCEPT ("+pushVersionCol+pushTimeColumns[table]+"))) AS r FROM "+table+" ORDER BY r")
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		require.NoError(t, rows.Scan(&r))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

// killSecondSessionBatch makes s push one session per batch and kills the
// push as its second batch starts, so exactly the newest session commits.
func killSecondSessionBatch(t *testing.T, s *Sync, full bool) {
	t.Helper()
	killCtx, kill := context.WithCancel(t.Context())
	defer kill()
	batches := 0
	s.sessionBatchSize = 1
	s.hooks = &pushHooks{
		beforeSessionRows: func([]db.Session) error {
			batches++
			if batches == 2 {
				kill()
				return killCtx.Err()
			}
			return nil
		},
	}
	_, err := s.Push(killCtx, full, nil)
	require.ErrorIs(t, err, context.Canceled)
}

// A resumed full push rewrites vectors for every session, including those
// the interrupted process pushed, even when the caller scopes vectors to the
// sessions this push changed: the interrupted process never reached its
// vector phase.
func TestResumedFullPushWritesVectorsForEarlierSessions(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	opts := storage.PusherOptions{VectorSource: newFixtureVectorSource()}
	// A clean pass first, so a scoped vector push reads only its scope
	// instead of covering the whole generation.
	first, err := newTestSync(t, local, target, opts).Push(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.Vectors.SessionsPushed)
	killSecondSessionBatch(t, newTestSync(t, local, target, opts), true)

	res, err := newTestSync(t, local, target, opts).PushWithOptions(ctx,
		storage.PushOptions{ScopeVectorsToChangedSessions: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SkippedResumed, "the newest session committed before the kill")
	assert.Equal(t, 2, res.Vectors.SessionsPushed,
		"the committed session's vectors are pushed too")
}

// An explicit full push starts over instead of resuming an unfinished one,
// and its reason says it discarded that run only when one was waiting. The
// CLI resyncs the archive before a --full push, giving it a new generation
// the run cannot resume against; the discard is reported then too.
func TestRequestedFullPushDiscardsUnfinishedRun(t *testing.T) {
	for _, tt := range []struct {
		name     string
		resynced bool
	}{
		{name: "resumable run"},
		{name: "after archive resync", resynced: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			local, target := seedFixture(t)
			killSecondSessionBatch(t, newTestSync(t, local, target, storage.PusherOptions{}), false)
			if tt.resynced {
				require.NoError(t, local.SetDatabaseIDForTest(ctx, "resynced-generation"))
			}

			res, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, true, nil)
			require.NoError(t, err)
			assert.Equal(t, "requested; discarded an unfinished full push", res.FullReason)
			assert.Equal(t, 3, res.SessionsPushed)
			assert.Zero(t, res.SkippedResumed)

			again, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, true, nil)
			require.NoError(t, err)
			assert.True(t, again.Full)
			assert.Empty(t, again.FullReason, "nothing was discarded")
		})
	}
}

// A full push that ended with session errors does not keep later pushes
// full once nothing else requires one; they go back to incremental.
func TestErroredFullPushRunDroppedWhenNoFullPushRequired(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	first, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, first.Errors)

	failing := newTestSync(t, local, target, storage.PusherOptions{})
	failing.hooks = &pushHooks{beforeSessionRows: func(batch []db.Session) error {
		for _, sess := range batch {
			if sess.ID == fixtureAlphaID {
				return errors.New("simulated session failure")
			}
		}
		return nil
	}}
	errored, err := failing.Push(ctx, true, nil)
	require.NoError(t, err)
	require.NotZero(t, errored.Errors)

	conn := chtest.Open(t, target.URL, target.Database)
	key := failing.archiveKey(fullPushRunKeyBase)
	meta, err := readMetadata(ctx, conn, key)
	require.NoError(t, err)
	require.NotEmpty(t, meta[key], "the errored run stays recorded")

	next, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, next.Full)
	assert.Empty(t, next.FullReason)
	meta, err = readMetadata(ctx, conn, key)
	require.NoError(t, err)
	assert.Empty(t, meta[key], "the errored run record is cleared")
}

// A filtered full push deletes sessions outside its scope and then fails a
// session, so the stored scope stays unfiltered. When the next push restores
// that scope, nothing else forces a full push, but the deleted sessions are
// older than the stored cutoff and only a full push writes them back. The
// errored run belongs to the filtered scope, so it cannot resume and keeps
// the push full.
func TestErroredFilteredFullPushKeepsNextScopeFull(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	conn := chtest.Open(t, target.URL, target.Database)
	first, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, first.Errors)

	filtered := newTestSync(t, local, target, storage.PusherOptions{Projects: []string{"alpha"}})
	filtered.hooks = &pushHooks{beforeSessionRows: func(batch []db.Session) error {
		for _, sess := range batch {
			if sess.ID == fixtureAlphaID {
				return errors.New("simulated session failure")
			}
		}
		return nil
	}}
	errored, err := filtered.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, "push scope changed", errored.FullReason)
	require.NotZero(t, errored.Errors)
	require.Zero(t, chtest.Count(t, conn, "sessions", "id = ?", fixtureBetaID),
		"the filtered push removed the out-of-scope session")

	next, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.True(t, next.Full)
	assert.Equal(t, "push scope changed", next.FullReason)
	assert.Zero(t, next.Errors)
	assert.Equal(t, 1, chtest.Count(t, conn, "sessions", "id = ?", fixtureBetaID),
		"the out-of-scope session is back in the mirror")
	assert.Equal(t, 3, chtest.Count(t, conn, "sessions", ""))
}

// The push that restores the wider scope after a failed filtered push can
// fail too, on the very session the filtered push removed. Its run then
// matches the wider scope, but it must not be dropped: the session is older
// than the stored cutoff, so only a full push brings it back.
func TestFailedScopeRecoveryKeepsNextPushFull(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	conn := chtest.Open(t, target.URL, target.Database)
	first, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, first.Errors)

	failSession := func(s *Sync, id string) {
		s.hooks = &pushHooks{beforeSessionRows: func(batch []db.Session) error {
			for _, sess := range batch {
				if sess.ID == id {
					return errors.New("simulated session failure")
				}
			}
			return nil
		}}
	}
	filtered := newTestSync(t, local, target, storage.PusherOptions{Projects: []string{"alpha"}})
	failSession(filtered, fixtureAlphaID)
	errored, err := filtered.Push(ctx, false, nil)
	require.NoError(t, err)
	require.NotZero(t, errored.Errors)
	require.Zero(t, chtest.Count(t, conn, "sessions", "id = ?", fixtureBetaID))

	recovery := newTestSync(t, local, target, storage.PusherOptions{})
	failSession(recovery, fixtureBetaID)
	failed, err := recovery.Push(ctx, false, nil)
	require.NoError(t, err)
	require.True(t, failed.Full)
	require.NotZero(t, failed.Errors)
	require.Zero(t, chtest.Count(t, conn, "sessions", "id = ?", fixtureBetaID),
		"the failed recovery did not restore the removed session")

	next, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.True(t, next.Full)
	assert.Equal(t, "resuming an unfinished full push, started because: push scope changed", next.FullReason)
	assert.Zero(t, next.Errors)
	assert.Equal(t, 1, chtest.Count(t, conn, "sessions", "id = ?", fixtureBetaID),
		"the removed session is back in the mirror")

	after, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, after.Full, "the clean push cleared the run")
}
