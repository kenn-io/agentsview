//go:build chtest

package clickhouse

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

// mirrorQueriesSince counts the finished queries clients sent to conn's
// database since start that match condition, a predicate over
// system.query_log. Queries the server runs itself, such as a view
// refresh an earlier push started, are left out.
func mirrorQueriesSince(t *testing.T, conn *sql.DB, start time.Time, condition string) uint64 {
	t.Helper()
	_, err := conn.ExecContext(t.Context(), "SYSTEM FLUSH LOGS")
	require.NoError(t, err)
	var n uint64
	require.NoError(t, conn.QueryRowContext(t.Context(), `
		SELECT count() FROM system.query_log
		WHERE current_database = currentDatabase() AND type = 'QueryFinish'
			AND is_initial_query AND interface = 1
			AND query_start_time_microseconds >= fromUnixTimestamp64Micro(?)
			AND (`+condition+`)`, start.UnixMicro()).Scan(&n))
	return n
}

var changeTriggered = storage.PushOptions{SkipIfUnchanged: true}

// A change-triggered push with nothing new in the archive writes nothing to
// the mirror and scans none of its sessions, whatever the archive keeps.
func TestChangeTriggeredPushWritesNothingWhenArchiveUnchanged(t *testing.T) {
	t.Parallel()
	for _, content := range []config.ArchiveContent{config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(content), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			local, target := seedFixture(t)
			local.SetArchiveContent(content)
			s := newTestSync(t, local, target, storage.PusherOptions{})
			_, err := s.Push(ctx, false, nil)
			require.NoError(t, err)
			conn := chtest.Open(t, target.URL, target.Database)

			start := time.Now()
			res, err := s.PushWithOptions(ctx, changeTriggered, nil)
			require.NoError(t, err)
			assert.Zero(t, res.SessionsPushed)
			assert.Zero(t, mirrorQueriesSince(t, conn, start, "query_kind != 'Select'"),
				"an unchanged archive writes no rows, metadata, or schema")
			assert.Zero(t, mirrorQueriesSince(t, conn, start, "has(tables, currentDatabase() || '.sessions')"),
				"an unchanged archive reads no session rows")
		})
	}
}

// Each local change a change-triggered push owns reaches the mirror.
func TestChangeTriggeredPushWritesLocalChanges(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		change func(t *testing.T, local *db.DB)
		landed func(t *testing.T, conn *sql.DB) bool
	}{
		"new message": {
			change: func(t *testing.T, local *db.DB) {
				appendMessage(t, local, fixtureBetaID, "beta again", "2026-01-11T00:01:00.000Z")
			},
			landed: func(t *testing.T, conn *sql.DB) bool {
				var n int
				require.NoError(t, conn.QueryRowContext(t.Context(),
					"SELECT count() FROM messages WHERE session_id = ?", fixtureBetaID).Scan(&n))
				return n == 2
			},
		},
		"star": {
			change: func(t *testing.T, local *db.DB) {
				ok, err := local.StarSession(t.Context(), fixtureBetaID)
				require.NoError(t, err)
				require.True(t, ok)
			},
			landed: func(t *testing.T, conn *sql.DB) bool {
				var n int
				require.NoError(t, conn.QueryRowContext(t.Context(),
					"SELECT count() FROM starred_sessions WHERE session_id = ?", fixtureBetaID).Scan(&n))
				return n == 1
			},
		},
		"deleted session": {
			change: func(t *testing.T, local *db.DB) {
				require.NoError(t, local.DeleteSession(t.Context(), fixtureBetaID))
			},
			landed: func(t *testing.T, conn *sql.DB) bool {
				var n int
				require.NoError(t, conn.QueryRowContext(t.Context(),
					"SELECT count() FROM sessions WHERE id = ?", fixtureBetaID).Scan(&n))
				return n == 0
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			local, target := seedFixture(t)
			s := newTestSync(t, local, target, storage.PusherOptions{})
			_, err := s.Push(ctx, false, nil)
			require.NoError(t, err)
			conn := chtest.Open(t, target.URL, target.Database)
			require.False(t, tc.landed(t, conn), "the fixture must not already show the change")

			tc.change(t, local)
			_, err = s.PushWithOptions(ctx, changeTriggered, nil)
			require.NoError(t, err)
			assert.True(t, tc.landed(t, conn))
		})
	}
}

// Upkeep a change-triggered push leaves when the archive is unchanged, such
// as new model prices, lands with the next push that is not change-triggered.
func TestPushAfterSkippedChangePushPublishesPrices(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	conn := chtest.Open(t, target.URL, target.Database)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "claude-idle-test",
		InputPerMTok:  money.MustParseDollars("7"),
		OutputPerMTok: money.MustParseDollars("21"),
	}}))
	pattern := func() int {
		var n int
		require.NoError(t, conn.QueryRowContext(ctx,
			"SELECT count() FROM model_pricing WHERE model_pattern = 'claude-idle-test'").Scan(&n))
		return n
	}

	_, err = s.PushWithOptions(ctx, changeTriggered, nil)
	require.NoError(t, err)
	require.Zero(t, pattern(), "the change-triggered push left prices for later")

	_, err = s.PushWithOptions(ctx, storage.PushOptions{}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, pattern())
}

// A second Sync for a mirror this process already prepared skips the table
// checks, and one whose tables changed since prepares them again.
func TestEnsureSchemaReusesSchemaThisProcessPrepared(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	newTestSync(t, local, target, storage.PusherOptions{})
	conn := chtest.Open(t, target.URL, target.Database)

	start := time.Now()
	newTestSync(t, local, target, storage.PusherOptions{})
	assert.Zero(t, mirrorQueriesSince(t, conn, start, "query_kind IN ('Create', 'Alter', 'Insert', 'Drop')"),
		"an unchanged schema runs no DDL and records nothing")

	_, err := conn.ExecContext(ctx, "DROP TABLE pinned_messages SYNC")
	require.NoError(t, err)
	newTestSync(t, local, target, storage.PusherOptions{})
	var tables int
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT count() FROM system.tables WHERE database = currentDatabase() AND name = 'pinned_messages'",
	).Scan(&tables))
	assert.Equal(t, 1, tables, "a dropped table is created again")
}
