//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestStoreGetAnalyticsActivityModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_activity_model_messages_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'activity-model-messages-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:30:00Z'::timestamptz,
			2, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('activity-model-messages-001', 0, 'user', 'q',
			 '2024-06-01T09:00:00Z'::timestamptz, 1, FALSE, 'gpt-4o'),
			('activity-model-messages-001', 1, 'assistant', 'a',
			 '2024-06-01T09:05:00Z'::timestamptz, 1, FALSE, 'claude-3-5-sonnet')`)
	require.NoError(t, err, "insert messages")

	resp, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "day")
	require.NoError(t, err, "GetAnalyticsActivity")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Sessions, "Sessions")
	assert.Equal(t, 1, resp.Series[0].Messages, "Messages")
	assert.Equal(t, 1, resp.Series[0].UserMessages, "UserMessages")
	assert.Equal(t, 0, resp.Series[0].AssistantMessages,
		"AssistantMessages")
}

func TestStoreGetAnalyticsActivityModelFilterKeepsNullTimestampSessionsWithoutTimeFilter(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_activity_model_null_ts_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'activity-model-null-ts-001', 'test-machine', 'alpha', 'gpt',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:30:00Z'::timestamptz,
			2, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('activity-model-null-ts-001', 0, 'user', 'q',
			 '2024-06-01T09:00:00Z'::timestamptz, 1, FALSE, ''),
			('activity-model-null-ts-001', 1, 'assistant', 'a',
			 NULL, 1, FALSE, 'gpt-4o')`)
	require.NoError(t, err, "insert messages")

	resp, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "day")
	require.NoError(t, err, "GetAnalyticsActivity")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Sessions, "Sessions")
	assert.Equal(t, 2, resp.Series[0].Messages, "Messages")
	assert.Equal(t, 1, resp.Series[0].UserMessages, "UserMessages")
	assert.Equal(t, 1, resp.Series[0].AssistantMessages,
		"AssistantMessages")
}

func TestStoreGetAnalyticsActivityModelAndHourFilterUseSameMessage(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_activity_model_hour_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'activity-model-hour-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T10:30:00Z'::timestamptz,
			2, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('activity-model-hour-001', 0, 'user', 'q',
			 '2024-06-01T09:00:00Z'::timestamptz, 1, FALSE, 'gpt-4o'),
			('activity-model-hour-001', 1, 'assistant', 'a',
			 '2024-06-01T10:00:00Z'::timestamptz, 1, FALSE, 'claude-3-5-sonnet')`)
	require.NoError(t, err, "insert messages")

	hour := 10
	resp, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "day")
	require.NoError(t, err, "GetAnalyticsActivity")
	assert.Empty(t, resp.Series, "Series")
}

func TestStoreGetAnalyticsActivityModelAndHourFilterKeepsPairedUserTurn(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_activity_model_hour_paired_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'activity-model-hour-paired-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T10:30:00Z'::timestamptz,
			2, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('activity-model-hour-paired-001', 0, 'user', 'q',
			 '2024-06-01T09:00:00Z'::timestamptz, 1, FALSE, ''),
			('activity-model-hour-paired-001', 1, 'assistant', 'a',
			 '2024-06-01T10:00:00Z'::timestamptz, 1, FALSE, 'gpt-4o')`)
	require.NoError(t, err, "insert messages")

	// The paired empty-model user turn sits in hour 9; the gpt-4o assistant in
	// hour 10. Filtering by hour 9 must keep the session via the user turn.
	hour := 9
	resp, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "day")
	require.NoError(t, err, "GetAnalyticsActivity")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Sessions, "Sessions")
	assert.Equal(t, 1, resp.Series[0].Messages, "Messages")
	assert.Equal(t, 1, resp.Series[0].UserMessages, "UserMessages")
	assert.Equal(t, 0, resp.Series[0].AssistantMessages,
		"AssistantMessages")
}

func TestStoreGetAnalyticsHeatmapSessionsModelAndHourFilterKeepsPairedUserTurn(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_heatmap_sessions_paired_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'heatmap-sessions-paired-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T10:30:00Z'::timestamptz,
			2, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('heatmap-sessions-paired-001', 0, 'user', 'q',
			 '2024-06-01T09:00:00Z'::timestamptz, 1, FALSE, ''),
			('heatmap-sessions-paired-001', 1, 'assistant', 'a',
			 '2024-06-01T10:00:00Z'::timestamptz, 1, FALSE, 'gpt-4o')`)
	require.NoError(t, err, "insert messages")

	// Empty-model user turn at hour 9 paired with a gpt-4o assistant at hour
	// 10. The sessions heatmap must keep the session via the paired user turn.
	hour := 9
	resp, err := store.GetAnalyticsHeatmap(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "sessions")
	require.NoError(t, err, "GetAnalyticsHeatmap")
	require.Len(t, resp.Entries, 1, "len(Entries)")
	assert.Equal(t, 1, resp.Entries[0].Value, "Value")
}

func TestStoreGetAnalyticsTopSessionsDurationModelAndHourFilterKeepsPairedUserTurn(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_top_duration_paired_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'top-duration-paired-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T10:00:00Z'::timestamptz,
			2, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('top-duration-paired-001', 0, 'user', 'q',
			 '2024-06-01T09:00:00Z'::timestamptz, 1, FALSE, ''),
			('top-duration-paired-001', 1, 'assistant', 'a',
			 '2024-06-01T10:00:00Z'::timestamptz, 1, FALSE, 'gpt-4o')`)
	require.NoError(t, err, "insert messages")

	// Empty-model user turn at hour 9 paired with a gpt-4o assistant at hour
	// 10. Ranking by duration under the gpt-4o + hour-9 filter must keep the
	// session via the paired user turn.
	hour := 9
	resp, err := store.GetAnalyticsTopSessions(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "duration")
	require.NoError(t, err, "GetAnalyticsTopSessions")
	require.Len(t, resp.Sessions, 1, "len(Sessions)")
	assert.Equal(t, "top-duration-paired-001", resp.Sessions[0].ID, "ID")
}

