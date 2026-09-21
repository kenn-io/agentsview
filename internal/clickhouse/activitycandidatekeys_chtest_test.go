//go:build chtest

package clickhouse

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// An older event can admit a key, but only its latest version may admit a session.
func TestActivityCandidateKeysKeepLatestEventVersion(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	for _, statement := range []string{
		`SYSTEM STOP MERGES tool_result_events`,

		`INSERT INTO tool_result_events
		 (session_id, tool_call_message_ordinal, call_index, event_index, source, status, timestamp, push_version)
		 VALUES
		 ('active',0,0,0,'tool_execution','completed','2026-01-10 12:00:00',1),
		 ('moved',0,0,0,'tool_execution','completed','2026-01-10 12:00:00',1),
		 ('pending',0,0,0,'tool_execution','completed','2026-01-10 12:00:00',1),
		 ('newer',0,0,0,'tool_execution','completed','2026-01-01 12:00:00',1),
		 ('null-time',0,0,0,'tool_execution','completed',NULL,1)`,
		`INSERT INTO tool_result_events
		 (session_id, tool_call_message_ordinal, call_index, event_index, source, status, timestamp, push_version)
		 VALUES
		 ('moved',0,0,0,'tool_execution','completed','2026-01-01 12:00:00',2),
		 ('pending',0,0,0,'tool_execution','started','2026-01-10 12:00:00',2),
		 ('newer',0,0,0,'tool_execution','completed','2026-01-10 12:00:00',2)`,
		// The pusher publishes session versions after their dependent rows.
		`INSERT INTO sessions (id, project, agent, started_at, ended_at, created_at, message_count, push_version)
		 SELECT arrayJoin(['active', 'moved', 'pending', 'newer', 'null-time']) AS id,
		 'terminal-key-fixture', 'claude', '2026-01-01 12:00:00',
		 '2026-01-01 12:01:00', '2026-01-01 12:00:00', 1, if(id IN ('moved', 'pending', 'newer'), 2, 1)`,
	} {
		_, err := store.DB().ExecContext(ctx, statement)
		require.NoError(t, err)
	}
	where, args := clickActivityReportCandidateWhere(
		db.AnalyticsFilter{Project: "terminal-key-fixture", Timezone: "UTC"},
		"2026-01-10T00:00:00Z", "2026-01-11T00:00:00Z",
	)
	_, ids, err := store.activityReportSessions(ctx, where, args)
	require.NoError(t, err)
	slices.Sort(ids)
	require.Equal(t, []string{"active", "newer"}, ids)
}
