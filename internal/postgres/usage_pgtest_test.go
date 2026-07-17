//go:build pgtest

package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func prepareUsageSchema(
	t *testing.T, schema string,
) (string, *Store) {
	t.Helper()

	pgURL := testPGURL(t)
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = pg.Close() })

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	t.Cleanup(func() { _ = store.Close() })
	return pgURL, store
}

func TestStoreGetDailyUsageUsesFallbackPricing(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_fallback_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'usage-fallback-001', 'test-machine', 'proj', 'claude',
			'2026-03-12T10:00:00Z'::timestamptz, 1, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model, token_usage
		) VALUES (
			'usage-fallback-001', 0, 'assistant', 'hi',
			'2026-03-12T10:00:00Z'::timestamptz, 2,
			'claude-sonnet-4-20250514',
			'{"input_tokens":1000000}'
		)`)
	require.NoError(t, err, "insert message")

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:     "2026-03-12",
		To:       "2026-03-12",
		Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	assert.Equal(t, 3.0, result.Totals.TotalCost)
	assert.Len(t, result.Daily, 1)
}

func TestStoreGetDailyUsageWithBreakdowns(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_breakdown_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES
			('test-model-a', 1, 2, 3, 0.5, 'seed'),
			('test-model-b', 2, 4, 0, 0, 'seed')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('usage-breakdown-001', 'host-a', 'proj-a', 'claude',
			 '2026-03-12T10:00:00Z'::timestamptz, 1, 1),
			('usage-breakdown-002', 'host-b', 'proj-b', 'codex',
			 '2026-03-12T11:00:00Z'::timestamptz, 1, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage
		) VALUES
			('usage-breakdown-001', 0, 'assistant', 'one',
			 '2026-03-12T10:00:00Z'::timestamptz, 3,
			 'test-model-a',
			 '{"input_tokens":1000000,"output_tokens":500000,"cache_creation_input_tokens":250000,"cache_read_input_tokens":250000}'),
			('usage-breakdown-002', 0, 'assistant', 'two',
			 '2026-03-12T11:00:00Z'::timestamptz, 3,
			 'test-model-b',
			 '{"input_tokens":500000,"output_tokens":250000}')`)
	require.NoError(t, err, "insert messages")

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:       "2026-03-12",
		To:         "2026-03-12",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, result.Daily, 1)
	day := result.Daily[0]
	assert.Equal(t, 1500000, day.InputTokens)
	assert.Equal(t, 750000, day.OutputTokens)
	assert.Len(t, day.ProjectBreakdowns, 2)
	assert.Len(t, day.AgentBreakdowns, 2)
	assert.Len(t, day.ModelBreakdowns, 2)
	require.Len(t, day.MachineBreakdowns, 2)
	assert.Equal(t, "host-a", day.MachineBreakdowns[0].MachineName)
	assert.Equal(t, "host-b", day.MachineBreakdowns[1].MachineName)
	assert.InDelta(t, day.TotalCost,
		day.MachineBreakdowns[0].Cost+day.MachineBreakdowns[1].Cost, 1e-9)
	assert.Greater(t, day.TotalCost, 0.0)

	noCounts, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:              "2026-03-12",
		To:                "2026-03-12",
		Timezone:          "UTC",
		SkipSessionCounts: true,
	})
	require.NoError(t, err, "GetDailyUsage skip session counts")
	assert.Equal(t, result.Totals.InputTokens, noCounts.Totals.InputTokens)
	assert.Zero(t, noCounts.SessionCounts.Total)
	assert.Nil(t, noCounts.SessionCounts.ByProject)
	assert.Nil(t, noCounts.SessionCounts.ByAgent)
}