func TestStoreGetAnalyticsActivityModelAndHourFilterCountsOnlyMatchingHourRows(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_activity_model_hour_rows_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'activity-model-hour-rows-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T10:30:00Z'::timestamptz,
			2, 0
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('activity-model-hour-rows-001', 0, 'assistant', 'read',
			 '2024-06-01T09:00:00Z'::timestamptz, 4, FALSE, 'gpt-4o'),
			('activity-model-hour-rows-001', 1, 'assistant', 'grep',
			 '2024-06-01T10:00:00Z'::timestamptz, 4, FALSE, 'gpt-4o')`)
	require.NoError(t, err, "insert messages")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO tool_calls (
			session_id, tool_name, category, call_index,
			message_ordinal
		) VALUES
			('activity-model-hour-rows-001', 'Read', 'Read', 0, 0),
			('activity-model-hour-rows-001', 'Bash', 'Bash', 1, 0),
			('activity-model-hour-rows-001', 'Grep', 'Grep', 0, 1)`)
	require.NoError(t, err, "insert tool_calls")

	hour := 10
	resp, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "day")
	require.NoError(t, err, "GetAnalyticsActivity")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Sessions, "Sessions")
	assert.Equal(t, 1, resp.Series[0].Messages, "Messages")
	assert.Equal(t, 1, resp.Series[0].AssistantMessages,
		"AssistantMessages")
	assert.Equal(t, 1, resp.Series[0].ToolCalls, "ToolCalls")
}

func TestStoreGetAnalyticsHourOfWeekModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_hour_of_week_model_messages_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'hour-of-week-model-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T10:30:00Z'::timestamptz,
			2, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('hour-of-week-model-001', 0, 'user', 'q',
			 '2024-06-01T09:00:00Z'::timestamptz, 1, FALSE, 'gpt-4o'),
			('hour-of-week-model-001', 1, 'assistant', 'a',
			 '2024-06-01T10:00:00Z'::timestamptz, 1, FALSE, 'claude-3-5-sonnet')`)
	require.NoError(t, err, "insert messages")

	resp, err := store.GetAnalyticsHourOfWeek(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsHourOfWeek")
	assert.Equal(t, 1, findPGHOWCell(resp.Cells, 5, 9), "Sat 09:00")
	assert.Equal(t, 0, findPGHOWCell(resp.Cells, 5, 10), "Sat 10:00")
}

func TestStoreGetAnalyticsHourOfWeekModelFilterIncludesPairedUserTurns(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_hour_of_week_model_paired_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'hour-of-week-paired-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T10:30:00Z'::timestamptz,
			2, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('hour-of-week-paired-001', 0, 'user', 'q',
			 '2024-06-01T09:00:00Z'::timestamptz, 1, FALSE, ''),
			('hour-of-week-paired-001', 1, 'assistant', 'a',
			 '2024-06-01T10:00:00Z'::timestamptz, 1, FALSE, 'gpt-4o')`)
	require.NoError(t, err, "insert messages")

	resp, err := store.GetAnalyticsHourOfWeek(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsHourOfWeek")
	assert.Equal(t, 1, findPGHOWCell(resp.Cells, 5, 9),
		"paired empty-model user turn at Sat 09:00")
	assert.Equal(t, 1, findPGHOWCell(resp.Cells, 5, 10),
		"selected-model assistant at Sat 10:00")
}

func TestStoreGetAnalyticsToolsModelFilterCountsOnlyMatchingToolCalls(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_tools_model_calls_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'tools-model-calls-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:30:00Z'::timestamptz,
			2, 0
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('tools-model-calls-001', 0, 'assistant', 'read',
			 '2024-06-01T09:00:00Z'::timestamptz, 4, FALSE, 'gpt-4o'),
			('tools-model-calls-001', 1, 'assistant', 'grep',
			 '2024-06-01T09:05:00Z'::timestamptz, 4, FALSE, 'claude-3-5-sonnet')`)
	require.NoError(t, err, "insert messages")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO tool_calls (
			session_id, tool_name, category, call_index,
			message_ordinal
		) VALUES
			('tools-model-calls-001', 'Read', 'Read', 0, 0),
			('tools-model-calls-001', 'Bash', 'Bash', 1, 0),
			('tools-model-calls-001', 'Grep', 'Grep', 0, 1)`)
	require.NoError(t, err, "insert tool_calls")

	resp, err := store.GetAnalyticsTools(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsTools")
	assert.Equal(t, 2, resp.TotalCalls, "TotalCalls")
	require.Len(t, resp.ByCategory, 2, "len(ByCategory)")

	catMap := make(map[string]int)
	for _, c := range resp.ByCategory {
		catMap[c.Category] = c.Count
	}
	assert.Equal(t, 1, catMap["Read"], "Read")
	assert.Equal(t, 1, catMap["Bash"], "Bash")
	assert.Zero(t, catMap["Grep"], "Grep")

	toolMap := make(map[string]int)
	for _, tool := range resp.ByTool {
		toolMap[tool.ToolName] = tool.CallCount
	}
	assert.Equal(t, 1, toolMap["Read"], "Read tool")
	assert.Equal(t, 1, toolMap["Bash"], "Bash tool")
	assert.Zero(t, toolMap["Grep"], "Grep tool")
}

