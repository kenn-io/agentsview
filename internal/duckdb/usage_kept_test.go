//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

func TestUsageKeptSharesDailyAndTopReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	fresh := NewStoreFromDB(store.DB())
	reads := 0
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(string) { reads++ }
	t.Cleanup(func() { analyticsQueryObserver = previous })
	f := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC", Breakdowns: true}
	summary, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 500, summary.Totals.OutputTokens)
	require.Equal(t, 1, summary.SessionCounts.Total)
	require.Equal(t, 1, reads)
	f.SkipSessionCounts = true
	ranking, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 1, reads)
	require.Empty(t, ranking.SessionCounts)
	want, err := fresh.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, want, ranking)
	f.SkipSessionCounts = false
	want, err = fresh.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, want, summary)
	before := reads
	_, err = store.GetTopSessionsByCost(t.Context(), f, 1)
	require.NoError(t, err)
	require.Equal(t, before+1, reads)
	f.TopSessionsSort = "tokens"
	f.TopSessionsTokenTypes = db.UsageTokenTypeOutput
	top, err := store.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, before+1, reads)
	require.Len(t, top, 1)
	require.Equal(t, 500, top[0].OutputTokens)
	wantTop, err := fresh.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, wantTop, top)
	before = reads
	counts, err := store.GetPlanUsageSessionCounts(t.Context(), f)
	require.NoError(t, err)
	f.Breakdowns = false
	f.SkipSessionCounts = true
	again, err := store.GetPlanUsageSessionCounts(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, before+1, reads)
	require.Equal(t, counts, again)
	wantCounts, err := fresh.GetPlanUsageSessionCounts(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, wantCounts, counts)
}

func TestUsageKeptMirrorReplacementAndPricing(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	f := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
	store.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 10_000_000}})
	first, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.005"), first.Totals.TotalCost)
	_, err = store.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	_, err = store.GetPlanUsageSessionCounts(t.Context(), f)
	require.NoError(t, err)
	next := filepath.Join(dir, "next.duckdb")
	keptActivityMirror(t, next, "session-b", 900)
	require.NoError(t, swapMirrorFile(next, path))
	store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
	changed, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, 900, changed.Totals.OutputTokens)
	require.Equal(t, money.MustParseDollars("0.009"), changed.Totals.TotalCost)
	top, err := store.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Len(t, top, 1)
	require.Equal(t, "session-b", top[0].SessionID)
	store.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 20_000_000}})
	repriced, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.018"), repriced.Totals.TotalCost)
	top, err = store.GetTopSessionsByCost(t.Context(), f, 10)
	require.NoError(t, err)
	require.Equal(t, money.MustParseDollars("0.018"), top[0].Cost)
}

func TestUsageKeptBorrowedHandleReadsEveryTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	keptActivityMirror(t, path, "session-a", 500)
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	borrowed := NewStoreFromDB(store.DB())
	reads := 0
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(string) { reads++ }
	t.Cleanup(func() { analyticsQueryObserver = previous })
	for range 2 {
		result, err := borrowed.GetDailyUsage(t.Context(), db.UsageFilter{})
		require.NoError(t, err)
		require.Equal(t, 500, result.Totals.OutputTokens)
		top, err := borrowed.GetTopSessionsByCost(t.Context(), db.UsageFilter{}, 10)
		require.NoError(t, err)
		require.Len(t, top, 1)
		_, err = borrowed.GetPlanUsageSessionCounts(t.Context(), db.UsageFilter{})
		require.NoError(t, err)
	}
	require.Equal(t, 6, reads)
}