func TestStoreGetDailyUsageDedupesBySourceUUIDWhenClaudePairIncomplete(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_source_uuid_daily_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('test-model-source-daily', 1, 2, 3, 0.5, 'seed')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('usage-source-daily-001', 'test-machine', 'proj-a', 'claude',
			 '2026-03-12T10:00:00Z'::timestamptz, 1, 1),
			('usage-source-daily-002', 'test-machine', 'proj-b', 'claude',
			 '2026-03-12T10:01:00Z'::timestamptz, 1, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id, source_uuid
		) VALUES
			('usage-source-daily-001', 0, 'assistant', 'one',
			 '2026-03-12T10:00:00Z'::timestamptz, 3,
			 'test-model-source-daily',
			 '{"input_tokens":1000000,"output_tokens":500000,"cache_creation_input_tokens":250000,"cache_read_input_tokens":250000}',
			 'msg-1', '', 'source-1'),
			('usage-source-daily-002', 0, 'assistant', 'two',
			 '2026-03-12T10:01:00Z'::timestamptz, 3,
			 'test-model-source-daily',
			 '{"input_tokens":1000000,"output_tokens":500000,"cache_creation_input_tokens":250000,"cache_read_input_tokens":250000}',
			 'msg-1', '', 'source-1')`)
	require.NoError(t, err, "insert messages")

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:     "2026-03-12",
		To:       "2026-03-12",
		Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, result.Daily, 1)
	assert.Equal(t, 1000000, result.Daily[0].InputTokens)
	assert.Equal(t, 500000, result.Daily[0].OutputTokens)
}

func TestStoreGetSessionUsagePricedModel(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_priced_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('gpt-5.1', 3, 15, 3.75, 0.30, 'seed')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		) VALUES (
			'codex:usage-priced', 'test-machine', 'my-project', 'codex',
			'2026-03-12T10:00:00Z'::timestamptz, 2, 1,
			1234, 56789, true, true
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage
		) VALUES (
			'codex:usage-priced', 1, 'assistant', 'done',
			'2026-03-12T10:01:00Z'::timestamptz, 4,
			'gpt-5.1',
			'{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":200,"cache_read_input_tokens":300}'
		)`)
	require.NoError(t, err, "insert message")

	got, err := store.GetSessionUsage(ctx, "codex:usage-priced", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, got, "GetSessionUsage result")
	assert.Equal(t, "codex:usage-priced", got.SessionID)
	assert.Equal(t, "codex", got.Agent)
	assert.Equal(t, "my-project", got.Project)
	assert.Equal(t, 1234, got.TotalOutputTokens)
	assert.Equal(t, 56789, got.PeakContextTokens)
	assert.True(t, got.HasTokenData)
	assert.True(t, got.HasCost)
	assert.InDelta(t, 0.01134, got.CostUSD, 1e-9)
	assert.Equal(t, []string{"gpt-5.1"}, got.Models)
	assert.Empty(t, got.UnpricedModels)
	require.Len(t, got.Breakdown, 1, "Breakdown")
	entry := got.Breakdown[0]
	assert.Equal(t, 1, entry.Ordinal)
	require.NotNil(t, entry.MessageOrdinal)
	assert.Equal(t, 1, *entry.MessageOrdinal)
	assert.Equal(t, "message", entry.Source)
	assert.Equal(t, "Prompt 2", entry.Label)
	assert.Equal(t, "2026-03-12T10:01:00Z", entry.Timestamp)
	assert.Equal(t, "gpt-5.1", entry.Model)
	assert.Equal(t, 1000, entry.InputTokens)
	assert.Equal(t, 500, entry.OutputTokens)
	assert.Equal(t, 200, entry.CacheCreationInputTokens)
	assert.Equal(t, 300, entry.CacheReadInputTokens)
	assert.True(t, entry.HasCost)
	assert.InDelta(t, 0.01134, entry.CostUSD, 1e-9)
}

func TestStoreSessionUsageRollupParity(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_rollup_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('gpt-5.1', 3, 15, 3.75, 0.30, 'seed')`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, message_count,
			user_message_count, parent_session_id, relationship_type
		) VALUES
			('pg-rollup-root', 'test', 'project', 'codex', '2026-03-12T10:00:00Z', 1, 1, NULL, 'root'),
			('pg-rollup-continuation', 'test', 'project', 'codex', '2026-03-12T10:01:00Z', 0, 0, 'pg-rollup-root', 'continuation'),
			('pg-rollup-child', 'test', 'project', 'codex', '2026-03-12T10:02:00Z', 1, 1, 'pg-rollup-continuation', 'subagent')`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
			('pg-rollup-root', 0, 'assistant', 'root', '2026-03-12T10:00:00Z', 4, 'gpt-5.1', '{"input_tokens":1000,"output_tokens":500}', 'pg-rollup-shared', 'pg-rollup-request'),
			('pg-rollup-child', 0, 'assistant', 'child', '2026-03-12T10:02:00Z', 5, 'gpt-5.1', '{"input_tokens":1000,"output_tokens":500}', 'pg-rollup-shared', 'pg-rollup-request'),
			('pg-rollup-child', 1, 'assistant', 'child unique', '2026-03-12T10:03:00Z', 12, 'gpt-5.1', '{"input_tokens":1000,"output_tokens":500}', 'pg-rollup-unique', 'pg-rollup-unique-request')`)
	require.NoError(t, err)

	rollup, err := service.GetSessionUsageRollup(ctx, store, "pg-rollup-root", false)
	require.NoError(t, err)
	require.Equal(t, 1, rollup.SubagentCount)
	require.True(t, rollup.HasCost)
	assert.InDelta(t, 0.021, rollup.CostUSD, 1e-9)
}

func TestStoreSessionUsageRollupIncludesUntimedRows(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_rollup_untimed_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('gpt-5.1', 3, 15, 3.75, 0.30, 'seed')`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, message_count,
			user_message_count, parent_session_id, relationship_type
		) VALUES
			('pg-rollup-untimed-root', 'test', 'project', 'codex', '2026-03-12T10:00:00Z', 1, 1, NULL, 'root'),
			('pg-rollup-untimed-child', 'test', 'project', 'codex', '2026-03-12T10:02:00Z', 1, 1, 'pg-rollup-untimed-root', 'subagent')`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage
		) VALUES
			('pg-rollup-untimed-root', 0, 'assistant', 'root', NULL, 4, 'gpt-5.1', '{"input_tokens":1000,"output_tokens":500}'),
			('pg-rollup-untimed-child', 0, 'assistant', 'child', NULL, 5, 'gpt-5.1', '{"input_tokens":1000,"output_tokens":500}')`)
	require.NoError(t, err)

	rollup, err := service.GetSessionUsageRollup(ctx, store, "pg-rollup-untimed-root", false)
	require.NoError(t, err)
	require.Equal(t, 1, rollup.SubagentCount)
	require.True(t, rollup.HasCost)
	assert.InDelta(t, 0.021, rollup.CostUSD, 1e-9)
}