func TestStoreGetAnalyticsToolsModelAndHourFilterCountsOnlyMatchingHourToolCalls(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_tools_model_hour_calls_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'tools-model-hour-calls-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T10:30:00Z'::timestamptz,
			2, 0
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('tools-model-hour-calls-001', 0, 'assistant', 'read',
			 '2024-06-01T09:00:00Z'::timestamptz, 4, FALSE, 'gpt-4o'),
			('tools-model-hour-calls-001', 1, 'assistant', 'grep',
			 '2024-06-01T10:00:00Z'::timestamptz, 4, FALSE, 'gpt-4o')`)
	require.NoError(t, err, "insert messages")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO tool_calls (
			session_id, tool_name, category, call_index,
			message_ordinal
		) VALUES
			('tools-model-hour-calls-001', 'Read', 'Read', 0, 0),
			('tools-model-hour-calls-001', 'Bash', 'Bash', 1, 0),
			('tools-model-hour-calls-001', 'Grep', 'Grep', 0, 1)`)
	require.NoError(t, err, "insert tool_calls")

	hour := 10
	resp, err := store.GetAnalyticsTools(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	})
	require.NoError(t, err, "GetAnalyticsTools")
	assert.Equal(t, 1, resp.TotalCalls, "TotalCalls")
	require.Len(t, resp.ByCategory, 1, "len(ByCategory)")
	assert.Equal(t, "Grep", resp.ByCategory[0].Category, "Category")
	assert.Equal(t, 1, resp.ByCategory[0].Count, "Count")
	require.Len(t, resp.ByTool, 1, "len(ByTool)")
	assert.Equal(t, "Grep", resp.ByTool[0].ToolName, "ToolName")
	assert.Equal(t, 1, resp.ByTool[0].CallCount, "CallCount")
	assert.Equal(t, 1, resp.ByTool[0].SessionCount, "SessionCount")
}

func TestStoreGetAnalyticsSkillsModelFilterCountsOnlyMatchingSkillCalls(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_skills_model_calls_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'skills-model-calls-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:30:00Z'::timestamptz,
			2, 0
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('skills-model-calls-001', 0, 'assistant', 'review',
			 '2024-06-01T09:00:00Z'::timestamptz, 6, FALSE, 'gpt-4o'),
			('skills-model-calls-001', 1, 'assistant', 'write',
			 '2024-06-01T09:05:00Z'::timestamptz, 5, FALSE, 'claude-3-5-sonnet')`)
	require.NoError(t, err, "insert messages")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO tool_calls (
			session_id, tool_name, category, call_index,
			skill_name, message_ordinal
		) VALUES
			('skills-model-calls-001', 'Skill', 'Skill', 0, 'review-code', 0),
			('skills-model-calls-001', 'Skill', 'Skill', 0, 'write-tests', 1)`)
	require.NoError(t, err, "insert tool_calls")

	resp, err := store.GetAnalyticsSkills(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsSkills")
	assert.Equal(t, 1, resp.TotalSkillCalls, "TotalSkillCalls")
	assert.Equal(t, 1, resp.DistinctSkills, "DistinctSkills")
	require.Len(t, resp.BySkill, 1, "len(BySkill)")
	assert.Equal(t, "review-code", resp.BySkill[0].SkillName, "SkillName")
	assert.Equal(t, 1, resp.BySkill[0].CallCount, "CallCount")
	assert.Equal(t, "2024-06-01T09:00:00Z", resp.BySkill[0].LastUsedAt,
		"LastUsedAt")
}

func TestStoreGetAnalyticsSummaryModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_summary_model_messages_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'summary-model-messages-001', 'test-machine', 'alpha', 'mixed',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:30:00Z'::timestamptz,
			2, 0
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('summary-model-messages-001', 0, 'assistant', 'gpt',
			 '2024-06-01T09:00:00Z'::timestamptz, 3, FALSE, 'gpt-4o'),
			('summary-model-messages-001', 1, 'assistant', 'claude',
			 '2024-06-01T09:05:00Z'::timestamptz, 6, FALSE, 'claude-3-5-sonnet')`)
	require.NoError(t, err, "insert messages")

	resp, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsSummary")
	assert.Equal(t, 1, resp.TotalSessions, "TotalSessions")
	assert.Equal(t, 1, resp.TotalMessages, "TotalMessages")
	assert.Equal(t, []string{"gpt-4o"}, resp.Models, "Models")
	assert.Equal(t, 1.0, resp.AvgMessages, "AvgMessages")
	assert.Equal(t, 1, resp.MedianMessages, "MedianMessages")
	assert.Equal(t, 1, resp.P90Messages, "P90Messages")
	require.Contains(t, resp.Agents, "mixed")
	assert.Equal(t, 1, resp.Agents["mixed"].Messages, "AgentMessages")
}

func TestStoreGetAnalyticsSummaryModelsUseMatchingHourRowsOnly(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_summary_hour_models_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'summary-hour-models-001', 'test-machine', 'alpha', 'mixed',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T10:30:00Z'::timestamptz,
			2, 0
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('summary-hour-models-001', 0, 'assistant', 'gpt',
			 '2024-06-01T09:05:00Z'::timestamptz, 3, FALSE, 'gpt-4o'),
			('summary-hour-models-001', 1, 'assistant', 'claude',
			 '2024-06-01T10:05:00Z'::timestamptz, 6, FALSE, 'claude-3-5-sonnet')`)
	require.NoError(t, err, "insert messages")

	hour := 9
	resp, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Hour: &hour,
	})
	require.NoError(t, err, "GetAnalyticsSummary")
	assert.Equal(t, 1, resp.TotalSessions, "TotalSessions")
	assert.Equal(t, []string{"gpt-4o"}, resp.Models, "Models")
}

func TestStoreGetAnalyticsSummaryModelFilterUsesFilteredOutputTokens(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_summary_model_output_tokens_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count,
			total_output_tokens, has_total_output_tokens
		) VALUES
			(
				'summary-output-mixed-001', 'test-machine', 'alpha', 'mixed',
				'2024-06-01T10:00:00Z'::timestamptz,
				'2024-06-01T10:30:00Z'::timestamptz,
				2, 0, 111, TRUE
			),
			(
				'summary-output-uncovered-001', 'test-machine', 'alpha', 'mixed',
				'2024-06-01T10:40:00Z'::timestamptz,
				'2024-06-01T11:00:00Z'::timestamptz,
				2, 0, 90, TRUE
			)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model,
			output_tokens, has_output_tokens
		) VALUES
			('summary-output-mixed-001', 0, 'assistant', 'gpt',
			 '2024-06-01T10:00:00Z'::timestamptz, 3, FALSE, 'gpt-4o',
			 11, TRUE),
			('summary-output-mixed-001', 1, 'assistant', 'claude',
			 '2024-06-01T10:05:00Z'::timestamptz, 6, FALSE, 'claude-3-5-sonnet',
			 100, TRUE),
			('summary-output-uncovered-001', 0, 'assistant', 'gpt',
			 '2024-06-01T10:40:00Z'::timestamptz, 3, FALSE, 'gpt-4o',
			 0, FALSE),
			('summary-output-uncovered-001', 1, 'assistant', 'claude',
			 '2024-06-01T10:45:00Z'::timestamptz, 6, FALSE, 'claude-3-5-sonnet',
			 90, TRUE)`)
	require.NoError(t, err, "insert messages")

	hour := 10
	resp, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	})
	require.NoError(t, err, "GetAnalyticsSummary")
	assert.Equal(t, 2, resp.TotalSessions, "TotalSessions")
	assert.Equal(t, 2, resp.TotalMessages, "TotalMessages")
	assert.Equal(t, []string{"gpt-4o"}, resp.Models, "Models")
	assert.Equal(t, 11, resp.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 1, resp.TokenReportingSessions, "TokenReportingSessions")
}

func TestStoreGetAnalyticsProjectsModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_projects_model_messages_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'projects-model-messages-001', 'test-machine', 'alpha', 'mixed',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:30:00Z'::timestamptz,
			2, 0
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('projects-model-messages-001', 0, 'assistant', 'gpt',
			 '2024-06-01T09:00:00Z'::timestamptz, 3, FALSE, 'gpt-4o'),
			('projects-model-messages-001', 1, 'assistant', 'claude',
			 '2024-06-01T09:05:00Z'::timestamptz, 6, FALSE, 'claude-3-5-sonnet')`)
	require.NoError(t, err, "insert messages")

	resp, err := store.GetAnalyticsProjects(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsProjects")
	require.Len(t, resp.Projects, 1, "len(Projects)")
	assert.Equal(t, 1, resp.Projects[0].Messages, "Messages")
	assert.Equal(t, 1.0, resp.Projects[0].AvgMessages, "AvgMessages")
	assert.Equal(t, 1, resp.Projects[0].MedianMessages, "MedianMessages")
	assert.Equal(t, 1.0, resp.Projects[0].DailyTrend, "DailyTrend")
}

func TestStoreGetAnalyticsHeatmapModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_heatmap_model_messages_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'heatmap-model-messages-001', 'test-machine', 'alpha', 'mixed',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:30:00Z'::timestamptz,
			2, 0
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('heatmap-model-messages-001', 0, 'assistant', 'gpt',
			 '2024-06-01T09:00:00Z'::timestamptz, 3, FALSE, 'gpt-4o'),
			('heatmap-model-messages-001', 1, 'assistant', 'claude',
			 '2024-06-01T09:05:00Z'::timestamptz, 6, FALSE, 'claude-3-5-sonnet')`)
	require.NoError(t, err, "insert messages")

	resp, err := store.GetAnalyticsHeatmap(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "messages")
	require.NoError(t, err, "GetAnalyticsHeatmap")
	require.Len(t, resp.Entries, 1, "len(Entries)")
	assert.Equal(t, 1, resp.Entries[0].Value, "Value")
}

func TestStoreGetAnalyticsHeatmapModelFilterUsesFilteredOutputTokens(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_heatmap_model_output_tokens_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count,
			total_output_tokens, has_total_output_tokens
		) VALUES
			(
				'heatmap-output-mixed-001', 'test-machine', 'alpha', 'mixed',
				'2024-06-01T10:00:00Z'::timestamptz,
				'2024-06-01T10:30:00Z'::timestamptz,
				2, 0, 111, TRUE
			),
			(
				'heatmap-output-uncovered-001', 'test-machine', 'alpha', 'mixed',
				'2024-06-01T10:40:00Z'::timestamptz,
				'2024-06-01T11:00:00Z'::timestamptz,
				2, 0, 90, TRUE
			)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model,
			output_tokens, has_output_tokens
		) VALUES
			('heatmap-output-mixed-001', 0, 'assistant', 'gpt',
			 '2024-06-01T10:00:00Z'::timestamptz, 3, FALSE, 'gpt-4o',
			 11, TRUE),
			('heatmap-output-mixed-001', 1, 'assistant', 'claude',
			 '2024-06-01T10:05:00Z'::timestamptz, 6, FALSE, 'claude-3-5-sonnet',
			 100, TRUE),
			('heatmap-output-uncovered-001', 0, 'assistant', 'gpt',
			 '2024-06-01T10:40:00Z'::timestamptz, 3, FALSE, 'gpt-4o',
			 0, FALSE),
			('heatmap-output-uncovered-001', 1, 'assistant', 'claude',
			 '2024-06-01T10:45:00Z'::timestamptz, 6, FALSE, 'claude-3-5-sonnet',
			 90, TRUE)`)
	require.NoError(t, err, "insert messages")

	hour := 10
	resp, err := store.GetAnalyticsHeatmap(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "output_tokens")
	require.NoError(t, err, "GetAnalyticsHeatmap")
	require.Len(t, resp.Entries, 1, "len(Entries)")
	assert.Equal(t, 11, resp.Entries[0].Value, "Value")
}

func TestStoreGetAnalyticsTopSessionsMessagesUseFilteredModelCounts(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_top_sessions_model_messages_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES
			(
				'top-model-mixed-001', 'test-machine', 'alpha', 'mixed',
				'2024-06-01T09:00:00Z'::timestamptz,
				'2024-06-01T10:00:00Z'::timestamptz,
				3, 0
			),
			(
				'top-model-gpt-001', 'test-machine', 'alpha', 'gpt',
				'2024-06-01T11:00:00Z'::timestamptz,
				'2024-06-01T12:00:00Z'::timestamptz,
				2, 0
			)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('top-model-mixed-001', 0, 'assistant', 'gpt',
			 '2024-06-01T09:00:00Z'::timestamptz, 3, FALSE, 'gpt-4o'),
			('top-model-mixed-001', 1, 'assistant', 'claude',
			 '2024-06-01T09:05:00Z'::timestamptz, 6, FALSE, 'claude-3-5-sonnet'),
			('top-model-mixed-001', 2, 'assistant', 'claude',
			 '2024-06-01T09:06:00Z'::timestamptz, 6, FALSE, 'claude-3-5-sonnet'),
			('top-model-gpt-001', 0, 'assistant', 'gpt',
			 '2024-06-01T11:00:00Z'::timestamptz, 3, FALSE, 'gpt-4o'),
			('top-model-gpt-001', 1, 'assistant', 'gpt',
			 '2024-06-01T11:05:00Z'::timestamptz, 3, FALSE, 'gpt-4o')`)
	require.NoError(t, err, "insert messages")

	resp, err := store.GetAnalyticsTopSessions(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "messages")
	require.NoError(t, err, "GetAnalyticsTopSessions")
	require.Len(t, resp.Sessions, 2, "len(Sessions)")
	assert.Equal(t, "top-model-gpt-001", resp.Sessions[0].ID, "top session")
	assert.Equal(t, 2, resp.Sessions[0].MessageCount, "top MessageCount")
	assert.Equal(t, "top-model-mixed-001", resp.Sessions[1].ID, "second session")
	assert.Equal(t, 1, resp.Sessions[1].MessageCount, "second MessageCount")
}

func TestStoreGetAnalyticsTopSessionsOutputTokensUseFilteredModelTotals(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_top_sessions_model_output_tokens_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count,
			total_output_tokens, has_total_output_tokens
		) VALUES
			(
				'top-output-mixed-001', 'test-machine', 'alpha', 'mixed',
				'2024-06-01T09:00:00Z'::timestamptz,
				'2024-06-01T10:00:00Z'::timestamptz,
				2, 0, 510, TRUE
			),
			(
				'top-output-gpt-001', 'test-machine', 'alpha', 'gpt',
				'2024-06-01T11:00:00Z'::timestamptz,
				'2024-06-01T12:00:00Z'::timestamptz,
				1, 0, 30, TRUE
			),
			(
				'top-output-uncovered-001', 'test-machine', 'alpha', 'mixed',
				'2024-06-01T13:00:00Z'::timestamptz,
				'2024-06-01T14:00:00Z'::timestamptz,
				2, 0, 900, TRUE
			)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model,
			output_tokens, has_output_tokens
		) VALUES
			('top-output-mixed-001', 0, 'assistant', 'gpt',
			 '2024-06-01T09:00:00Z'::timestamptz, 3, FALSE, 'gpt-4o',
			 10, TRUE),
			('top-output-mixed-001', 1, 'assistant', 'claude',
			 '2024-06-01T09:05:00Z'::timestamptz, 6, FALSE, 'claude-3-5-sonnet',
			 500, TRUE),
			('top-output-gpt-001', 0, 'assistant', 'gpt',
			 '2024-06-01T11:00:00Z'::timestamptz, 3, FALSE, 'gpt-4o',
			 30, TRUE),
			('top-output-uncovered-001', 0, 'assistant', 'gpt',
			 '2024-06-01T13:00:00Z'::timestamptz, 3, FALSE, 'gpt-4o',
			 0, FALSE),
			('top-output-uncovered-001', 1, 'assistant', 'claude',
			 '2024-06-01T13:05:00Z'::timestamptz, 6, FALSE, 'claude-3-5-sonnet',
			 900, TRUE)`)
	require.NoError(t, err, "insert messages")

	resp, err := store.GetAnalyticsTopSessions(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "output_tokens")
	require.NoError(t, err, "GetAnalyticsTopSessions")
	require.Len(t, resp.Sessions, 2, "len(Sessions)")
	assert.Equal(t, "top-output-gpt-001", resp.Sessions[0].ID, "top session")
	assert.Equal(t, 30, resp.Sessions[0].OutputTokens,
		"top OutputTokens")
	assert.Equal(t, "top-output-mixed-001", resp.Sessions[1].ID,
		"second session")
	assert.Equal(t, 10, resp.Sessions[1].OutputTokens,
		"second OutputTokens")
}

