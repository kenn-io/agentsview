//go:build chtest

package clickhouse

import (
	"context"
	"database/sql"
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
	assert.Equal(t, "resuming interrupted full push (first push from this archive)", final.FullReason)
	assert.Equal(t, 1, final.SessionsPushed)
	assert.Equal(t, 2, final.SkippedUnchanged)
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
	_, err := newTestSync(t, local, target, opts).Push(killCtx, false, nil)
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
	assert.Equal(t, "resuming interrupted full push (first push from this archive)", res.FullReason)
	assert.Zero(t, res.SessionsPushed)
	assert.Equal(t, 3, res.SkippedUnchanged)
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