func TestStoreGetSessionUsageDedupesSourceUUIDWhenClaudePairIncomplete(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_source_uuid_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('claude-opus-4-6', 5, 25, 6.25, 0.5, 'seed')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'claude:usage-source', 'test-machine', 'proj', 'claude-code',
			'2026-03-12T10:00:00Z'::timestamptz, 2, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id, source_uuid
		) VALUES
			('claude:usage-source', 0, 'assistant', 'one',
			 '2026-03-12T10:00:00Z'::timestamptz, 3,
			 'claude-opus-4-6', '{"input_tokens":1000,"output_tokens":500}',
			 'msg-1', '', 'source-1'),
			('claude:usage-source', 1, 'assistant', 'two',
			 '2026-03-12T10:01:00Z'::timestamptz, 3,
			 'claude-opus-4-6', '{"input_tokens":1000,"output_tokens":500}',
			 'msg-1', '', 'source-1')`)
	require.NoError(t, err, "insert messages")

	got, err := store.GetSessionUsage(ctx, "claude:usage-source", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, got, "GetSessionUsage result")
	assert.True(t, got.HasCost)
	assert.InDelta(t, 0.0175, got.CostUSD, 1e-9)
	assert.Equal(t, []string{"claude-opus-4-6"}, got.Models)
	require.Len(t, got.Breakdown, 1, "Breakdown")
	require.NotNil(t, got.Breakdown[0].MessageOrdinal)
	assert.Equal(t, 0, *got.Breakdown[0].MessageOrdinal)
}

func TestStoreGetSessionUsageNoTokenRowsKeepsMetadata(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_empty_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'codex:usage-empty', 'test-machine', 'quiet-project', 'codex',
			'2026-03-12T10:00:00Z'::timestamptz, 1, 1
		)`)
	require.NoError(t, err, "insert session")

	got, err := store.GetSessionUsage(ctx, "codex:usage-empty", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, got, "GetSessionUsage result")
	assert.Equal(t, "codex:usage-empty", got.SessionID)
	assert.Equal(t, "codex", got.Agent)
	assert.Equal(t, "quiet-project", got.Project)
	assert.False(t, got.HasTokenData)
	assert.False(t, got.HasCost)
	assert.Zero(t, got.CostUSD)
	assert.Empty(t, got.Models)
	assert.Empty(t, got.UnpricedModels)
	assert.Empty(t, got.Breakdown)
}

func TestStoreGetSessionUsageNotFound(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_missing_test")

	got, err := store.GetSessionUsage(context.Background(), "missing", true)
	require.NoError(t, err, "GetSessionUsage")
	assert.Nil(t, got, "GetSessionUsage")
}

func TestStoreGetTopSessionsByCostDedupesClaudeKeys(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_top_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('test-model-top', 1, 0, 0, 0, 'seed')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('usage-top-001', 'test-machine', 'proj-a', 'claude',
			 '2026-03-12T10:00:00Z'::timestamptz, 1, 1),
			('usage-top-002', 'test-machine', 'proj-b', 'claude',
			 '2026-03-12T10:01:00Z'::timestamptz, 1, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
			('usage-top-001', 0, 'assistant', 'one',
			 '2026-03-12T10:00:00Z'::timestamptz, 3,
			 'test-model-top', '{"input_tokens":1000000}', 'msg-1', 'req-1'),
			('usage-top-002', 0, 'assistant', 'two',
			 '2026-03-12T10:01:00Z'::timestamptz, 3,
			 'test-model-top', '{"input_tokens":1000000}', 'msg-1', 'req-1')`)
	require.NoError(t, err, "insert messages")

	top, err := store.GetTopSessionsByCost(ctx, db.UsageFilter{
		From:     "2026-03-12",
		To:       "2026-03-12",
		Timezone: "UTC",
	}, 20)
	require.NoError(t, err, "GetTopSessionsByCost")
	require.Len(t, top, 1)
	assert.Equal(t, "usage-top-001", top[0].SessionID)
}

func TestStoreGetTopSessionsByCostDedupesSourceUUIDFallback(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_top_source_uuid_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('test-model-top-source', 1, 0, 0, 0, 'seed')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('usage-top-source-001', 'test-machine', 'proj-a', 'claude',
			 '2026-03-12T10:00:00Z'::timestamptz, 1, 1),
			('usage-top-source-002', 'test-machine', 'proj-b', 'claude',
			 '2026-03-12T10:01:00Z'::timestamptz, 1, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id, source_uuid
		) VALUES
			('usage-top-source-001', 0, 'assistant', 'one',
			 '2026-03-12T10:00:00Z'::timestamptz, 3,
			 'test-model-top-source', '{"input_tokens":1000000}', 'msg-1', '', 'source-1'),
			('usage-top-source-002', 0, 'assistant', 'two',
			 '2026-03-12T10:01:00Z'::timestamptz, 3,
			 'test-model-top-source', '{"input_tokens":1000000}', 'msg-1', '', 'source-1')`)
	require.NoError(t, err, "insert messages")

	top, err := store.GetTopSessionsByCost(ctx, db.UsageFilter{
		From:     "2026-03-12",
		To:       "2026-03-12",
		Timezone: "UTC",
	}, 20)
	require.NoError(t, err, "GetTopSessionsByCost")
	require.Len(t, top, 1)
	assert.Equal(t, "usage-top-source-001", top[0].SessionID)
}