// The model-scoped messages/output_tokens path drops the SQL LIMIT to re-rank
// by filtered counts in Go, so it must reapply the top-10 cap before returning
// like SQLite and DuckDB. Seed more than 10 model-matching sessions and assert
// both metrics still cap at 10 with the highest-count session first.
func TestStoreGetAnalyticsTopSessionsModelFilterCapsAtTen(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_top_sessions_model_cap_test")
	ctx := context.Background()

	const sessionCount = 12
	var sessionRows, messageRows []string
	for k := 0; k < sessionCount; k++ {
		id := fmt.Sprintf("top-cap-%02d", k)
		// Distinct gpt-4o message counts 1..12 so ranking is unambiguous.
		msgs := k + 1
		sessionRows = append(sessionRows, fmt.Sprintf(
			`('%s', 'test-machine', 'alpha', 'gpt',
				'2024-06-01T09:00:00Z'::timestamptz,
				'2024-06-01T10:00:00Z'::timestamptz,
				%d, 0, %d, TRUE)`, id, msgs, msgs*10))
		for o := 0; o < msgs; o++ {
			messageRows = append(messageRows, fmt.Sprintf(
				`('%s', %d, 'assistant', 'gpt',
					'2024-06-01T09:00:00Z'::timestamptz, 3, FALSE, 'gpt-4o',
					10, TRUE)`, id, o))
		}
	}
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count,
			total_output_tokens, has_total_output_tokens
		) VALUES `+strings.Join(sessionRows, ","))
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model,
			output_tokens, has_output_tokens
		) VALUES `+strings.Join(messageRows, ","))
	require.NoError(t, err, "insert messages")

	for _, tc := range []struct {
		metric string
		topVal int
		field  func(db.TopSession) int
	}{
		{"messages", 12, func(s db.TopSession) int { return s.MessageCount }},
		{"output_tokens", 120, func(s db.TopSession) int { return s.OutputTokens }},
	} {
		t.Run(tc.metric, func(t *testing.T) {
			resp, err := store.GetAnalyticsTopSessions(ctx, db.AnalyticsFilter{
				From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
				Model: "gpt-4o",
			}, tc.metric)
			require.NoError(t, err, "GetAnalyticsTopSessions")
			require.Len(t, resp.Sessions, 10,
				"more than 10 model-matching sessions must cap at 10")
			assert.Equal(t, "top-cap-11", resp.Sessions[0].ID,
				"highest-count session ranks first")
			assert.Equal(t, tc.topVal, tc.field(resp.Sessions[0]),
				"top session filtered count")
		})
	}
}

