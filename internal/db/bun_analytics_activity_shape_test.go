package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBunAnalyticsActivityAggregatesPairedMessagesInOneQuery(t *testing.T) {
	database := testDB(t)
	started := "2026-08-04T09:00:00Z"
	ended := "2026-08-04T10:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "aggregate-activity", Project: "aggregate", Machine: "host",
		Agent: "codex", CreatedAt: started, StartedAt: &started, EndedAt: &ended,
		MessageCount: 4, UserMessageCount: 2,
	}))
	require.NoError(t, database.InsertMessages([]Message{
		{SessionID: "aggregate-activity", Ordinal: 0, Role: "user",
			Content: "selected", Timestamp: started},
		{SessionID: "aggregate-activity", Ordinal: 1, Role: "assistant",
			Content: "selected reply", Timestamp: "2026-08-04T09:01:00Z",
			Model: "gpt-5", HasThinking: true, HasToolUse: true,
			ToolCalls: []ToolCall{{ToolName: "Read", Category: "Read"}}},
		{SessionID: "aggregate-activity", Ordinal: 2, Role: "user",
			Content: "other", Timestamp: "2026-08-04T09:02:00Z"},
		{SessionID: "aggregate-activity", Ordinal: 3, Role: "assistant",
			Content: "other reply", Timestamp: "2026-08-04T09:03:00Z",
			Model: "other", HasToolUse: true,
			ToolCalls: []ToolCall{{ToolName: "Grep", Category: "Grep"}}},
	}))

	hook := new(countingQueryHook)
	database.bunReader = database.bunReader.WithQueryHook(hook)
	result, err := database.GetAnalyticsActivity(t.Context(), AnalyticsFilter{
		Project: "aggregate", Model: "gpt-5", From: "2026-08-04",
		To: "2026-08-04", Timezone: "UTC",
	}, "day")
	require.NoError(t, err)
	require.Len(t, result.Series, 1)
	assert.Equal(t, ActivityEntry{
		Date: "2026-08-04", Sessions: 1, Messages: 2, UserMessages: 1,
		AssistantMessages: 1, ToolCalls: 1, ThinkingMessages: 1,
		ByAgent: map[string]int{"codex": 2},
	}, result.Series[0])
	assert.Equal(t, 1, hook.selects,
		"activity should receive grouped facts instead of hydrated sessions and messages")
	require.Len(t, hook.queries, 1)
	assert.Contains(t, hook.queries[0], "GROUP BY")
}

func TestBunAnalyticsSessionShapeAggregatesPairedMessagesInOneQuery(t *testing.T) {
	database := testDB(t)
	started := "2026-08-04T09:00:00Z"
	ended := "2026-08-04T10:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "aggregate-shape", Project: "aggregate", Machine: "host",
		Agent: "codex", CreatedAt: started, StartedAt: &started, EndedAt: &ended,
		MessageCount: 6, UserMessageCount: 4,
	}))
	require.NoError(t, database.InsertMessages([]Message{
		{SessionID: "aggregate-shape", Ordinal: 0, Role: "user", Content: "q"},
		{SessionID: "aggregate-shape", Ordinal: 1, Role: "assistant",
			Content: "a", Model: "gpt-5", HasToolUse: true},
		{SessionID: "aggregate-shape", Ordinal: 2, Role: "user",
			Content: "other q1", Model: "other"},
		{SessionID: "aggregate-shape", Ordinal: 3, Role: "user",
			Content: "other q2", Model: "other"},
		{SessionID: "aggregate-shape", Ordinal: 4, Role: "user",
			Content: "other q3", Model: "other"},
		{SessionID: "aggregate-shape", Ordinal: 5, Role: "assistant",
			Content: "other a", Model: "other"},
	}))

	hook := new(countingQueryHook)
	database.bunReader = database.bunReader.WithQueryHook(hook)
	result, err := database.GetAnalyticsSessionShape(t.Context(), AnalyticsFilter{
		Project: "aggregate", Model: "gpt-5", From: "2026-08-04",
		To: "2026-08-04", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Count)
	assert.Equal(t, map[string]int{"1-5": 1}, bucketMap(result.LengthDistribution))
	assert.Equal(t, map[string]int{"1-2": 1}, bucketMap(result.AutonomyDistribution))
	assert.Equal(t, map[string]int{"1-2h": 1}, bucketMap(result.DurationDistribution))
	assert.Equal(t, 1, hook.selects,
		"session shape should receive per-session aggregates instead of hydrated messages")
	require.Len(t, hook.queries, 1)
	assert.Contains(t, hook.queries[0], "GROUP BY")
}