func TestStoreGetUsageSessionCountsDedupesClaudeKeys(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_counts_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('usage-counts-001', 'test-machine', 'proj-a', 'claude',
			 '2026-03-12T10:00:00Z'::timestamptz, 1, 1),
			('usage-counts-002', 'test-machine', 'proj-b', 'claude',
			 '2026-03-12T10:01:00Z'::timestamptz, 1, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
			('usage-counts-001', 0, 'assistant', 'one',
			 '2026-03-12T10:00:00Z'::timestamptz, 3,
			 'test-model-counts', '{"input_tokens":1}', 'msg-1', 'req-1'),
			('usage-counts-002', 0, 'assistant', 'two',
			 '2026-03-12T10:01:00Z'::timestamptz, 3,
			 'test-model-counts', '{"input_tokens":1}', 'msg-1', 'req-1')`)
	require.NoError(t, err, "insert messages")

	counts, err := store.GetUsageSessionCounts(ctx, db.UsageFilter{
		From:     "2026-03-12",
		To:       "2026-03-12",
		Timezone: "UTC",
	})
	require.NoError(t, err, "GetUsageSessionCounts")
	assert.Equal(t, 1, counts.Total)
	assert.Equal(t, 1, counts.ByProject["proj-a"])
	_, ok := counts.ByProject["proj-b"]
	assert.False(t, ok, "proj-b should have been deduped out: %#v", counts.ByProject)
}

func TestStoreGetUsageMatchingSessionCountCountsCopilotSessionsWithoutUsageRows(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_usage_matching_sessions_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES
			('copilot-empty', 'test-machine', 'proj-a', 'copilot',
			 '2026-03-12T10:00:00Z'::timestamptz,
			 '2026-03-12T10:00:00Z'::timestamptz, 1, 1),
			('claude-usage', 'test-machine', 'proj-a', 'claude',
			 '2026-03-12T11:00:00Z'::timestamptz,
			 '2026-03-12T11:00:00Z'::timestamptz, 1, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage
		) VALUES
			('copilot-empty', 0, 'assistant', 'copilot',
			 '2026-03-12T10:00:00Z'::timestamptz, 7,
			 'gpt-5.3-codex', ''),
			('claude-usage', 0, 'assistant', 'claude',
			 '2026-03-12T11:00:00Z'::timestamptz, 6,
			 'claude-sonnet-4-20250514', '{"input_tokens":1}')`)
	require.NoError(t, err, "insert messages")

	count, err := store.GetUsageMatchingSessionCount(ctx, db.UsageFilter{
		From:     "2026-03-12",
		To:       "2026-03-12",
		Timezone: "UTC",
		Agent:    "copilot",
		Model:    "gpt-5.3-codex",
	})
	require.NoError(t, err, "GetUsageMatchingSessionCount")
	assert.Equal(t, 1, count)
}

func TestStoreGetUsageMatchingSessionCountCountsCopilotSessionByMessageTimestampOutsideSessionWindow(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_usage_matching_sessions_late_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES
			('copilot-late-message', 'test-machine', 'proj-a', 'copilot',
			 '2026-02-08T10:00:00Z'::timestamptz,
			 '2026-02-08T10:00:00Z'::timestamptz, 1, 1),
			('copilot-out-of-range', 'test-machine', 'proj-a', 'copilot',
			 '2026-02-08T10:00:00Z'::timestamptz,
			 '2026-02-08T10:00:00Z'::timestamptz, 1, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage
		) VALUES
			('copilot-late-message', 0, 'assistant', 'copilot',
			 '2026-02-10T12:00:00Z'::timestamptz, 7,
			 'gpt-5.3-codex', ''),
			('copilot-out-of-range', 0, 'assistant', 'copilot',
			 '2026-02-08T10:00:00Z'::timestamptz, 7,
			 'gpt-5.3-codex', '')`)
	require.NoError(t, err, "insert messages")

	count, err := store.GetUsageMatchingSessionCount(ctx, db.UsageFilter{
		From:     "2026-02-10",
		To:       "2026-02-10",
		Timezone: "UTC",
		Agent:    "copilot",
	})
	require.NoError(t, err, "GetUsageMatchingSessionCount")
	assert.Equal(t, 1, count)
}

// TestStoreGetUsageMatchingSessionCountModelFilterAppliesToBoundedRow
// guards against the model/exclude-model predicate matching session-wide
// instead of on the in-range message row: a session with an out-of-range
// message on the filtered model but an in-range message on a different
// model must not match a Model filter for the out-of-range model.
func TestStoreGetUsageMatchingSessionCountModelFilterAppliesToBoundedRow(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_usage_matching_sessions_model_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES
			('copilot-mixed-model', 'test-machine', 'proj-a', 'copilot',
			 '2026-02-08T10:00:00Z'::timestamptz,
			 '2026-02-10T12:00:00Z'::timestamptz, 2, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage
		) VALUES
			('copilot-mixed-model', 0, 'assistant', 'copilot',
			 '2026-02-08T10:00:00Z'::timestamptz, 7,
			 'gpt-5.3-codex', ''),
			('copilot-mixed-model', 1, 'assistant', 'claude',
			 '2026-02-10T12:00:00Z'::timestamptz, 6,
			 'claude-sonnet', '')`)
	require.NoError(t, err, "insert messages")

	count, err := store.GetUsageMatchingSessionCount(ctx, db.UsageFilter{
		From:     "2026-02-10",
		To:       "2026-02-10",
		Timezone: "UTC",
		Agent:    "copilot",
		Model:    "gpt-5.3-codex",
	})
	require.NoError(t, err, "GetUsageMatchingSessionCount")
	assert.Equal(t, 0, count,
		"out-of-range message's model must not match the bounded window")
}