func TestStoreGetAnalyticsVelocityModelFilterUsesMatchingRowsOnly(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_velocity_model_rows_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at, message_count
		) VALUES (
			'velocity-model-rows-001', 'test-machine', 'alpha', 'mixed',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:11:00Z'::timestamptz,
			4
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('velocity-model-rows-001', 0, 'user', 'claude q',
			 '2024-06-01T09:00:00Z'::timestamptz, 8, FALSE, 'claude-3-5-sonnet'),
			('velocity-model-rows-001', 1, 'assistant', 'offscope-offscope-xx',
			 '2024-06-01T09:00:10Z'::timestamptz, 20, FALSE, 'claude-3-5-sonnet'),
			('velocity-model-rows-001', 2, 'user', 'gpt q',
			 '2024-06-01T09:10:00Z'::timestamptz, 5, FALSE, ''),
			('velocity-model-rows-001', 3, 'assistant', 'reply',
			 '2024-06-01T09:11:00Z'::timestamptz, 5, FALSE, 'gpt-4o')`)
	require.NoError(t, err, "insert messages")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO tool_calls (
			session_id, tool_name, category, call_index, message_ordinal
		) VALUES
			('velocity-model-rows-001', 'Read', 'Read', 0, 1),
			('velocity-model-rows-001', 'Bash', 'Bash', 1, 1),
			('velocity-model-rows-001', 'Grep', 'Grep', 2, 1),
			('velocity-model-rows-001', 'Edit', 'Edit', 0, 3),
			('velocity-model-rows-001', 'Write', 'Write', 1, 3)`)
	require.NoError(t, err, "insert tool_calls")

	resp, err := store.GetAnalyticsVelocity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsVelocity")
	assert.Equal(t, 60.0, resp.Overall.FirstResponseSec.P50,
		"FirstResponse P50")
	assert.Equal(t, 2.0, resp.Overall.MsgsPerActiveMin,
		"MsgsPerActiveMin")
	assert.Equal(t, 5.0, resp.Overall.CharsPerActiveMin,
		"CharsPerActiveMin")
	assert.Equal(t, 2.0, resp.Overall.ToolCallsPerActiveMin,
		"ToolCallsPerActiveMin")
}

func TestStoreGetAnalyticsVelocityModelFilterPreservesSubsecondTimestamps(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_velocity_model_subsecond_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at, message_count
		) VALUES (
			'velocity-model-subsecond-001', 'test-machine', 'alpha', 'mixed',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:00:01Z'::timestamptz,
			2
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('velocity-model-subsecond-001', 0, 'user', 'gpt q',
			 '2024-06-01T09:00:00.000Z'::timestamptz, 5, FALSE, ''),
			('velocity-model-subsecond-001', 1, 'assistant', 'reply',
			 '2024-06-01T09:00:00.500Z'::timestamptz, 5, FALSE, 'gpt-4o')`)
	require.NoError(t, err, "insert messages")

	resp, err := store.GetAnalyticsVelocity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsVelocity")
	// The paired empty-model user turn at 09:00:00.000 and the gpt-4o assistant
	// at 09:00:00.500 are 0.5s apart. A whole-second TO_CHAR in the scope
	// resolver would collapse the first-response gap to 0.0s.
	assert.Equal(t, 0.5, resp.Overall.FirstResponseSec.P50,
		"FirstResponse P50 keeps the subsecond gap")
}

func TestStoreGetAnalyticsVelocityModelFilterCountsNullTimestampToolCallsWithoutTimeFilter(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_velocity_model_null_tool_ts_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at, message_count
		) VALUES (
			'velocity-model-null-ts-001', 'test-machine', 'alpha', 'mixed',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:11:00Z'::timestamptz,
			5
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('velocity-model-null-ts-001', 0, 'user', 'claude q',
			 '2024-06-01T09:00:00Z'::timestamptz, 8, FALSE, 'claude-3-5-sonnet'),
			('velocity-model-null-ts-001', 1, 'assistant', 'offscope-offscope-xx',
			 '2024-06-01T09:00:10Z'::timestamptz, 20, FALSE, 'claude-3-5-sonnet'),
			('velocity-model-null-ts-001', 2, 'user', 'gpt q',
			 '2024-06-01T09:10:00Z'::timestamptz, 5, FALSE, ''),
			('velocity-model-null-ts-001', 3, 'assistant', 'reply',
			 '2024-06-01T09:11:00Z'::timestamptz, 5, FALSE, 'gpt-4o'),
			('velocity-model-null-ts-001', 4, 'assistant', 'extra',
			 NULL, 5, FALSE, 'gpt-4o')`)
	require.NoError(t, err, "insert messages")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO tool_calls (
			session_id, tool_name, category, call_index, message_ordinal
		) VALUES
			('velocity-model-null-ts-001', 'Read', 'Read', 0, 1),
			('velocity-model-null-ts-001', 'Bash', 'Bash', 1, 1),
			('velocity-model-null-ts-001', 'Grep', 'Grep', 2, 1),
			('velocity-model-null-ts-001', 'Edit', 'Edit', 0, 3),
			('velocity-model-null-ts-001', 'Write', 'Write', 1, 3),
			('velocity-model-null-ts-001', 'Search', 'Search', 0, 4)`)
	require.NoError(t, err, "insert tool_calls")

	resp, err := store.GetAnalyticsVelocity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsVelocity")
	assert.Equal(t, 60.0, resp.Overall.FirstResponseSec.P50,
		"FirstResponse P50")
	assert.Equal(t, 3.0, resp.Overall.ToolCallsPerActiveMin,
		"ToolCallsPerActiveMin")
}

func TestStoreGetAnalyticsSignalSessionsModelFilterKeepsParserUserEvidence(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_signal_model_parser_user_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count,
			quality_signal_version, short_prompt_count
		) VALUES (
			'signal-model-parser-user-001', 'test-machine', 'alpha', 'mixed',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:10:00Z'::timestamptz,
			2, 1,
			1, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model, has_tool_use
		) VALUES
			('signal-model-parser-user-001', 0, 'user', 'help',
			 '2024-06-01T09:00:00Z'::timestamptz, 4, FALSE, '', FALSE),
			('signal-model-parser-user-001', 1, 'assistant', 'reply',
			 '2024-06-01T09:01:00Z'::timestamptz, 5, FALSE, 'gpt-4o', TRUE)`)
	require.NoError(t, err, "insert messages")

	resp, err := store.GetAnalyticsSignalSessions(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "short_prompt_count", 10)
	require.NoError(t, err, "GetAnalyticsSignalSessions")
	require.Len(t, resp.Sessions, 1, "len(Sessions)")
	assert.Equal(t, "help", resp.Sessions[0].Excerpt)
	require.NotNil(t, resp.Sessions[0].MessageOrdinal)
	assert.Equal(t, 0, *resp.Sessions[0].MessageOrdinal)
}

