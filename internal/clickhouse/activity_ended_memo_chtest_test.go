//go:build chtest

package clickhouse

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

// The report of an ended day is kept across a push that touches no session
// with activity or usage that day, and rebuilt after one that does; either
// way it matches a report built by a store that kept nothing.
func TestEndedActivityReportSurvivesUnrelatedPushes(t *testing.T) {
	ctx := t.Context()
	store, syncer, local := newPushedStore(t)
	push := func() {
		t.Helper()
		_, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		_, err = store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
		require.NoError(t, err)
	}
	_, err := store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)
	ready, err := store.preparedUsageReady(ctx)
	require.NoError(t, err)
	require.True(t, ready)
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"},
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.False(t, q.Partial)
	filter := db.AnalyticsFilter{Timezone: "UTC", IncludeSubagents: true}
	requireFresh := func(got activity.Report) {
		t.Helper()
		fresh, err := NewStoreFromDB(store.DB()).GetActivityReport(ctx, filter, q)
		require.NoError(t, err)
		require.Equal(t, fresh, got)
	}
	first, err := store.GetActivityReport(ctx, filter, q)
	require.NoError(t, err)
	requireFresh(first)
	usageReads := store.activityUsageQueries.Load()

	// A new session two days later changes the mirror but not the day.
	const laterID = "ended-memo-later"
	later := fixtureSession(laterID, "alpha", "later first", "2026-01-12T09:00:00.000Z", 1)
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: later, DataVersion: 1, ReplaceMessages: true,
		Messages: []db.Message{usagePriceMessage(laterID, 0, "2026-01-12T09:00:00.000Z", "claude-test",
			`{"input_tokens":40,"output_tokens":4}`)},
	}})
	require.NoError(t, err)
	push()
	kept, err := store.GetActivityReport(ctx, filter, q)
	require.NoError(t, err)
	require.Equal(t, usageReads, store.activityUsageQueries.Load(), "the kept report answers")
	require.Equal(t, first, kept)
	requireFresh(kept)

	// A turn on the day itself rebuilds the report.
	appendMessage(t, local, fixtureAlphaID, "one more turn", "2026-01-10T12:00:00.000Z")
	push()
	rebuilt, err := store.GetActivityReport(ctx, filter, q)
	require.NoError(t, err)
	require.Greater(t, store.activityUsageQueries.Load(), usageReads)
	require.NotEqual(t, first, rebuilt)
	requireFresh(rebuilt)
}

// A store that starts after another kept an ended day's report on disk
// answers from the file without reading the day's rows, and rebuilds it