// TestStoreGetUsageMatchingSessionCountCountsAssistantMessageWithNoModel
// guards against gating matching-session eligibility on m.model != ”:
// some Copilot assistant messages parse before a model name is known, so
// an assistant message with an empty model must still count toward the
// matching-session total when no Model/ExcludeModel filter narrows it.
func TestStoreGetUsageMatchingSessionCountCountsAssistantMessageWithNoModel(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_usage_matching_sessions_no_model_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES
			('copilot-no-model', 'test-machine', 'proj-a', 'copilot',
			 '2026-02-10T10:00:00Z'::timestamptz,
			 '2026-02-10T10:00:00Z'::timestamptz, 1, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage
		) VALUES
			('copilot-no-model', 0, 'assistant', 'copilot',
			 '2026-02-10T10:00:00Z'::timestamptz, 7,
			 '', '')`)
	require.NoError(t, err, "insert messages")

	count, err := store.GetUsageMatchingSessionCount(ctx, db.UsageFilter{
		From:     "2026-02-10",
		To:       "2026-02-10",
		Timezone: "UTC",
		Agent:    "copilot",
	})
	require.NoError(t, err, "GetUsageMatchingSessionCount")
	assert.Equal(t, 1, count,
		"assistant message with no model must still count without a model filter")
}

// TestStoreGetUsageMatchingSessionCountUnboundedMatchesBoundedSemantics
// guards against the unbounded (no From/To) branch drifting from the
// bounded branch: soft-deleted sessions and sessions without
// assistant/event activity must not count, and empty-model assistant
// messages must survive an ExcludeModel filter, exactly as on the
// bounded path.
func TestStoreGetUsageMatchingSessionCountUnboundedMatchesBoundedSemantics(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_usage_matching_sessions_unbounded_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count, deleted_at
		) VALUES
			('copilot-live', 'test-machine', 'proj-a', 'copilot',
			 '2026-03-01T10:00:00Z'::timestamptz,
			 '2026-03-01T10:00:00Z'::timestamptz, 1, 1, NULL),
			('copilot-trashed', 'test-machine', 'proj-a', 'copilot',
			 '2026-03-01T10:00:00Z'::timestamptz,
			 '2026-03-01T10:00:00Z'::timestamptz, 1, 1,
			 '2026-03-02T00:00:00Z'::timestamptz),
			('copilot-user-only', 'test-machine', 'proj-a', 'copilot',
			 '2026-03-01T10:00:00Z'::timestamptz,
			 '2026-03-01T10:00:00Z'::timestamptz, 1, 1, NULL)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage
		) VALUES
			('copilot-live', 0, 'assistant', 'copilot',
			 '2026-03-01T10:00:00Z'::timestamptz, 7,
			 '', ''),
			('copilot-trashed', 0, 'assistant', 'copilot',
			 '2026-03-01T10:00:00Z'::timestamptz, 7,
			 'gpt-5.3-codex', ''),
			('copilot-user-only', 0, 'user', 'hello',
			 '2026-03-01T10:00:00Z'::timestamptz, 5,
			 '', '')`)
	require.NoError(t, err, "insert messages")

	unbounded := db.UsageFilter{Timezone: "UTC", Agent: "copilot"}
	count, err := store.GetUsageMatchingSessionCount(ctx, unbounded)
	require.NoError(t, err, "GetUsageMatchingSessionCount unbounded")
	assert.Equal(t, 1, count,
		"soft-deleted and assistant-less sessions must not count unbounded")

	bounded := unbounded
	bounded.From = "2026-03-01"
	bounded.To = "2026-03-01"
	boundedCount, err := store.GetUsageMatchingSessionCount(ctx, bounded)
	require.NoError(t, err, "GetUsageMatchingSessionCount bounded")
	assert.Equal(t, count, boundedCount,
		"bounded and unbounded requests must match the same sessions")

	excluded := unbounded
	excluded.ExcludeModel = "gpt-5.3-codex"
	excludedCount, err := store.GetUsageMatchingSessionCount(ctx, excluded)
	require.NoError(t, err, "GetUsageMatchingSessionCount exclude-model")
	assert.Equal(t, 1, excludedCount,
		"empty-model assistant message must survive an ExcludeModel filter")
}

