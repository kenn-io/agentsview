package kept_test

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/service"
)

func TestUsageDailySharesCountsAndCopies(t *testing.T) {
	for _, skipFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "summary first", true: "ranking first"}[skipFirst], func(t *testing.T) {
			var usage kept.Usage
			loads := 0
			load := func(_ context.Context, f db.UsageFilter) (db.DailyUsageResult, error) {
				loads++
				result := db.DailyUsageResult{Daily: []db.DailyUsageEntry{{Date: "2026-01-01", ModelsUsed: []string{"model-a"}, ModelBreakdowns: []db.ModelBreakdown{{ModelName: "model-a", OutputTokens: 500}}}}}
				if !f.SkipSessionCounts {
					result.SessionCounts = db.UsageSessionCounts{Total: 1, ByProject: map[string]int{"project-a": 1}, ByAgent: map[string]int{"claude": 1}}
				}
				return result, nil
			}
			f := db.UsageFilter{SkipSessionCounts: skipFirst}
			first, err := usage.Daily(t.Context(), f, "v1", load)
			require.NoError(t, err)
			require.NoError(t, service.NewModelGroups(map[string]string{"model-a": "group"}).GroupDaily(first.Daily))
			if first.SessionCounts.ByProject != nil {
				first.SessionCounts.ByProject["project-a"] = 99
			}
			f.SkipSessionCounts = !skipFirst
			second, err := usage.Daily(t.Context(), f, "v1", load)
			require.NoError(t, err)
			require.Equal(t, 1, loads)
			require.Equal(t, "model-a", second.Daily[0].ModelsUsed[0])
			require.Equal(t, "model-a", second.Daily[0].ModelBreakdowns[0].ModelName)
			fresh, err := load(t.Context(), f)
			require.NoError(t, err)
			require.Equal(t, fresh, second)
			_, err = usage.Daily(t.Context(), f, "v2", load)
			require.NoError(t, err)
			require.Equal(t, 3, loads)
		})
	}
}

func TestUsageKeysAndBypass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		filter  db.UsageFilter
		want    int
	}{
		{"same", "v1", db.UsageFilter{}, 1},
		{"top options ignored", "v1", db.UsageFilter{TopSessionsSort: "tokens", TopSessionsByGroup: true, TopSessionsTokenTypes: db.UsageTokenTypeOutput}, 1},
		{"aliases retained", "v1", db.UsageFilter{ModelAliases: map[string]string{"model-a": "group", "model-b": "group"}}, 2},
		{"range retained", "v1", db.UsageFilter{From: "2026-01-02"}, 2},
		{"timezone retained", "v1", db.UsageFilter{Timezone: "America/New_York"}, 2},
		{"active bypass", "v1", db.UsageFilter{Termination: "active"}, 3},
		{"empty version", "", db.UsageFilter{}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage kept.Usage
			loads := 0
			load := func(context.Context, db.UsageFilter) (db.DailyUsageResult, error) {
				loads++
				return db.DailyUsageResult{Totals: db.UsageTotals{OutputTokens: 500}}, nil
			}
			_, err := usage.Daily(t.Context(), db.UsageFilter{}, "v1", load)
			require.NoError(t, err)
			for range 2 {
				result, err := usage.Daily(t.Context(), tc.filter, tc.version, load)
				require.NoError(t, err)
				require.Equal(t, 500, result.Totals.OutputTokens)
			}
			require.Equal(t, tc.want, loads)
		})
	}
}

func TestUsagePlanAndTopTotals(t *testing.T) {
	var usage kept.Usage
	planLoads, topLoads := 0, 0
	plan := func(context.Context, db.UsageFilter) (db.PlanUsageSessionCounts, error) {
		planLoads++
		return db.PlanUsageSessionCounts{PlanOnly: 1, PlanOnlyByModel: map[string]int{"model-a": 1}}, nil
	}
	top := func(context.Context, db.UsageFilter) ([]db.TopSessionEntry, error) {
		topLoads++
		return []db.TopSessionEntry{{SessionID: "small", TotalTokens: 1, OutputTokens: 1, SessionName: "original"}, {SessionID: "large", TotalTokens: 10, OutputTokens: 10}}, nil
	}
	for range 2 {
		counts, err := usage.Plan(t.Context(), db.UsageFilter{}, "v1", plan)
		require.NoError(t, err)
		require.Equal(t, 1, counts.PlanOnlyByModel["model-a"])
		counts.PlanOnlyByModel["model-a"] = 9
		rows, err := usage.TopTotals(t.Context(), db.UsageFilter{}, "v1", top)
		require.NoError(t, err)
		require.Equal(t, "small", rows[0].SessionID)
		require.Equal(t, "original", rows[0].SessionName)
		db.SortAndLimitTopSessions(rows, 1, "tokens", 0)
	}
	f := db.UsageFilter{Breakdowns: true, SkipSessionCounts: true, TopSessionsSort: "tokens"}
	_, err := usage.Plan(t.Context(), f, "v1", plan)
	require.NoError(t, err)
	_, err = usage.TopTotals(t.Context(), f, "v1", top)
	require.NoError(t, err)
	require.Equal(t, 1, planLoads)
	require.Equal(t, 1, topLoads)
	_, err = usage.Plan(t.Context(), f, "v2", plan)
	require.NoError(t, err)
	_, err = usage.TopTotals(t.Context(), f, "v2", top)
	require.NoError(t, err)
	require.Equal(t, 2, planLoads)
	require.Equal(t, 2, topLoads)
	usage.Clear()
	_, err = usage.Plan(t.Context(), f, "v2", plan)
	require.NoError(t, err)
	_, err = usage.TopTotals(t.Context(), f, "v2", top)
	require.NoError(t, err)
	require.Equal(t, 3, planLoads)
	require.Equal(t, 3, topLoads)
}

func TestUsageConcurrentCancellation(t *testing.T) {
	for _, cancelBoth := range []bool{false, true} {
		t.Run(map[bool]string{false: "one leaves", true: "both leave"}[cancelBoth], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var usage kept.Usage
				var loads atomic.Int32
				release, canceled := make(chan struct{}), make(chan struct{})
				load := func(ctx context.Context, f db.UsageFilter) (db.DailyUsageResult, error) {
					loads.Add(1)
					select {
					case <-release:
						return db.DailyUsageResult{Totals: db.UsageTotals{OutputTokens: 500}}, nil
					case <-ctx.Done():
						close(canceled)
						return db.DailyUsageResult{}, ctx.Err()
					}
				}
				first, cancelFirst := context.WithCancel(t.Context())
				defer cancelFirst()
				second, cancelSecond := context.WithCancel(t.Context())
				defer cancelSecond()
				done := make(chan error, 2)
				go func() { _, err := usage.Daily(first, db.UsageFilter{}, "v1", load); done <- err }()
				synctest.Wait()
				go func() {
					result, err := usage.Daily(second, db.UsageFilter{SkipSessionCounts: true}, "v1", load)
					if !cancelBoth {
						require.Equal(t, 500, result.Totals.OutputTokens)
					}
					done <- err
				}()
				synctest.Wait()
				cancelFirst()
				require.ErrorIs(t, <-done, context.Canceled)
				if cancelBoth {
					cancelSecond()
					require.ErrorIs(t, <-done, context.Canceled)
					<-canceled
				} else {
					close(release)
					require.NoError(t, <-done)
				}
				require.Equal(t, int32(1), loads.Load())
			})
		})
	}
}
