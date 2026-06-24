//go:build pgtest

package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
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
			('usage-breakdown-001', 'test-machine', 'proj-a', 'claude',
			 '2026-03-12T10:00:00Z'::timestamptz, 1, 1),
			('usage-breakdown-002', 'test-machine', 'proj-b', 'codex',
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

	got, err := store.GetSessionUsage(ctx, "codex:usage-priced")
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

	got, err := store.GetSessionUsage(ctx, "claude:usage-source")
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, got, "GetSessionUsage result")
	assert.True(t, got.HasCost)
	assert.InDelta(t, 0.0175, got.CostUSD, 1e-9)
	assert.Equal(t, []string{"claude-opus-4-6"}, got.Models)
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

	got, err := store.GetSessionUsage(ctx, "codex:usage-empty")
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
}

func TestStoreGetSessionUsageNotFound(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_missing_test")

	got, err := store.GetSessionUsage(context.Background(), "missing")
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

	u, err := store.GetSessionUsage(ctx, "copilot:s1")
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	assert.True(t, u.HasCost, "HasCost")
	assert.Equal(t, 0.10, u.CostUSD, "CostUSD")
	assert.Equal(t, 10.0, u.AICredits, "AICredits")
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

	u, err := store.GetSessionUsage(ctx, "copilot:s2")
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	assert.False(t, u.HasCost, "HasCost should be false")
	assert.Equal(t, 0.0, u.AICredits, "AICredits should be 0 when unpriced")
}