func TestStoreGetUsageSessionCountsDedupesSourceUUIDFallback(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_counts_source_uuid_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('usage-counts-source-001', 'test-machine', 'proj-a', 'claude',
			 '2026-03-12T10:00:00Z'::timestamptz, 1, 1),
			('usage-counts-source-002', 'test-machine', 'proj-b', 'claude',
			 '2026-03-12T10:01:00Z'::timestamptz, 1, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id, source_uuid
		) VALUES
			('usage-counts-source-001', 0, 'assistant', 'one',
			 '2026-03-12T10:00:00Z'::timestamptz, 3,
			 'test-model-counts', '{"input_tokens":1}', 'msg-1', '', 'source-1'),
			('usage-counts-source-002', 0, 'assistant', 'two',
			 '2026-03-12T10:01:00Z'::timestamptz, 3,
			 'test-model-counts', '{"input_tokens":1}', 'msg-1', '', 'source-1')`)
	require.NoError(t, err, "insert messages")

	counts, err := store.GetUsageSessionCounts(ctx, db.UsageFilter{
		From:     "2026-03-12",
		To:       "2026-03-12",
		Timezone: "UTC",
	})
	require.NoError(t, err, "GetUsageSessionCounts")
	assert.Equal(t, 1, counts.Total)
	assert.Equal(t, 1, counts.ByProject["proj-a"])
	_, ok := counts.ByProject["proj-b"]
	assert.False(t, ok, "proj-b should have been deduped out: %#v", counts.ByProject)
}

func TestPostgresUsageQueriesUnionUsageEvents(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_events_union_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES
			('claude-sonnet-4-20250514', 1, 1, 1, 1, 'seed'),
			('gpt-5.4', 1, 1, 1, 1, 'seed')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('claude-msg', 'test-machine', 'proj-a', 'claude',
			 '2026-05-14T09:00:00Z'::timestamptz, 1, 1),
			('hermes-event', 'test-machine', 'proj-b', 'hermes',
			 '2026-05-14T10:00:00Z'::timestamptz, 1, 1),
			('hermes-event-2', 'test-machine', 'proj-b', 'hermes',
			 '2026-05-14T10:10:00Z'::timestamptz, 1, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage
		) VALUES
			('claude-msg', 0, 'assistant', 'one',
			 '2026-05-14T09:05:00Z'::timestamptz, 3,
			 'claude-sonnet-4-20250514',
			 '{"input_tokens":100,"output_tokens":40}')`)
	require.NoError(t, err, "insert message")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO usage_events (
			session_id, source, model, input_tokens, output_tokens,
			cache_read_input_tokens, occurred_at, dedup_key
		) VALUES
			('hermes-event', 'session', 'gpt-5.4', 300, 70, 20,
			 '2026-05-14T10:05:00Z'::timestamptz, 'shared-key'),
			('hermes-event-2', 'session', 'gpt-5.4', 50, 5, 0,
			 '2026-05-14T10:10:00Z'::timestamptz, 'shared-key')`)
	require.NoError(t, err, "insert usage event")

	filter := db.UsageFilter{
		From:       "2026-05-14",
		To:         "2026-05-14",
		Timezone:   "UTC",
		Breakdowns: true,
	}
	result, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err, "GetDailyUsage")
	assert.Equal(t, 450, result.Totals.InputTokens)
	assert.Equal(t, 115, result.Totals.OutputTokens)
	assert.Equal(t, 20, result.Totals.CacheReadTokens)
	assert.Len(t, result.Daily[0].AgentBreakdowns, 2)

	top, err := store.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(t, err, "GetTopSessionsByCost")
	require.Len(t, top, 3)
	assert.Equal(t, "hermes-event", top[0].SessionID)
	assert.Equal(t, 390, top[0].TotalTokens)

	counts, err := store.GetUsageSessionCounts(ctx, filter)
	require.NoError(t, err, "GetUsageSessionCounts")
	assert.Equal(t, 3, counts.Total)
	assert.Equal(t, 2, counts.ByAgent["hermes"])
	assert.Equal(t, 2, counts.ByProject["proj-b"])
}

func TestPostgresUsagePreservesSessionSummaryUsageEventTokens(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_summary_event_test")

	ctx := context.Background()
	rawInput := db.MaxPlausibleTokens + 250_000
	rawOutput := db.MaxPlausibleTokens + 500_000
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('gpt-5.4', 1, 2, 0, 0, 'seed')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		) VALUES (
			'hermes-summary', 'test-machine', 'proj', 'hermes',
			'2026-05-14T10:00:00Z'::timestamptz, 1, 1,
			$1, $2, true, true
		)`, rawOutput, rawInput)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO usage_events (
			session_id, message_ordinal, source, model, input_tokens,
			output_tokens, occurred_at, dedup_key
		) VALUES (
			'hermes-summary', 0, 'session', 'gpt-5.4', $1, $2,
			'2026-05-14T10:05:00Z'::timestamptz, 'session:hermes-summary'
		)`, rawInput, rawOutput)
	require.NoError(t, err, "insert usage event")

	daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:     "2026-05-14",
		To:       "2026-05-14",
		Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, daily.Daily, 1, "daily entries")
	assert.Equal(t, rawInput, daily.Totals.InputTokens, "daily input")
	assert.Equal(t, rawOutput, daily.Totals.OutputTokens, "daily output")

	usage, err := store.GetSessionUsage(ctx, "hermes-summary", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, usage, "session usage")
	assert.Equal(t, rawOutput, usage.TotalOutputTokens)
	assert.Equal(t, rawInput, usage.PeakContextTokens)
	require.True(t, usage.HasCost, "HasCost")
	wantCost := (float64(rawInput)*1.0 + float64(rawOutput)*2.0) / 1_000_000
	assert.InDelta(t, wantCost, usage.CostUSD, 1e-9, "session cost")
	require.Len(t, usage.Breakdown, 1, "Breakdown")
	entry := usage.Breakdown[0]
	assert.Equal(t, "session", entry.Source)
	assert.Equal(t, "Step 1", entry.Label)
	require.NotNil(t, entry.MessageOrdinal)
	assert.Equal(t, 0, *entry.MessageOrdinal)
	assert.Equal(t, rawInput, entry.InputTokens)
	assert.Equal(t, rawOutput, entry.OutputTokens)
	assert.True(t, entry.HasCost)
	assert.InDelta(t, wantCost, entry.CostUSD, 1e-9, "breakdown cost")
}

func TestPostgresUsageCostsMessageReasoningTokens(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_message_reasoning_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('gpt-5.4', 1, 2, 0, 0, 'seed')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'pg-message-reasoning', 'test-machine', 'proj', 'codex',
			'2026-05-14T10:00:00Z'::timestamptz, 1, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model, token_usage
		) VALUES (
			'pg-message-reasoning', 0, 'assistant', 'done',
			'2026-05-14T10:30:00Z'::timestamptz, 4,
			'gpt-5.4',
			'{"input_tokens":1000,"output_tokens":0,"reasoning_tokens":3000000000}'
		)`)
	require.NoError(t, err, "insert message")

	daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:     "2026-05-14",
		To:       "2026-05-14",
		Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, daily.Daily, 1, "daily entries")
	assert.Equal(t, 1000, daily.Totals.InputTokens)
	assert.Zero(t, daily.Totals.OutputTokens)
	assert.InDelta(t, 4.001, daily.Totals.TotalCost, 1e-12)

	usage, err := store.GetSessionUsage(ctx, "pg-message-reasoning", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, usage)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 4.001, usage.CostUSD, 1e-12)
}

