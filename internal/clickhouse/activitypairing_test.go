//go:build chtest

package clickhouse

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// The next message is selected by ordinal among timestamps after the event.
// Choosing the closest timestamp instead silently changes tool intervals.
func TestActivityPairingWithReversedMessageTimes(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	for _, query := range []string{
		`INSERT INTO messages (session_id,ordinal,timestamp,role,model,push_version) VALUES
		('pair-order',0,'2026-01-10 12:00:00','user','',1),
		('pair-order',1,'2026-01-10 12:10:00','assistant','future-model',1),
		('pair-order',2,'2026-01-10 12:03:00','assistant','backwards-model',1),
		('pair-order',3,'2026-01-10 12:12:00','user','',1)`,
		`INSERT INTO tool_result_events (session_id,tool_call_message_ordinal,call_index,event_index,source,status,timestamp,push_version) VALUES
		('pair-order',0,0,0,'tool_execution','completed','2026-01-10 12:01:00',1),
		('pair-order',2,0,0,'tool_execution','completed','2026-01-10 12:04:00',1)`,
	} {
		_, err := store.DB().ExecContext(ctx, query)
		require.NoError(t, err)
	}
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	var got []activity.IntervalCandidate
	require.NoError(t, store.ActivityReportCandidateSource([]string{"pair-order"}, q)(ctx, func(c activity.IntervalCandidate) error {
		got = append(got, c)
		return nil
	}))
	at := func(minute int) time.Time { return time.Date(2026, 1, 10, 12, minute, 0, 0, time.UTC) }
	require.Equal(t, []activity.IntervalCandidate{
		{SessionID: "pair-order", StartOrdinal: 0, EndOrdinal: 1, Start: at(0), End: at(10), ClosingRole: "assistant", ClosingModel: "future-model", PriorModel: "unknown"},
		{SessionID: "pair-order", StartOrdinal: 0, EndOrdinal: 2, Start: at(1), End: at(4), ClosingRole: "tool", PriorModel: "unknown"},
		{SessionID: "pair-order", StartOrdinal: 2, EndOrdinal: 3, Start: at(3), End: at(12), ClosingRole: "user", PriorModel: "future-model"},
		{SessionID: "pair-order", StartOrdinal: 2, EndOrdinal: 3, Start: at(4), End: at(12), ClosingRole: "user", PriorModel: "backwards-model"},
		{SessionID: "pair-order", StartOrdinal: 1, EndOrdinal: 2, Start: at(10), End: at(3), ClosingRole: "assistant", ClosingModel: "backwards-model", PriorModel: "future-model"},
	}, got)
}
