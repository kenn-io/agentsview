//go:build chtest

package clickhouse

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

// A day's report re-reads pairing inputs only for sessions a push changed,
// and matches a report built without the memo.
func TestActivityPairingMemoRereadsOnlyChangedSessions(t *testing.T) {
	ctx := t.Context()
	store, syncer, local := newPushedStore(t)
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"},
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	filter := db.AnalyticsFilter{Timezone: "UTC", IncludeSubagents: true}
	// The push started a refresh. If it lands between the two reports
	// below, the second one correctly reads the new prepared rows.
	_, err = store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)
	first, err := store.GetActivityReport(ctx, filter, q)
	require.NoError(t, err)
	counts := func() [3]int64 {
		return [3]int64{store.activityInputQueries.Load(), store.activityUsageQueries.Load(), store.activitySessionQueries.Load()}
	}
	reads := counts()
	require.Equal(t, [3]int64{1, 1, 1}, reads)
	second, err := store.GetActivityReport(ctx, filter, q)
	require.NoError(t, err)
	require.Equal(t, reads, counts(), "an unchanged mirror is not read again")
	require.Equal(t, first.Totals, second.Totals)

	appendMessage(t, local, fixtureAlphaID, "one more turn", "2026-01-10T12:00:00.000Z")
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)
	third, err := store.GetActivityReport(ctx, filter, q)
	require.NoError(t, err)
	require.Equal(t, [3]int64{reads[0] + 1, reads[1] + 1, reads[2] + 1}, counts(),
		"the changed session, the usage rows, and the listing are read once")
	fresh, err := NewStoreFromDB(store.DB()).GetActivityReport(ctx, filter, q)
	require.NoError(t, err)
	fresh.ReportID, third.ReportID = "", ""
	require.Equal(t, fresh, third)
}
