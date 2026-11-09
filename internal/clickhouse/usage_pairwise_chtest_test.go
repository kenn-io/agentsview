//go:build chtest

package clickhouse

import (
	"strings"
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

func TestUsagePairwiseDefaultReadsRankingOnce(t *testing.T) {
	t.Parallel()
	store, syncer, _ := newUsagePriceStore(t)
	comment := "pairwise-ranking-" + syncer.target.Database
	ctx := chdriver.Context(t.Context(), chdriver.WithSettings(chdriver.Settings{"log_comment": comment}))
	backend := service.NewReadOnlyBackend(store, service.WithModelGroups(service.NewModelGroups(map[string]string{
		"round-test": "priced", "tier-test": "priced",
	})))
	req := service.UsagePairwiseComparisonRequest{UsageRequest: service.UsageRequest{From: "2026-01-12", To: "2026-01-12", Timezone: "UTC", Agent: "hermes", GroupModels: true, IncludeOneShot: true}}
	got, err := backend.UsagePairwiseComparison(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "priced", got.Left.Value)
	assert.Equal(t, 6411, got.Left.InputTokens)
	assert.Equal(t, 210, got.Left.OutputTokens)
	assert.Equal(t, 2, got.Left.SessionCount)
	assert.Equal(t, money.MustParseDollars("0.010623"), got.Left.TotalCost)
	_, err = store.conn.ExecContext(t.Context(), "SYSTEM FLUSH LOGS")
	require.NoError(t, err)
	var queries uint64
	require.NoError(t, store.conn.QueryRowContext(t.Context(), `
		SELECT count() FROM system.query_log
		WHERE type = 'QueryFinish' AND log_comment = ?
		AND startsWith(trimLeft(query, ' \n\t\r'), 'WITH usage_raw AS')
		AND position(query, 'FROM usage_classified') > 0`, comment).Scan(&queries))
	assert.Equal(t, uint64(1), queries)
	req.LeftDimension, req.LeftValue = got.Left.Dimension, got.Left.Value
	req.RightDimension, req.RightValue = got.Right.Dimension, got.Right.Value
	want, err := backend.UsagePairwiseComparison(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestUsagePairwiseSidesMatchFilteredReads(t *testing.T) {
	t.Parallel()
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "raw"}[raw], func(t *testing.T) {
			local, syncer, store := newPreparedUsageStore(t)
			if raw {
				execPreparedUsage(t, store, "TRUNCATE TABLE prepared_usage")
			}
			for _, phase := range []string{"first", "repeat", "after push", "custom"} {
				t.Run(phase, func(t *testing.T) {
					if phase == "after push" {
						require.NoError(t, local.ReplaceSessionUsageEvents(t.Context(), usagePriceRoundID, []db.UsageEvent{{
							Source: "hermes", Model: "round-test", InputTokens: 2, DedupKey: "added", OccurredAt: "2026-01-12T04:00:00Z",
						}}))
						_, err := syncer.Push(t.Context(), false, nil)
						require.NoError(t, err)
					}
					if phase == "custom" {
						store.SetCustomPricing(map[string]config.CustomModelRate{"round-test": {InputMicrodollarsPerMTok: 800_000}})
					}
					filters := usagePriceFilters()
					filters["local window"] = db.UsageFilter{Timezone: "America/New_York", From: "2026-01-11", To: "2026-01-11"}
					for name, f := range filters {
						t.Run(name, func(t *testing.T) {
							f.Breakdowns = true
							ranking, err := store.GetDailyUsage(t.Context(), f)
							require.NoError(t, err)
							require.NotNil(t, ranking.Pairwise)
							for _, selection := range []struct {
								dimension string
								values    []string
							}{
								{"model", []string{"round-test"}}, {"model", []string{"tier-test"}},
								{"model", []string{"claude-test"}}, {"model", []string{"round-test", "tier-test"}},
								{"project", []string{"gamma"}}, {"project", []string{"delta"}},
							} {
								got, ok := ranking.Pairwise.Side(selection.dimension, selection.values)
								require.True(t, ok)
								filtered := f
								filtered.Breakdowns = false
								if selection.dimension == "model" {
									filtered.Model = strings.Join(selection.values, ",")
									// A base model selection intersects the side.
									if f.HasModelFilter() && f.Model != filtered.Model {
										continue
									}
								} else {
									if f.Project != "" && f.Project != selection.values[0] {
										continue
									}
									filtered.Project, filtered.ProjectLabels = "", selection.values
								}
								want, err := store.GetDailyUsage(t.Context(), filtered)
								require.NoError(t, err)
								assert.Equal(t, want.Totals.InputTokens, got.Totals.InputTokens)
								assert.Equal(t, want.Totals.OutputTokens, got.Totals.OutputTokens)
								assert.Equal(t, want.Totals.CacheCreationTokens, got.Totals.CacheCreationTokens)
								assert.Equal(t, want.Totals.CacheReadTokens, got.Totals.CacheReadTokens)
								assert.Equal(t, want.Totals.TotalCost, got.Totals.TotalCost)
								assert.Equal(t, want.SessionCounts.Total, got.SessionCounts.Total)
							}
						})
					}
				})
			}
		})
	}
}