func TestStoreGetAnalyticsSessionShapeModelFilterUsesMatchingRowsOnly(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_shape_model_rows_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'shape-model-rows-001', 'test-machine', 'alpha', 'mixed',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:30:00Z'::timestamptz,
			6, 4
		)`)
	require.NoError(t, err, "insert session")

	type row struct {
		ordinal    int
		role       string
		content    string
		timestamp  string
		model      string
		hasToolUse bool
	}
	rows := []row{
		{0, "user", "gpt q", "2024-06-01T09:00:00Z", "", false},
		{1, "assistant", "gpt tool", "2024-06-01T09:01:00Z", "gpt-4o", true},
		{2, "user", "claude q1", "2024-06-01T09:02:00Z", "claude-3-5-sonnet", false},
		{3, "user", "claude q2", "2024-06-01T09:03:00Z", "claude-3-5-sonnet", false},
		{4, "user", "claude q3", "2024-06-01T09:04:00Z", "claude-3-5-sonnet", false},
		{5, "assistant", "claude reply", "2024-06-01T09:05:00Z", "claude-3-5-sonnet", false},
	}
	for _, row := range rows {
		_, err = store.DB().ExecContext(ctx, `
			INSERT INTO messages (
				session_id, ordinal, role, content, timestamp,
				content_length, is_system, model, has_tool_use
			) VALUES (
				$1, $2, $3, $4, $5::timestamptz,
				$6, FALSE, $7, $8
			)`,
			"shape-model-rows-001", row.ordinal, row.role, row.content,
			row.timestamp, len(row.content), row.model, row.hasToolUse,
		)
		require.NoError(t, err, "insert message %d", row.ordinal)
	}
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO tool_calls (
			session_id, tool_name, category, call_index, message_ordinal
		) VALUES (
			$1, $2, $3, $4, $5
		)`,
		"shape-model-rows-001", "Read", "Read", 0, 1,
	)
	require.NoError(t, err, "insert tool call")

	resp, err := store.GetAnalyticsSessionShape(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsSessionShape")
	assert.Equal(t, 1, resp.Count, "Count")

	lengths := map[string]int{}
	for _, bucket := range resp.LengthDistribution {
		lengths[bucket.Label] = bucket.Count
	}
	assert.Equal(t, 1, lengths["1-5"], "filtered length bucket")
	assert.Equal(t, 0, lengths["6-15"], "full-session count must not leak")

	autonomy := map[string]int{}
	for _, bucket := range resp.AutonomyDistribution {
		autonomy[bucket.Label] = bucket.Count
	}
	assert.Equal(t, 1, autonomy["1-2"], "filtered autonomy bucket")
	assert.Equal(t, 0, autonomy["<0.5"], "off-model user turns must not leak")
}

func TestStoreGetAnalyticsVelocityModelFilterUsesMatchingComplexityBucket(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_velocity_model_complexity_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at, message_count
		) VALUES (
			'velocity-model-complexity-001', 'test-machine', 'alpha', 'mixed',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T09:15:00Z'::timestamptz,
			16
		)`)
	require.NoError(t, err, "insert session")

	type row struct {
		ordinal   int
		role      string
		content   string
		timestamp string
		model     string
	}
	rows := []row{
		{0, "user", "gpt q", "2024-06-01T09:00:00Z", ""},
		{1, "assistant", "reply", "2024-06-01T09:01:00Z", "gpt-4o"},
	}
	for i := 2; i < 16; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		rows = append(rows, row{
			ordinal: i, role: role, content: "claude",
			timestamp: "2024-06-01T09:10:00Z",
			model:     "claude-3-5-sonnet",
		})
	}
	for _, row := range rows {
		_, err = store.DB().ExecContext(ctx, `
			INSERT INTO messages (
				session_id, ordinal, role, content, timestamp,
				content_length, is_system, model
			) VALUES (
				$1, $2, $3, $4, $5::timestamptz,
				$6, FALSE, $7
			)`,
			"velocity-model-complexity-001", row.ordinal, row.role,
			row.content, row.timestamp, len(row.content), row.model,
		)
		require.NoError(t, err, "insert message %d", row.ordinal)
	}

	resp, err := store.GetAnalyticsVelocity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsVelocity")
	require.Len(t, resp.ByComplexity, 1, "len(ByComplexity)")
	assert.Equal(t, "1-15", resp.ByComplexity[0].Label,
		"complexity bucket should use filtered message count")
	assert.Equal(t, 1, resp.ByComplexity[0].Sessions, "Sessions")
}

func findPGHOWCell(cells []db.HourOfWeekCell, dow, hour int) int {
	for _, cell := range cells {
		if cell.DayOfWeek == dow && cell.Hour == hour {
			return cell.Messages
		}
	}
	return -1
}