func TestStoreGetDailyUsageSkipsCursorUsageForTerminationFilter(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_cursor_termination_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count, termination_status
		) VALUES (
			'clean-session', 'test-machine', 'proj', 'claude',
			'2026-05-14T10:00:00Z'::timestamptz, 1, 1, 'clean'
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model, token_usage
		) VALUES (
			'clean-session', 0, 'assistant', 'one',
			'2026-05-14T10:30:00Z'::timestamptz, 3,
			'claude-sonnet-4-20250514',
			'{"input_tokens":100,"output_tokens":40}'
		)`)
	require.NoError(t, err, "insert message")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO cursor_usage_events (
			occurred_at, model, kind, input_tokens, output_tokens,
			cache_read_tokens, charged_cents, cursor_token_fee,
			user_id, user_email, dedup_key
		) VALUES (
			'2026-05-14T10:05:00Z'::timestamptz,
			'claude-4.6-opus-high-thinking',
			'USAGE_EVENT_KIND_USAGE_BASED',
			1234, 567, 8901, 15.66, 3.32,
			'152683922', 'member@example.com', 'cursor:termination'
		)`)
	require.NoError(t, err, "insert cursor usage")

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:        "2026-05-14",
		To:          "2026-05-14",
		Timezone:    "UTC",
		Termination: "clean",
	})
	require.NoError(t, err, "GetDailyUsage clean termination")
	require.Len(t, result.Daily, 1, "daily entries")
	assert.Equal(t, 100, result.Totals.InputTokens, "InputTokens")
	assert.Equal(t, 40, result.Totals.OutputTokens, "OutputTokens")
	assert.Equal(t, 1, result.SessionCounts.Total, "SessionCounts.Total")
}

