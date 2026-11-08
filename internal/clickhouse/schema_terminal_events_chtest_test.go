//go:build chtest

package clickhouse

import (
	"fmt"
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestClickHouseTerminalSnapshotUpdates(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// A mirror created before the view filtered tool_result_events to the
// inserted sessions keeps that view, and every session insert then reads the
// whole table. The schema step must swap in the filtered query, and the
// snapshots must not change.
func TestClickHouseTerminalSnapshotViewUpgrade(t *testing.T) {
	t.Parallel()
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	conn := store.DB()
	const unrelated = 200_000
	for _, query := range []string{
		"DROP VIEW terminal_event_snapshots_mv",
		// The definition an older release created, as a production mirror
		// still runs it.
		`CREATE MATERIALIZED VIEW terminal_event_snapshots_mv TO terminal_event_snapshots AS
   SELECT s.id AS session_id, s.push_version AS push_version, max(tre.timestamp) AS last_terminal_at,
   toUInt128(s.push_version) * 2 + 1 AS revision
   FROM sessions s LEFT JOIN tool_result_events tre ON tre.session_id = s.id AND tre.push_version = s.push_version
   AND tre.source = 'tool_execution' AND tre.status IN ('completed', 'errored')
   GROUP BY s.id, s.push_version`,
		fmt.Sprintf(`INSERT INTO tool_result_events
   (session_id,tool_call_message_ordinal,call_index,event_index,source,status,timestamp,push_version)
   SELECT concat('unrelated-', toString(number %% 1000)), number, 0, 0, 'tool_execution', 'completed',
   parseDateTime64BestEffort('2026-01-20T12:00:00Z'), 1 FROM numbers(%d)`, unrelated),
	} {
		_, err := conn.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	var database string
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT currentDatabase()").Scan(&database))
	// publish pushes one session with one terminal event and returns the
	// rows its session insert read, the view's reads included.
	publish := func(id string) uint64 {
		comment := database + "." + id
		_, err := conn.ExecContext(ctx, `INSERT INTO tool_result_events
   (session_id,tool_call_message_ordinal,call_index,event_index,source,status,timestamp,push_version)
   VALUES (?,0,0,0,'tool_execution','completed',parseDateTime64BestEffort('2026-01-11T12:00:00Z'),1)`, id)
		require.NoError(t, err)
		logged := chdriver.Context(ctx, chdriver.WithSettings(chdriver.Settings{"log_comment": comment}))
		_, err = conn.ExecContext(logged, `INSERT INTO sessions
   (id,agent,project,started_at,ended_at,message_count,push_version)
   VALUES (?,'codex',?,parseDateTime64BestEffort('2026-01-01T12:00:00Z'),
   parseDateTime64BestEffort('2026-01-01T12:30:00Z'),1,1)`, id, id)
		require.NoError(t, err)
		var last string
		require.NoError(t, conn.QueryRowContext(ctx,
			"SELECT toString(last_terminal_at) FROM terminal_event_snapshots WHERE session_id = ?", id).Scan(&last))
		assert.Equal(t, "2026-01-11 12:00:00.000000", last)
		_, err = conn.ExecContext(ctx, "SYSTEM FLUSH LOGS")
		require.NoError(t, err)
		// The connection inserts asynchronously, so the view runs in the
		// flush, which the log records as a separate entry with the comment.
		var read uint64
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT sum(read_rows) FROM system.query_log
   WHERE type = 'QueryFinish' AND log_comment = ?`, comment).Scan(&read))
		return read
	}
	stale := publish("terminal-view-stale")
	require.GreaterOrEqual(t, stale, uint64(unrelated), "the stale view must read every event, or the check below proves nothing")
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	current := publish("terminal-view-current")
	assert.Less(t, current, uint64(unrelated/10), "the updated view must read only the inserted session's events")
}
