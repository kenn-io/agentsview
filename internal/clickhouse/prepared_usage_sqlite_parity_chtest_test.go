//go:build chtest

package clickhouse

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

// Trashing a session and restoring it changes only its deleted_at, not its
// usage. Usage, Top sessions, session counts, per-session usage, and the
// Activity report must still match the SQLite archive the mirror copies,
// both while prepared_usage holds the rows from before the change and after
// the refresh that follows. Before the refresh, a trashed session's old rows
// are kept out twice, by the replaced-session list and by the live session
// join; a restored session's rows come only from the rows reads prepare.
func TestPreparedUsageMatchesSQLiteThroughTrashAndRestore(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, syncer, store := newPreparedUsageStore(t)
	assertPreparedUsageMatchesSQLite(t, local, store, "refreshed")
	require.Contains(t, topSessionIDs(t, store), usagePriceTierID,
		"the fixture must rank the session this test trashes")

	require.NoError(t, local.SoftDeleteSession(ctx, usagePriceTierID))
	pushPreparedUsage(t, syncer)
	requirePreparedUsageBehind(t, store, usagePriceTierID, true, false)
	assertPreparedUsageMatchesSQLite(t, local, store, "trashed, before the refresh")
	assert.NotContains(t, topSessionIDs(t, store), usagePriceTierID)

	runPreparedUsageRefresh(t, store)
	assertPreparedUsageMatchesSQLite(t, local, store, "trashed, after the refresh")

	restored, err := local.RestoreSession(ctx, usagePriceTierID)
	require.NoError(t, err)
	require.EqualValues(t, 1, restored)
	pushPreparedUsage(t, syncer)
	requirePreparedUsageBehind(t, store, usagePriceTierID, false, true)
	assertPreparedUsageMatchesSQLite(t, local, store, "restored, before the refresh")
	assert.Contains(t, topSessionIDs(t, store), usagePriceTierID)

	runPreparedUsageRefresh(t, store)
	assertPreparedUsageMatchesSQLite(t, local, store, "restored, after the refresh")
}

// requirePreparedUsageBehind checks that reads take the prepared path while
// the stopped view leaves the session out of date, so the comparison that
// follows exercises the rows reads prepare themselves rather than a refresh
// or the raw rows. A trashed session's old rows are stale; a restored
// session has a new snapshot with no prepared rows, so it has changed.
func requirePreparedUsageBehind(t *testing.T, store *Store, sessionID string, stale, changed bool) {
	t.Helper()
	state, err := store.preparedUsageState(t.Context())
	require.NoError(t, err)
	require.True(t, state.ready, "prepared usage must serve the read")
	assert.Equal(t, stale, slices.Contains(state.stale, sessionID), "stale sessions %v", state.stale)
	assert.Equal(t, changed, slices.Contains(state.changed, sessionID), "changed sessions %v", state.changed)
}

func topSessionIDs(t *testing.T, store *Store) []string {
	t.Helper()
	top, err := store.GetTopSessionsByCost(t.Context(), db.UsageFilter{Timezone: "UTC"}, 10)
	require.NoError(t, err)
	ids := make([]string, 0, len(top))
	for _, entry := range top {
		ids = append(ids, entry.SessionID)
	}
	return ids
}

// assertPreparedUsageMatchesSQLite compares every usage read prepared_usage
// serves with the same read of the SQLite archive.
func assertPreparedUsageMatchesSQLite(t *testing.T, local *db.DB, store *Store, step string) {
	t.Helper()
	ctx := t.Context()
	for name, filter := range usagePriceFilters() {
		msg := step + ": " + name
		want, err := local.GetDailyUsage(ctx, filter)
		require.NoError(t, err, msg)
		got, err := store.GetDailyUsage(ctx, filter)
		require.NoError(t, err, msg)
		assert.Equal(t, dailyUsageWire(t, want, true), dailyUsageWire(t, got, false), msg)

		wantTop, err := local.GetTopSessionsByCost(ctx, filter, 10)
		require.NoError(t, err, msg)
		normalizeSQLiteTopSessions(t, wantTop)
		gotTop, err := store.GetTopSessionsByCost(ctx, filter, 10)
		require.NoError(t, err, msg)
		assert.Equal(t, wantTop, gotTop, msg)

		wantCounts, err := local.GetUsageSessionCounts(ctx, filter)
		require.NoError(t, err, msg)
		gotCounts, err := store.GetUsageSessionCounts(ctx, filter)
		require.NoError(t, err, msg)
		assert.Equal(t, wantCounts, gotCounts, msg)
	}

	for _, id := range []string{
		usagePriceRoundID, usagePriceTierID, usagePriceSnapAID,
		usagePriceSnapBID, usagePriceCopilotID, usagePriceMixedID,
	} {
		want, wantErr := local.GetSessionUsage(ctx, id, true)
		got, gotErr := store.GetSessionUsage(ctx, id, true)
		require.Equal(t, wantErr == nil, gotErr == nil, "%s: session usage %s: sqlite %v, clickhouse %v",
			step, id, wantErr, gotErr)
		if want == nil || got == nil {
			assert.Equal(t, want == nil, got == nil, "%s: session usage %s", step, id)
			continue
		}
		assert.Equal(t, want.Cost, got.Cost, "%s: session usage %s", step, id)
		assert.Equal(t, want.HasCost, got.HasCost, "%s: session usage %s", step, id)
		assert.Equal(t, want.TotalOutputTokens, got.TotalOutputTokens, "%s: session usage %s", step, id)
		assert.Equal(t, want.Models, got.Models, "%s: session usage %s", step, id)
	}

	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2026-01-12", Timezone: "UTC",
	}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	filter := db.AnalyticsFilter{Timezone: "UTC", IncludeSubagents: true}
	want, err := local.GetActivityReport(ctx, filter, q)
	require.NoError(t, err, step)
	// A fresh store builds the report from the rows. The long-lived store
	// keeps the ended day's report from the previous step, so it must
	// notice that a trash or restore changed the day.
	got, err := NewStoreFromDB(store.DB(), store.bulk).GetActivityReport(ctx, filter, q)
	require.NoError(t, err, step)
	kept, err := store.GetActivityReport(ctx, filter, q)
	require.NoError(t, err, step)
	assert.Equal(t, got, kept, "%s: the kept activity report must match a fresh one", step)
	assert.Equal(t, want.Totals.Sessions, got.Totals.Sessions, "%s: activity sessions", step)
	assert.Equal(t, want.Totals.OutputTokens, got.Totals.OutputTokens, "%s: activity output tokens", step)
	assert.Equal(t, want.Totals.Cost, got.Totals.Cost, "%s: activity cost", step)
	wantRows := map[string]activity.SessionRow{}
	for _, row := range want.BySession {
		wantRows[row.SessionID] = row
	}
	gotRows := map[string]activity.SessionRow{}
	for _, row := range got.BySession {
		gotRows[row.SessionID] = row
	}
	require.Equal(t, db.SortedKeys(wantRows), db.SortedKeys(gotRows), "%s: activity sessions", step)
	for id, ref := range wantRows {
		assert.Equal(t, ref.Cost, gotRows[id].Cost, "%s: activity cost of %s", step, id)
		assert.Equal(t, ref.OutputTokens, gotRows[id].OutputTokens, "%s: activity output of %s", step, id)
	}
}
