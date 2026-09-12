//go:build pgtest

// ABOUTME: End-to-end PostgreSQL coverage for the Anthropic web search fee,
// ABOUTME: read from the pushed token_usage blob like SQLite reads it.
package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

// seedWebSearchUsage inserts one Claude session with two identical assistant
// turns, the first billed for two server-side web searches. Pricing is
// $1/MTok in and $2/MTok out, so each turn's token cost is exactly $0.30.
func seedWebSearchUsage(t *testing.T, store *Store, model string) {
	t.Helper()
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok,
			output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		) VALUES ('claude-websearch-test', 1000000, 2000000, 0, 0, '2026-01-01T00:00:00Z')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'pg-websearch', 'test-machine', 'proj', 'claude',
			'2026-07-30T10:00:00Z'::timestamptz, 2, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
		(
			'pg-websearch', 0, 'assistant', 'searched',
			'2026-07-30T10:01:00Z'::timestamptz, 8, $1,
			'{"input_tokens":100000,"output_tokens":100000,"server_tool_use":{"web_search_requests":2,"web_fetch_requests":0}}',
			'm-0', 'r-0'
		),
		(
			'pg-websearch', 1, 'assistant', 'answered',
			'2026-07-30T10:02:00Z'::timestamptz, 8, $1,
			'{"input_tokens":100000,"output_tokens":100000,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0}}',
			'm-1', 'r-1'
		)`, model)
	require.NoError(t, err, "insert messages")
}

func TestStoreSessionUsageBillsWebSearchRequests(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_websearch_test")
	seedWebSearchUsage(t, store, "claude-websearch-test")
	ctx := context.Background()

	got, err := store.GetSessionUsage(ctx, "pg-websearch", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, got)
	require.True(t, got.HasCost)
	// Two turns at $0.30 of tokens each, plus two searches at $0.01.
	assert.Equal(t, money.MustParseDollars("0.62"), got.Cost)
	require.Len(t, got.Breakdown, 2)
	assert.Equal(t, 2, got.Breakdown[0].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.32"), got.Breakdown[0].Cost)
	assert.Zero(t, got.Breakdown[1].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.30"), got.Breakdown[1].Cost)

	daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-07-30", To: "2026-07-30", Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	assert.Equal(t, money.MustParseDollars("0.62"), daily.Totals.TotalCost)

	rowSet, err := store.GetSessionUsageRows(ctx, []string{"pg-websearch"})
	require.NoError(t, err, "GetSessionUsageRows")
	rows := rowSet.Rows
	require.Len(t, rows, 2)
	assert.Equal(t, 2, rows[0].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.32"), rows[0].Cost)
	assert.Zero(t, rows[1].WebSearchRequests)
}

func TestPGAsymmetricClaudeSnapshotsPreserveWebSearchFee(t *testing.T) {
	_, store := prepareUsageSchema(
		t, "agentsview_usage_websearch_asymmetric_test")
	seedWebSearchUsage(t, store, "claude-websearch-test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES (
			'pg-websearch', 2, 'assistant', 'fuller snapshot',
			'2026-07-30T10:03:00Z'::timestamptz, 15,
			'claude-websearch-test',
			'{"input_tokens":100000,"output_tokens":200000,"server_tool_use":{"web_search_requests":0}}',
			'm-0', 'r-0'
		)`)
	require.NoError(t, err, "insert fuller snapshot")

	usage, err := store.GetSessionUsage(ctx, "pg-websearch", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, money.MustParseDollars("0.82"), usage.Cost)
	require.Len(t, usage.Breakdown, 2)
	assert.Equal(t, 2, usage.Breakdown[1].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.52"),
		usage.Breakdown[1].Cost)

	daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-07-30", To: "2026-07-30", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.82"),
		daily.Totals.TotalCost)

	report, err := store.GetActivityReport(ctx,
		db.AnalyticsFilter{Timezone: "UTC"},
		pgDayQuery(t, "2026-07-30", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.82"), report.Totals.Cost)

	rowSet, err := store.GetSessionUsageRows(ctx, []string{"pg-websearch"})
	require.NoError(t, err)
	require.Len(t, rowSet.Rows, 2)
	assert.Equal(t, 2, rowSet.Rows[1].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.52"), rowSet.Rows[1].Cost)
}

func TestPGUsageJSONExtractionUsesExactPathsAndMalformedFallback(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_json_paths_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok,
			output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		) VALUES ('claude-json-path-test', 0, 1000000, 0, 0, '2026-01-01T00:00:00Z');
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'pg-json-paths', 'test-machine', 'proj', 'claude',
			'2026-07-30T10:00:00Z'::timestamptz, 4, 1
		);
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
		(
			'pg-json-paths', 0, 'assistant', 'partial',
			'2026-07-30T10:01:00Z'::timestamptz, 7,
			'claude-json-path-test',
			'{"metadata":{"output_tokens":900000,"web_search_requests":9},"input_tokens":0,"output_tokens":100000,"server_tool_use":{"web_search_requests":1}}',
			'm-scoped', 'r-scoped'
		),
		(
			'pg-json-paths', 1, 'assistant', 'complete',
			'2026-07-30T10:02:00Z'::timestamptz, 8,
			'claude-json-path-test',
			'{"metadata":{"output_tokens":1,"web_search_requests":8},"input_tokens":0,"output_tokens":200000,"server_tool_use":{"web_search_requests":2}}',
			'm-scoped', 'r-scoped'
		),
		(
			'pg-json-paths', 2, 'assistant', 'truncated',
			'2026-07-30T10:03:00Z'::timestamptz, 9,
			'claude-json-path-test',
			'{"metadata":{"output_tokens":700000,"web_search_requests":7},"input_tokens":0,"output_tokens":300000,"server_tool_use":{"web_search_requests":3',
			'm-truncated', 'r-truncated'
		),
		(
			'pg-json-paths', 3, 'assistant', 'complete comparison',
			'2026-07-30T10:04:00Z'::timestamptz, 19,
			'claude-json-path-test',
			'{"input_tokens":0,"output_tokens":400000,"server_tool_use":{"web_search_requests":1}}',
			'm-truncated', 'r-truncated'
		)`)
	require.NoError(t, err, "seed usage")

	daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-07-30", To: "2026-07-30", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 600000, daily.Totals.OutputTokens)
	assert.Equal(t, money.MustParseDollars("0.63"), daily.Totals.TotalCost)
}

// An unpriced model still owes the flat fee, which does not depend on token
// rates, while the session stays reported as an incomplete estimate.
func TestStoreSessionUsageBillsWebSearchOnUnpricedModel(t *testing.T) {
	_, store := prepareUsageSchema(
		t, "agentsview_usage_websearch_unpriced_test")
	seedWebSearchUsage(t, store, "some-unlisted-model")
	ctx := context.Background()

	got, err := store.GetSessionUsage(ctx, "pg-websearch", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, got)
	assert.False(t, got.HasCost)
	assert.Equal(t, []string{"some-unlisted-model"}, got.UnpricedModels)
	require.Len(t, got.Breakdown, 2)
	assert.Equal(t, money.MustParseDollars("0.02"), got.Breakdown[0].Cost)
	assert.False(t, got.Breakdown[0].HasCost)
	assert.Equal(t, 2, got.Breakdown[0].WebSearchRequests)
	assert.Equal(t, money.Money{}, got.Breakdown[1].Cost)

	daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-07-30", To: "2026-07-30", Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	assert.Equal(t, money.MustParseDollars("0.02"), daily.Totals.TotalCost)
}