func TestPushSyncsModelPricingToPostgres(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "test-model-sync",
		InputPerMTok:         1.5,
		OutputPerMTok:        2.5,
		CacheCreationPerMTok: 3.5,
		CacheReadPerMTok:     0.5,
	}}), "UpsertModelPricing")

	ps, err := New(pgURL, "agentsview", local, "test-machine", true, SyncOptions{})
	require.NoError(t, err, "New")
	defer ps.Close()

	_, err = ps.Push(context.Background(), false, nil)
	require.NoError(t, err, "Push")

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	rows, err := store.DB().QueryContext(context.Background(), `
		SELECT model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok
		FROM model_pricing
		WHERE model_pattern = 'test-model-sync'`)
	require.NoError(t, err, "query pricing")
	defer rows.Close()

	require.True(t, rows.Next(), "expected synced pricing row")
	var (
		model                                   string
		input, output, cacheCreation, cacheRead float64
	)
	require.NoError(t, rows.Scan(
		&model, &input, &output, &cacheCreation, &cacheRead,
	), "scan pricing")
	assert.Equal(t, "test-model-sync", model)
	assert.Equal(t, 1.5, input)
	assert.Equal(t, 2.5, output)
	assert.Equal(t, 3.5, cacheCreation)
	assert.Equal(t, 0.5, cacheRead)
}

func TestPushFallsBackToBuiltinPricingWhenLocalTableEmpty(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, "agentsview", local, "test-machine", true, SyncOptions{})
	require.NoError(t, err, "New")
	defer ps.Close()

	_, err = ps.Push(context.Background(), false, nil)
	require.NoError(t, err, "Push")

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	rows, err := store.DB().QueryContext(context.Background(), `
		SELECT model_pattern
		FROM model_pricing
		ORDER BY model_pattern`)
	require.NoError(t, err, "query pricing")
	defer rows.Close()

	var models []string
	for rows.Next() {
		var model string
		require.NoError(t, rows.Scan(&model), "scan model")
		models = append(models, model)
	}
	require.NoError(t, rows.Err(), "rows err")
	joined := strings.Join(models, ",")
	assert.Contains(t, joined, "claude-sonnet-4-20250514",
		"fallback pricing not synced")
}

func TestStoreGetSessionUsage_CopilotAICreditsComputed(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_copilot_credits_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'copilot:s1', 'test-machine', 'proj', 'copilot',
			'2026-03-12T10:00:00Z'::timestamptz, 1, 1
		)`)
	require.NoError(t, err, "insert session")

	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO usage_events (
			session_id, source, model, input_tokens, output_tokens, cost_usd, occurred_at
		) VALUES (
			'copilot:s1', 'api', 'gpt-4', 1000, 500, 0.10, '2026-03-12T10:00:00Z'::timestamptz
		)`)
	require.NoError(t, err, "insert usage event")

	u, err := store.GetSessionUsage(ctx, "copilot:s1", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	assert.True(t, u.HasCost, "HasCost")
	assert.Equal(t, 0.10, u.CostUSD, "CostUSD")
	assert.Equal(t, 10.0, u.AICredits, "AICredits")
}

func TestStoreGetSessionUsage_CopilotReportedCost(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_copilot_reported_cost_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'copilot:reported', 'test-machine', 'proj', 'copilot',
			'2026-03-12T10:00:00Z'::timestamptz, 1, 1
		)`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO usage_events (
			session_id, source, model, input_tokens, output_tokens,
			cost_usd, cost_status, cost_source, occurred_at, dedup_key
		) VALUES
			('copilot:reported', 'shutdown', 'gpt-4', 1000, 500,
			 NULL, '', '', '2026-03-12T10:01:00Z'::timestamptz, 'segment-1'),
			('copilot:reported', 'shutdown', 'gpt-4', 1000, 500,
			 0.0275, 'exact', 'copilot-reported',
			 '2026-03-13T10:02:00Z'::timestamptz, 'segment-2')`)
	require.NoError(t, err)

	usage, err := store.GetSessionUsage(ctx, "copilot:reported", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.InDelta(t, 0.0275, usage.CostUSD, 1e-12)
	assert.InDelta(t, 2.75, usage.AICredits, 1e-12)
	require.Len(t, usage.Breakdown, 2)
	assert.NotEqual(t, usage.CostUSD, usage.Breakdown[1].CostUSD)

	daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-03-12", To: "2026-03-13", Timezone: "UTC",
	})
	require.NoError(t, err)
	require.Len(t, daily.Daily, 2)
	assert.Zero(t, daily.Daily[0].TotalCost)
	assert.InDelta(t, 0.0275, daily.Daily[1].TotalCost, 1e-12)
	assert.InDelta(t, 0.0275, daily.Totals.TotalCost, 1e-12)
	assert.InDelta(t, 2.75, daily.Totals.CopilotAICredits, 1e-12)
}

func TestStoreGetSessionUsage_CopilotNoAICreditsUnpriced(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_copilot_unpriced_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'copilot:s2', 'test-machine', 'proj', 'copilot',
			'2026-03-12T10:00:00Z'::timestamptz, 1, 1
		)`)
	require.NoError(t, err, "insert session")

	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO usage_events (
			session_id, source, model, input_tokens, output_tokens, occurred_at
		) VALUES (
			'copilot:s2', 'api', 'local-model', 1000, 500, '2026-03-12T10:00:00Z'::timestamptz
		)`)
	require.NoError(t, err, "insert usage event")

	u, err := store.GetSessionUsage(ctx, "copilot:s2", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	assert.False(t, u.HasCost, "HasCost should be false")
	assert.Equal(t, 0.0, u.AICredits, "AICredits should be 0 when unpriced")
}
