//go:build pgtest

package postgres

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/duckdb"
)

func TestAnalyticsToolResultsAreNotUserPrompts(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })
	local := testDB(t)
	ctx := t.Context()
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour).Format(time.RFC3339)
	require.NoError(t, local.UpsertSession(db.Session{
		ID: "tool-results", Project: "project", Machine: "local", Agent: "cortex",
		StartedAt: &started, MessageCount: 6, UserMessageCount: 1,
	}))
	require.NoError(t, local.InsertMessages([]db.Message{
		{SessionID: "tool-results", Ordinal: 0, Role: "user", Model: "model-a", SourceSubtype: "tool_result", Content: "failed", Timestamp: started},
		{SessionID: "tool-results", Ordinal: 1, Role: "user", Content: "help", Timestamp: started},
		{SessionID: "tool-results", Ordinal: 2, Role: "assistant", Model: "model-a", HasToolUse: true, Timestamp: started},
		{SessionID: "tool-results", Ordinal: 3, Role: "user", SourceSubtype: "tool_result", Content: "WHY IS THIS STILL BROKEN", Timestamp: started},
		{SessionID: "tool-results", Ordinal: 4, Role: "assistant", Model: "model-a", HasToolUse: true, Timestamp: started},
		{SessionID: "tool-results", Ordinal: 5, Role: "assistant", Model: "model-a", HasToolUse: true, Timestamp: started},
	}))
	require.NoError(t, local.UpdateSessionSignals("tool-results", db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion, ShortPromptCount: 1}}))
	ps, err := New(pgURL, "agentsview", local, "analytics-test-machine", true, SyncOptions{})
	require.NoError(t, err)
	defer ps.Close()
	require.NoError(t, ps.EnsureSchema(ctx))
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err)
	defer store.Close()
	duckPath := filepath.Join(t.TempDir(), "analytics.duckdb")
	_, err = duckdb.Push(ctx, duckPath, local, "analytics-test-machine", duckdb.SyncOptions{}, true, nil)
	require.NoError(t, err)
	duckStore, err := duckdb.NewStore(duckPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, duckStore.Close()) })
	for name, backend := range map[string]interface {
		GetAnalyticsSignalSessions(context.Context, db.AnalyticsFilter, string, int) (db.SignalSessionsResponse, error)
		GetSessionActivity(context.Context, string) (*db.SessionActivityResponse, error)
		GetAnalyticsActivity(context.Context, db.AnalyticsFilter, string) (db.ActivityResponse, error)
		GetAnalyticsSessionShape(context.Context, db.AnalyticsFilter) (db.SessionShapeResponse, error)
	}{"sqlite": local, "postgres": store, "duckdb": duckStore} {
		t.Run(name, func(t *testing.T) {
			activity, err := backend.GetSessionActivity(ctx, "tool-results")
			require.NoError(t, err)
			require.Len(t, activity.Buckets, 1)
			assert.Equal(t, 1, activity.Buckets[0].UserCount)
			assert.Equal(t, 3, activity.Buckets[0].AssistantCount)
			assert.Equal(t, 6, activity.TotalMessages)
			for _, model := range []string{"", "model-a"} {
				filter := db.AnalyticsFilter{Model: model}
				report, err := backend.GetAnalyticsActivity(ctx, filter, "day")
				require.NoError(t, err)
				require.Len(t, report.Series, 1)
				assert.Equal(t, 1, report.Series[0].UserMessages, model)
				assert.Equal(t, 6, report.Series[0].Messages, model)
				shape, err := backend.GetAnalyticsSessionShape(ctx, filter)
				require.NoError(t, err)
				assert.Equal(t, 1, bucketCount(shape.AutonomyDistribution, "2-5"), model)
				evidence, err := backend.GetAnalyticsSignalSessions(ctx, filter, "short_prompt_count", 10)
				require.NoError(t, err)
				require.Len(t, evidence.Sessions, 1)
				assert.Equal(t, "help", evidence.Sessions[0].Excerpt, model)
				frustration, err := backend.GetAnalyticsSignalSessions(ctx, filter, "frustration_marker_count", 10)
				require.NoError(t, err)
				assert.Empty(t, frustration.Sessions, model)
			}
		})
	}
	stats, err := local.GetSessionStats(ctx, db.StatsFilter{Since: "28d"})
	require.NoError(t, err)
	require.Len(t, stats.Temporal.HourlyUTC, 1)
	assert.Equal(t, 1, stats.Temporal.HourlyUTC[0].UserMessages)
}
