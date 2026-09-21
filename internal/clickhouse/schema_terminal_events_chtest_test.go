//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestClickHouseTerminalSnapshotUpdates(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	for _, phase := range []struct {
		name, timestamp, status string
		version                 uint64
		want                    uint64
	}{
		{"initial", "2026-01-11T12:00:00Z", "completed", 1, 1},
		{"shorter", "2026-01-10T12:00:00Z", "completed", 2, 0},
		{"retry-later", "2026-01-11T12:00:00Z", "errored", 2, 1},
		{"retry-earlier", "2026-01-09T12:00:00Z", "completed", 2, 0},
		{"nonterminal", "2026-01-11T12:00:00Z", "running", 2, 0},
		{"empty", "", "", 3, 0},
	} {
		t.Run(phase.name, func(t *testing.T) {
			if phase.name == "initial" {
				_, err := store.DB().ExecContext(ctx, `INSERT INTO tool_result_events
 (session_id,tool_call_message_ordinal,call_index,event_index,source,status,timestamp,push_version)
 VALUES ('terminal-updates',0,0,1,'tool_execution','completed',parseDateTime64BestEffort('2026-01-08T12:00:00Z'),1)`)
				require.NoError(t, err)
			}
			if phase.timestamp != "" {
				_, err := store.DB().ExecContext(ctx, `INSERT INTO tool_result_events
 (session_id,tool_call_message_ordinal,call_index,event_index,source,status,timestamp,push_version)
 VALUES ('terminal-updates',0,0,0,'tool_execution',?,parseDateTime64BestEffort(?),?)`, phase.status, phase.timestamp, phase.version)
				require.NoError(t, err)
			}
			_, err := store.DB().ExecContext(ctx, "DELETE FROM tool_result_events WHERE session_id='terminal-updates' AND push_version<? SETTINGS mutations_sync=1", phase.version)
			require.NoError(t, err)
			_, err = store.DB().ExecContext(ctx, `INSERT INTO sessions
 (id,agent,project,started_at,ended_at,message_count,push_version)
 VALUES ('terminal-updates','codex','terminal-updates',parseDateTime64BestEffort('2026-01-01T12:00:00Z'),
 parseDateTime64BestEffort('2026-01-01T12:30:00Z'),1,?)`, phase.version)
			require.NoError(t, err)
			// An older copy of this same version finishes after the insert trigger.
			_, err = store.DB().ExecContext(ctx, `INSERT INTO terminal_event_snapshots
 VALUES ('terminal-updates',?,parseDateTime64BestEffort('2026-01-31T12:00:00Z'),toUInt128(?)*2)`, phase.version, phase.version)
			require.NoError(t, err)
			var hasTerminal uint8
			require.NoError(t, store.DB().QueryRowContext(ctx,
				"SELECT isNotNull(last_terminal_at) FROM terminal_event_snapshots WHERE session_id='terminal-updates'").Scan(&hasTerminal))
			require.Equal(t, phase.status == "completed" || phase.status == "errored", hasTerminal == 1)
			where, args := clickActivityReportCandidateWhere(db.AnalyticsFilter{Project: "terminal-updates"},
				"2026-01-11T00:00:00Z", "2026-01-12T00:00:00Z")
			var count uint64
			require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT count() FROM sessions s WHERE "+where, args...).Scan(&count))
			require.Equal(t, phase.want, count)
		})
	}
}

func TestClickHouseTerminalSnapshotBackfill(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	for _, query := range []string{
		"DROP VIEW terminal_event_snapshots_mv",
		"DROP TABLE terminal_event_snapshots",
		"DELETE FROM sync_metadata WHERE key='terminal_event_snapshots_backfill' SETTINGS mutations_sync=1",
		`INSERT INTO tool_result_events
   (session_id,tool_call_message_ordinal,call_index,event_index,source,status,timestamp,push_version)
   VALUES ('terminal-backfill',0,0,0,'tool_execution','completed',parseDateTime64BestEffort('2026-01-11T12:00:00Z'),7)`,
		`INSERT INTO sessions (id,agent,project,started_at,ended_at,message_count,push_version)
   VALUES ('terminal-backfill','codex','terminal-backfill',parseDateTime64BestEffort('2026-01-01T12:00:00Z'),
   parseDateTime64BestEffort('2026-01-01T12:30:00Z'),1,7)`,
	} {
		_, err := store.DB().ExecContext(ctx, query)
		require.NoError(t, err)
	}
	var before uint64
	require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT count() FROM tool_result_events").Scan(&before))
	require.ErrorContains(t, CheckSchemaCompat(ctx, store.DB()), "terminal_event_snapshots")
	require.NoError(t, ensureTerminalEventSnapshots(ctx, store.DB()))
	require.NoError(t, CheckSchemaCompat(ctx, store.DB()))
	where, args := clickActivityReportCandidateWhere(db.AnalyticsFilter{Project: "terminal-backfill"},
		"2026-01-11T00:00:00Z", "2026-01-12T00:00:00Z")
	var count uint64
	require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT count() FROM sessions s WHERE "+where, args...).Scan(&count))
	require.Equal(t, uint64(1), count)
	var after uint64
	require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT count() FROM tool_result_events").Scan(&after))
	require.Equal(t, before, after)
	_, err := store.DB().ExecContext(ctx, "DELETE FROM sync_metadata WHERE key='terminal_event_snapshots_backfill' SETTINGS mutations_sync=1")
	require.NoError(t, err)
	require.ErrorContains(t, CheckSchemaCompat(ctx, store.DB()), "terminal event backfill is incomplete")
	require.NoError(t, ensureTerminalEventSnapshots(ctx, store.DB()))
	require.NoError(t, CheckSchemaCompat(ctx, store.DB()))
}
