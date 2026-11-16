//go:build pgtest

// ABOUTME: End-to-end PostgreSQL coverage for the Anthropic web search fee,
// ABOUTME: read from the counters push stores from token_usage, as SQLite reads it.
package postgres

import (
	"strings"
	"testing"
	"time"

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
	ctx := t.Context()
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
	_, err = backfillMessageUsageCountersPG(ctx, store.DB())
	require.NoError(t, err, "store usage counters as push does")
}

func TestStoreSessionUsageBillsWebSearchRequests(t *testing.T) {
	t.Parallel()
	_, store := prepareUsageSchema(t, usageTestSchema)
	seedWebSearchUsage(t, store, "claude-websearch-test")
	ctx := t.Context()

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
	t.Parallel()
	_, store := prepareUsageSchema(t, usageTestSchema)
	seedWebSearchUsage(t, store, "claude-websearch-test")
	ctx := t.Context()
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
	_, err = backfillMessageUsageCountersPG(ctx, store.DB())
	require.NoError(t, err, "store usage counters as push does")

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
	t.Parallel()
	_, store := prepareUsageSchema(t, usageTestSchema)
	ctx := t.Context()
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
	_, err = backfillMessageUsageCountersPG(ctx, store.DB())
	require.NoError(t, err, "store usage counters as push does")

	daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-07-30", To: "2026-07-30", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 600000, daily.Totals.OutputTokens)
	// $0.60 of output plus three searches: the larger count (2) for
	// m-scoped and 1 for m-truncated. The truncated row's server_tool_use
	// object never closes, so it bills no searches, as SQLite reads it.
	assert.Equal(t, money.MustParseDollars("0.63"), daily.Totals.TotalCost)
}

// An unpriced model still owes the flat fee, which does not depend on token
// rates, while the session stays reported as an incomplete estimate.
func TestStoreSessionUsageBillsWebSearchOnUnpricedModel(t *testing.T) {
	t.Parallel()
	_, store := prepareUsageSchema(t, usageTestSchema)
	seedWebSearchUsage(t, store, "some-unlisted-model")
	ctx := t.Context()

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

// A binary older than the stored usage counters pushes messages without them.
// The next push's schema check fills them, so those messages bill their web
// searches and rank their Claude snapshots by output again.
func TestPGPushSchemaCheckFillsMissingUsageCounters(t *testing.T) {
	const schema = "agentsview_usage_counter_repair_test"
	_, store := prepareUsageSchema(t, schema)
	seedWebSearchUsage(t, store, "claude-websearch-test")
	ctx := t.Context()
	filter := db.UsageFilter{From: "2026-07-30", To: "2026-07-30", Timezone: "UTC"}
	// An older binary writes the message without counters and bumps its
	// session, as every publisher does with a message write.
	olderPublisherWrite := func() {
		t.Helper()
		_, err := store.DB().ExecContext(ctx, `
			UPDATE messages
			SET usage_output_tokens = NULL, usage_web_search_requests = NULL`)
		require.NoError(t, err)
		_, err = store.DB().ExecContext(ctx, `UPDATE sessions SET updated_at = NOW()`)
		require.NoError(t, err)
	}
	olderPublisherWrite()
	require.True(t, pushSchemaCurrent(ctx, store.DB()),
		"the per-push repair path must run, not a full schema setup")

	syncer := &Sync{pg: store.DB(), schema: schema}
	require.NoError(t, syncer.EnsureSchema(ctx))
	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.62"), daily.Totals.TotalCost)

	// The same Sync runs the next push. Usage read before it sees the
	// older binary's message without its web searches; the push fills the
	// counters and the read after it sees them.
	olderPublisherWrite()
	daily, err = store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.60"), daily.Totals.TotalCost)
	require.NoError(t, syncer.EnsureSchema(ctx))

	var outputTokens, webSearchRequests int64
	require.NoError(t, store.DB().QueryRowContext(ctx, `
		SELECT usage_output_tokens, usage_web_search_requests
		FROM messages WHERE session_id = 'pg-websearch' AND ordinal = 0`,
	).Scan(&outputTokens, &webSearchRequests))
	assert.EqualValues(t, 100000, outputTokens)
	assert.EqualValues(t, 2, webSearchRequests)
	daily, err = store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.62"), daily.Totals.TotalCost)
}

// A publisher locks a session and then its messages, while the counter fill
// locks messages and then their session. When the two meet, PostgreSQL aborts
// the fill as a deadlock. The push must go on, whether the fill ran in a new
// push process's first schema check or a later one, and a later push fills the
// counters.
func TestPGPushSchemaCheckSurvivesCounterFillDeadlock(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		schemaDone   bool
	}{
		{"first push", "agentsview_usage_counter_deadlock_first_test", false},
		{"later push", "agentsview_usage_counter_deadlock_test", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store := prepareUsageSchema(t, tc.schema)
			ctx := t.Context()
			var version string
			require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT version()`).Scan(&version))
			if strings.Contains(version, "CockroachDB") {
				t.Skip("CockroachDB resolves lock conflicts by transaction priority, not deadlock detection")
			}
			seedWebSearchUsage(t, store, "claude-websearch-test")
			if !tc.schemaDone {
				// A new push process checks a current schema through its data
				// repairs, which run the fill before the first push. One check
				// now settles the other repairs, such as classifying the seeded
				// session, so the fill is the only one left to meet the
				// publisher.
				require.True(t, pushSchemaCurrent(ctx, store.DB()))
				require.NoError(t, (&Sync{pg: store.DB(), schema: tc.schema}).EnsureSchema(ctx))
			}
			_, err := store.DB().ExecContext(ctx, `
				UPDATE messages
				SET usage_output_tokens = NULL, usage_web_search_requests = NULL`)
			require.NoError(t, err)

			publisher, err := store.DB().BeginTx(ctx, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = publisher.Rollback() })
			_, err = publisher.ExecContext(ctx,
				`UPDATE sessions SET updated_at = NOW() WHERE id = 'pg-websearch'`)
			require.NoError(t, err)

			syncer := &Sync{pg: store.DB(), schema: tc.schema, schemaDone: tc.schemaDone}
			fill := make(chan error, 1)
			go func() { fill <- syncer.EnsureSchema(ctx) }()
			// The fill now holds the session's message rows and waits for its
			// session row, which the publisher holds.
			require.Eventually(t, func() bool {
				var waiting bool
				err := store.DB().QueryRowContext(ctx, `
					SELECT EXISTS (
						SELECT 1 FROM pg_stat_activity
						WHERE datname = current_database() AND pid <> pg_backend_pid()
						  AND wait_event_type = 'Lock'
						  AND query LIKE '%usage_output_tokens = v.output_tokens%')`,
				).Scan(&waiting)
				return err == nil && waiting
			}, 10*time.Second, 10*time.Millisecond)
			_, publishErr := publisher.ExecContext(ctx,
				`UPDATE messages SET content = content WHERE session_id = 'pg-websearch'`)
			require.NoError(t, <-fill, "a deadlocked counter fill must not fail the push")
			require.NoError(t, publishErr)
			require.NoError(t, publisher.Commit())

			require.NoError(t, syncer.EnsureSchema(ctx))
			var outputTokens, webSearchRequests int64
			require.NoError(t, store.DB().QueryRowContext(ctx, `
				SELECT usage_output_tokens, usage_web_search_requests
				FROM messages WHERE session_id = 'pg-websearch' AND ordinal = 0`,
			).Scan(&outputTokens, &webSearchRequests))
			assert.EqualValues(t, 100000, outputTokens)
			assert.EqualValues(t, 2, webSearchRequests)
		})
	}
}
