//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// After a push changes the mirror, warming re-reads the day a client asked
// for, so the client's next request is answered from the memo and still
// matches the local archive.
func TestUsageWarmerRefillsRecentReads(t *testing.T) {
	ctx := t.Context()
	store, syncer, local := newUsagePriceStore(t)
	_, err := store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)
	filter := db.UsageFilter{Timezone: "UTC", Breakdowns: true}
	_, err = store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	_, err = store.GetTopSessionsByCost(ctx, filter, 5)
	require.NoError(t, err)
	queries := store.usageReadQueries.Load()
	_, err = store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, queries, store.usageReadQueries.Load(), "a repeated read is served from the memo")
	// A warm with unchanged parts reads nothing.
	require.NoError(t, store.warmUsageReads(ctx))
	require.NoError(t, store.warmUsageReads(ctx))
	require.Equal(t, queries, store.usageReadQueries.Load())

	appendMessage(t, local, usagePriceRoundID, "another turn", "2026-01-12T09:00:00.000Z")
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)
	require.NoError(t, store.warmUsageReads(ctx))
	warmed := store.usageReadQueries.Load()
	require.Equal(t, queries+2, warmed, "the day's summary and top sessions are re-read once each")
	got, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, warmed, store.usageReadQueries.Load(), "the client's read is answered from the warm memo")
	want, err := local.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, dailyUsageWire(t, want, true), dailyUsageWire(t, got, false))
}