func TestUsagePairwiseDuplicateFallsBack(t *testing.T) {
	t.Parallel()
	store, syncer, local := newUsagePriceStore(t)
	for i, project := range []string{"winner", "loser,raw", "absent"} {
		id := project
		stamp := "2026-01-12T05:00:00Z"
		m := usagePriceMessage(id, 0, stamp, []string{"round-test", "tier-test", "only-dropped"}[i], `{"input_tokens":2000}`)
		m.SourceUUID = "shared-source"
		// Order the winner before each losing row.
		m.Timestamp = []string{stamp, "2026-01-12T05:01:00Z", "2026-01-12T05:02:00Z"}[i]
		_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: fixtureSession(id, project, "duplicate", stamp, 1), Messages: []db.Message{m}, DataVersion: 1, ReplaceMessages: true}})
		require.NoError(t, err)
	}
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	for range 2 {
		ranking, err := store.GetDailyUsage(t.Context(), db.UsageFilter{Timezone: "UTC", Breakdowns: true})
		require.NoError(t, err)
		for _, selection := range []struct{ dimension, value string }{{"model", "tier-test"}, {"model", "only-dropped"}, {"project", "loser,raw"}, {"project", "absent"}} {
			_, ok := ranking.Pairwise.Side(selection.dimension, []string{selection.value})
			assert.False(t, ok)
		}
	}
	got, err := service.NewReadOnlyBackend(store).UsagePairwiseComparison(t.Context(), service.UsagePairwiseComparisonRequest{
		UsageRequest:  service.UsageRequest{From: "2026-01-12", To: "2026-01-12", Timezone: "UTC", IncludeOneShot: true},
		LeftDimension: "model", LeftValue: "round-test", RightDimension: "model", RightValue: "tier-test", FallbackToDefault: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 8400, got.Right.InputTokens)
	assert.Equal(t, money.MustParseDollars("0.01462"), got.Right.TotalCost)
}

func TestUsagePairwiseEstimatedOverflowFallsBack(t *testing.T) {
	t.Parallel()
	store, syncer, local := newUsagePriceStore(t)
	cost := money.Money{Microdollars: 9_100_000_000_000_000_000}
	for _, id := range []string{"large-a", "large-b"} {
		session := fixtureSession(id, "large", "large", "2026-01-12T06:00:00Z", 0)
		session.Agent = "copilot"
		_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, DataVersion: 1, ReplaceMessages: true, UsageEvents: []db.UsageEvent{
			{Source: "provider", Model: "large", Cost: &cost, CostSource: "reported", OccurredAt: "2026-01-12T06:00:00Z", DedupKey: "estimate"},
			{Source: "shutdown", Model: "large", Cost: new(money.MustParseDollars("0.5")), CostSource: db.CopilotReportedCostSource, OccurredAt: "2026-01-12T06:01:00Z", DedupKey: "total"},
		}}})
		require.NoError(t, err)
	}
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	ranking, err := store.GetDailyUsage(t.Context(), db.UsageFilter{Timezone: "UTC", Project: "large", Breakdowns: true})
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("1"), ranking.Totals.TotalCost)
	_, ok := ranking.Pairwise.Side("model", []string{"large"})
	assert.False(t, ok)
	_, err = service.NewReadOnlyBackend(store).UsagePairwiseComparison(t.Context(), service.UsagePairwiseComparisonRequest{
		UsageRequest:  service.UsageRequest{From: "2026-01-12", To: "2026-01-12", Timezone: "UTC", Project: "large", IncludeOneShot: true},
		LeftDimension: "model", LeftValue: "large", RightDimension: "project", RightValue: "large", FallbackToDefault: true,
	})
	assert.ErrorIs(t, err, money.ErrOverflow)
}
