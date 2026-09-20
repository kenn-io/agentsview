//go:build chtest

package clickhouse

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
)

func TestStoreAnalyticsReads(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := context.Background()
	filter := db.AnalyticsFilter{
		From: "2026-01-01",
		To:   "2026-01-31",
	}

	t.Run("summary_includes_subagents", func(t *testing.T) {
		summary, err := store.GetAnalyticsSummary(ctx, filter)
		require.NoError(t, err)
		assert.Equal(t, 3, summary.TotalSessions)
		assert.Equal(t, 4, summary.TotalMessages)
	})

	t.Run("activity_day_buckets", func(t *testing.T) {
		activity, err := store.GetAnalyticsActivity(ctx, filter, "day")
		require.NoError(t, err)
		byDate := map[string]db.ActivityEntry{}
		for _, entry := range activity.Series {
			byDate[entry.Date] = entry
		}
		_, has10 := byDate["2026-01-10"]
		_, has11 := byDate["2026-01-11"]
		assert.True(t, has10, "activity should cover 2026-01-10")
		assert.True(t, has11, "activity should cover 2026-01-11")
		if entry, ok := byDate["2026-01-10"]; ok {
			assert.Equal(t, 1, entry.Sessions)
			assert.Equal(t, 2, entry.Messages)
		}
		if entry, ok := byDate["2026-01-11"]; ok {
			assert.Equal(t, 1, entry.Sessions)
			assert.Equal(t, 1, entry.Messages)
		}
	})

	t.Run("heatmap_has_a_day", func(t *testing.T) {
		heatmap, err := store.GetAnalyticsHeatmap(ctx, filter, "messages")
		require.NoError(t, err)
		require.NotEmpty(t, heatmap.Entries)
		found := false
		for _, entry := range heatmap.Entries {
			if entry.Value > 0 {
				found = true
				break
			}
		}
		assert.True(t, found, "heatmap should have at least one day with messages")
	})

	t.Run("tools_lists_search", func(t *testing.T) {
		tools, err := store.GetAnalyticsTools(ctx, filter)
		require.NoError(t, err)
		names := make([]string, 0, len(tools.ByTool))
		for _, tool := range tools.ByTool {
			names = append(names, tool.ToolName)
		}
		assert.Contains(t, names, "search")
	})

	t.Run("top_sessions_by_messages", func(t *testing.T) {
		top, err := store.GetAnalyticsTopSessions(ctx, filter, "messages")
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(top.Sessions), 2)
		assert.Equal(t, fixtureAlphaID, top.Sessions[0].ID)
		assert.Equal(t, fixtureBetaID, top.Sessions[1].ID)
		assert.Equal(t, 2, top.Sessions[0].MessageCount)
		assert.Equal(t, 1, top.Sessions[1].MessageCount)
		require.NotNil(t, top.Sessions[0].DisplayName)
		assert.Equal(t, "Alpha Saved Title", *top.Sessions[0].DisplayName)
	})

	t.Run("signals_aggregate", func(t *testing.T) {
		signals, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{
			From:             "2026-01-01",
			To:               "2026-01-31",
			IncludeSubagents: true,
		})
		require.NoError(t, err)
		assert.Equal(t, 3, signals.OutcomeDistribution["success"],
			"alpha, beta, and the alpha subagent are seeded with outcome success")
	})

	t.Run("trends_term_clickhouse", func(t *testing.T) {
		terms, err := db.ParseTrendTerms([]string{"clickhouse"})
		require.NoError(t, err)
		trends, err := store.GetTrendsTerms(ctx, filter, terms, "week")
		require.NoError(t, err)
		require.NotEmpty(t, trends.Series)
		assert.GreaterOrEqual(t, trends.Series[0].Total, 1)
	})
}

func TestGetAnalyticsSummaryEmptyMedian(t *testing.T) {
	ctx := context.Background()
	filter := db.AnalyticsFilter{
		From: "2026-01-01",
		To:   "2026-01-31",
	}

	t.Run("empty_mirror", func(t *testing.T) {
		dsn, database := chtest.FreshDatabase(t)
		require.NoError(t, EnsureSchema(ctx, Target{URL: dsn, Database: database}))
		store, err := NewStore(ctx, Target{URL: dsn, Database: database})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })

		summary, err := store.GetAnalyticsSummary(ctx, filter)
		require.NoError(t, err)
		assert.Equal(t, 0, summary.TotalSessions)
		assert.Equal(t, 0, summary.TotalMessages)
		assert.Equal(t, 0, summary.MedianMessages)
		assert.Equal(t, 0.0, summary.AvgMessages)
	})

	t.Run("unmatched_filter", func(t *testing.T) {
		store, _, _ := newPushedStore(t)
		summary, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
			From: "2025-01-01",
			To:   "2025-01-31",
		})
		require.NoError(t, err)
		assert.Equal(t, 0, summary.TotalSessions)
		assert.Equal(t, 0, summary.TotalMessages)
		assert.Equal(t, 0, summary.MedianMessages)
		assert.Equal(t, 0.0, summary.AvgMessages)
	})

	t.Run("populated_median", func(t *testing.T) {
		store, _, _ := newPushedStore(t)
		summary, err := store.GetAnalyticsSummary(ctx, filter)
		require.NoError(t, err)
		assert.Equal(t, 3, summary.TotalSessions)
		assert.Equal(t, 4, summary.TotalMessages)
		// Sorted message counts: 1, 1, 2.
		assert.Equal(t, 1, summary.MedianMessages)
	})
}

// TestAnalyticsSessionSetQueriesMatchSQLite covers the reads that select
// their sessions with a SQL subquery rather than an ID list: session shape
// autonomy buckets and velocity messages and tool counts.
func TestAnalyticsSessionSetQueriesMatchSQLite(t *testing.T) {
	store, _, local := newPushedStore(t)
	ctx := t.Context()
	filter := db.AnalyticsFilter{
		From:     "2026-01-01",
		To:       "2026-01-31",
		Timezone: "UTC",
	}

	wantShape, err := local.GetAnalyticsSessionShape(ctx, filter)
	require.NoError(t, err)
	gotShape, err := store.GetAnalyticsSessionShape(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, wantShape.Count, gotShape.Count)
	assert.Equal(t, wantShape.AutonomyDistribution, gotShape.AutonomyDistribution)
	assert.NotEmpty(t, gotShape.AutonomyDistribution,
		"fixture sessions have user messages, so autonomy buckets must be populated")

	wantVelocity, err := local.GetAnalyticsVelocity(ctx, filter)
	require.NoError(t, err)
	gotVelocity, err := store.GetAnalyticsVelocity(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, wantVelocity, gotVelocity)
	assert.NotEmpty(t, gotVelocity.ByAgent,
		"fixture sessions have paired user and assistant messages, so velocity must have data")
}
