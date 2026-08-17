//go:build pgtest

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
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
	assert.Equal(t, money.MustParseDollars("6"), result.Totals.TotalCost)
	assert.Len(t, result.Daily, 1)
}

func TestStoreGetDailyUsageReturnsAggregateCostOverflow(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_overflow_test")
	ctx := t.Context()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'usage-overflow', 'test-machine', 'proj', 'claude',
			'2026-07-26T12:00:00Z'::timestamptz, 1, 1
		);
		INSERT INTO usage_events (
			session_id, source, model, cost_microdollars, occurred_at, dedup_key
		) VALUES
			('usage-overflow', 'provider', 'model', 4611686018427387904,
			 '2026-07-26T12:00:00Z'::timestamptz, 'overflow-1'),
			('usage-overflow', 'provider', 'model', 4611686018427387904,
			 '2026-07-26T12:01:00Z'::timestamptz, 'overflow-2')`)
	require.NoError(t, err)

	_, err = store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-07-26", To: "2026-07-26", Timezone: "UTC",
	})

	require.ErrorIs(t, err, money.ErrOverflow)
}

func TestStoreGetDailyUsageWithBreakdowns(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_breakdown_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES
			('test-model-a', 1000000, 2000000, 3000000, 500000, '2026-01-01T00:00:00Z'),
			('test-model-b', 2000000, 4000000, 0, 0, '2026-01-01T00:00:00Z')`)
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
	assert.Equal(t, day.TotalCost, money.MustAdd(
		day.MachineBreakdowns[0].Cost, day.MachineBreakdowns[1].Cost))
	assert.Positive(t, day.TotalCost.Microdollars)

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

func TestStoreGetDailyUsageAppliesPricingBandsOnlyToRequests(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_pricing_band_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok,
			output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		) VALUES ('banded-model', 1000000, 0, 0, 0, '2026-01-01T00:00:00Z');
		INSERT INTO model_pricing_bands (
			model_pattern, above_input_tokens,
			input_microdollars_per_mtok,
			output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		) VALUES ('banded-model', 200000, 2000000, 0, 0, 0, '2026-01-01T00:00:00Z');
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'pricing-band', 'test-machine', 'proj', 'codex',
			'2026-03-12T10:00:00Z'::timestamptz, 1, 1
		);
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model, token_usage
		) VALUES (
			'pricing-band', 0, 'assistant', 'request',
			'2026-03-12T10:00:00Z'::timestamptz, 7,
			'banded-model', '{"input_tokens":300000}'
		);
		INSERT INTO usage_events (
			session_id, source, model, input_tokens,
			occurred_at, dedup_key
		) VALUES (
			'pricing-band', 'aggregate', 'banded-model', 300000,
			'2026-03-12T10:01:00Z'::timestamptz, 'aggregate'
		)`)
	require.NoError(t, err)

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-03-12", To: "2026-03-12", Timezone: "UTC",
	})
	require.NoError(t, err)

	assert.Equal(t, 600_000, result.Totals.InputTokens)
	assert.Equal(t, money.Money{Microdollars: 900_000},
		result.Totals.TotalCost)
	require.NotNil(t, result.Pricing)
	provenance := result.Pricing.Models["banded-model"]
	require.Len(t, provenance.Resolutions, 1)
	assert.Equal(t, export.PricingApplication{
		AggregateRowCount: 1,
		Bands: []export.AppliedPricingBand{{
			AboveInputTokens: 200_000,
			RequestCount:     1,
		}},
	}, provenance.Resolutions[0].Application)
}

func TestStoreGetDailyUsagePricesGooseRequestAsRequestScoped(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_goose_request_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok,
			output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		) VALUES ('banded-model', 1000000, 0, 0, 0, '2026-01-01T00:00:00Z');
		INSERT INTO model_pricing_bands (
			model_pattern, above_input_tokens,
			input_microdollars_per_mtok,
			output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		) VALUES ('banded-model', 200000, 2000000, 0, 0, 0, '2026-01-01T00:00:00Z');
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'goose:banded', 'test-machine', 'proj', 'goose',
			'2026-03-12T10:00:00Z'::timestamptz, 1, 1
		);
		INSERT INTO usage_events (
			session_id, source, model, input_tokens,
			occurred_at, dedup_key
		) VALUES (
			'goose:banded', 'goose-request', 'banded-model', 300000,
			'2026-03-12T10:01:00Z'::timestamptz, 'goose-request'
		)`)
	require.NoError(t, err)

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-03-12", To: "2026-03-12", Timezone: "UTC",
	})
	require.NoError(t, err)

	assert.Equal(t, 300_000, result.Totals.InputTokens)
	assert.Equal(t, money.Money{Microdollars: 600_000},
		result.Totals.TotalCost,
		"goose-request events must be priced per request, not at aggregate base rates")
	require.NotNil(t, result.Pricing)
	provenance := result.Pricing.Models["banded-model"]
	require.Len(t, provenance.Resolutions, 1)
	assert.Equal(t, export.PricingApplication{
		Bands: []export.AppliedPricingBand{{
			AboveInputTokens: 200_000,
			RequestCount:     1,
		}},
	}, provenance.Resolutions[0].Application)
}

func TestStoreGetDailyUsageDedupesBySourceUUIDWhenClaudePairIncomplete(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_source_uuid_daily_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('test-model-source-daily', 1000000, 2000000, 3000000, 500000, '2026-01-01T00:00:00Z')`)
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

func TestStoreUsageAggregatesPreferCompleteClaudeSnapshotAcrossSessions(t *testing.T) {
	_, store := prepareUsageSchema(
		t, "agentsview_daily_usage_streamed_snapshot_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('claude-opus-4-6', 5000000, 25000000, 6250000, 500000, '2026-01-01T00:00:00Z');
		INSERT INTO sessions (
			id, machine, project, agent, display_name, started_at,
			message_count, user_message_count
		) VALUES
		(
			'claude:daily-streamed', 'parent-machine', 'parent-project', 'parent-agent',
			'parent display',
			'2026-05-20T10:00:00Z'::timestamptz, 1, 1
		),
		(
			'agent-daily-streamed', 'child-machine', 'child-project', 'child-agent',
			'child display',
			'2026-05-20T10:31:00Z'::timestamptz, 2, 0
		);
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
			('claude:daily-streamed', 0, 'assistant', 'partial',
			 '2026-05-20T10:30:00Z'::timestamptz, 7,
			 'claude-opus-4-6', '{"input_tokens":1000,"output_tokens":5}',
			 'msg-stream', 'req-stream'),
			('agent-daily-streamed', 0, 'assistant', 'complete',
			 '2026-05-20T10:31:00Z'::timestamptz, 8,
			 'claude-opus-4-6', '{"input_tokens":1000,"output_tokens":631}',
			 'msg-stream', 'req-stream'),
			('agent-daily-streamed', 1, 'assistant', 'next-day',
			 '2026-05-21T00:00:00Z'::timestamptz, 8,
			 'claude-opus-4-6', '{"input_tokens":1000,"output_tokens":999}',
			 'msg-stream', 'req-stream')`)
	require.NoError(t, err)

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC", Breakdowns: true,
	})
	require.NoError(t, err)
	require.Len(t, result.Daily, 1)
	assert.Equal(t, 1000, result.Totals.InputTokens)
	assert.Equal(t, 631, result.Totals.OutputTokens)
	require.Len(t, result.Daily[0].ProjectBreakdowns, 1)
	assert.Equal(t, "parent-project", result.Daily[0].ProjectBreakdowns[0].Project)
	require.Len(t, result.Daily[0].AgentBreakdowns, 1)
	assert.Equal(t, "parent-agent", result.Daily[0].AgentBreakdowns[0].Agent)
	require.Len(t, result.Daily[0].MachineBreakdowns, 1)
	assert.Equal(t, "parent-machine", result.Daily[0].MachineBreakdowns[0].MachineName)

	top, err := store.GetTopSessionsByCost(ctx, db.UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
	}, 10)
	require.NoError(t, err)
	require.Len(t, top, 1)
	assert.Equal(t, "claude:daily-streamed", top[0].SessionID)
	assert.Equal(t, "parent display", top[0].DisplayName)
	assert.Equal(t, "parent-project", top[0].Project)
	assert.Equal(t, "parent-agent", top[0].Agent)
	assert.Equal(t, "2026-05-20T10:00:00Z", top[0].StartedAt)
	assert.Equal(t, 1000, top[0].InputTokens)
	assert.Equal(t, 631, top[0].OutputTokens)

	filtered, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
		ProjectLabels: []string{"parent-project"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1000, filtered.Totals.InputTokens)
	assert.Equal(t, 631, filtered.Totals.OutputTokens,
		"the attributed parent filter must retain the complete child snapshot")

	filteredTop, err := store.GetTopSessionsByCost(ctx, db.UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
		ProjectLabels: []string{"parent-project"},
	}, 10)
	require.NoError(t, err)
	require.Len(t, filteredTop, 1)
	assert.Equal(t, 631, filteredTop[0].OutputTokens)

	childFiltered, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
		ProjectLabels: []string{"child-project"},
	})
	require.NoError(t, err)
	assert.Zero(t, childFiltered.Totals.OutputTokens,
		"the source child metadata must not override parent attribution")
}

func TestStoreGetDailyUsagePrefersTimestampedEqualClaudeSnapshot(t *testing.T) {
	_, store := prepareUsageSchema(
		t, "agentsview_daily_usage_null_snapshot_timestamp_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok,
			output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		) VALUES ('claude-opus-4-6', 5000000, 25000000, 6250000, 500000, '2026-01-01T00:00:00Z');
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('a-null-snapshot', 'test-machine', 'proj', 'claude',
			 NULL, 1, 0),
			('z-timestamped-snapshot', 'test-machine', 'proj', 'claude',
			 '2026-05-20T10:00:00Z'::timestamptz, 1, 0);
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
			('a-null-snapshot', 0, 'assistant', 'missing timestamp', NULL, 17,
			 'claude-opus-4-6', '{"input_tokens":10,"output_tokens":100}',
			 'msg-null-ts', 'req-null-ts'),
			('z-timestamped-snapshot', 0, 'assistant', 'timestamped',
			 '2026-05-20T10:30:00Z'::timestamptz, 11,
			 'claude-opus-4-6', '{"input_tokens":900,"output_tokens":100}',
			 'msg-null-ts', 'req-null-ts')`)
	require.NoError(t, err)

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{Timezone: "UTC"})
	require.NoError(t, err)
	assert.Equal(t, 900, result.Totals.InputTokens)
	assert.Equal(t, 100, result.Totals.OutputTokens)
}

func TestStoreUsageRanksNumericStringClaudeSnapshot(t *testing.T) {
	_, store := prepareUsageSchema(
		t, "agentsview_daily_usage_numeric_string_snapshot_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('claude-opus-4-6', 5000000, 25000000, 6250000, 500000, '2026-01-01T00:00:00Z');
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('numeric-parent', 'test-machine', 'proj', 'claude-code',
			 '2026-05-20T10:00:00Z'::timestamptz, 1, 1),
			('numeric-child', 'test-machine', 'proj', 'claude-code',
			 '2026-05-20T10:01:00Z'::timestamptz, 1, 0);
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
			('numeric-parent', 0, 'assistant', 'partial',
			 '2026-05-20T10:00:00Z'::timestamptz, 7,
			 'claude-opus-4-6', '{"input_tokens":1000,"output_tokens":5}',
			 'msg-numeric-string', 'req-numeric-string'),
			('numeric-child', 0, 'assistant', 'complete',
			 '2026-05-20T10:01:00Z'::timestamptz, 8,
			 'claude-opus-4-6', '{"input_tokens":"1000","output_tokens":"631"}',
			 'msg-numeric-string', 'req-numeric-string')`)
	require.NoError(t, err)

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 1000, result.Totals.InputTokens)
	assert.Equal(t, 631, result.Totals.OutputTokens)
}

func TestStoreGetSessionUsagePricedModel(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_priced_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('gpt-5.1', 3000000, 15000000, 3750000, 300000, '2026-01-01T00:00:00Z')`)
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
	assert.Equal(t, money.MustParseDollars("0.01134"), got.Cost)
	// cost_usd is a deprecated compatibility alias for
	// cost.microdollars/1e6; PostgreSQL must report the same value as
	// SQLite and DuckDB for the same cost (see db.CostUSDFromCost).
	require.NotNil(t, got.CostUSD)
	assert.InDelta(t, 0.01134, *got.CostUSD, 1e-9)
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
	assert.Equal(t, money.MustParseDollars("0.01134"), entry.Cost)
}

func TestStoreSessionUsageRollupParity(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_rollup_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('gpt-5.1', 3000000, 15000000, 3750000, 300000, '2026-01-01T00:00:00Z')`)
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
	assert.Equal(t, money.MustParseDollars("0.021"), rollup.Cost)
}

func TestStoreSessionUsageRollupUsesCopilotReportedSessionCost(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_rollup_copilot_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('gpt-5.1', 3000000, 15000000, 3750000, 300000, '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, message_count,
			user_message_count, parent_session_id, relationship_type
		) VALUES
			('pg-copilot-rollup-root', 'test', 'project', 'copilot',
			 '2026-03-12T10:00:00Z', 1, 1, NULL, 'root'),
			('pg-copilot-rollup-child', 'test', 'project', 'copilot',
			 '2026-03-12T10:02:00Z', 1, 1,
			 'pg-copilot-rollup-root', 'subagent')`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO usage_events (
			session_id, source, model, input_tokens, output_tokens,
			cost_microdollars, cost_status, cost_source, occurred_at, dedup_key
		) VALUES
			('pg-copilot-rollup-root', 'shutdown', 'gpt-5.1', 1000, 500,
			 NULL, '', '', '2026-03-12T10:01:00Z', 'first'),
			('pg-copilot-rollup-root', 'shutdown', 'gpt-5.1', 1000, 500,
			 30000, 'exact', 'copilot-reported', '2026-03-12T10:02:00Z', 'final'),
			('pg-copilot-rollup-child', 'provider', 'gpt-5.1', 0, 0,
			 20000, 'exact', 'provider', '2026-03-12T10:03:00Z', 'child')`)
	require.NoError(t, err)

	rollup, err := service.GetSessionUsageRollup(
		ctx, store, "pg-copilot-rollup-root", false)
	require.NoError(t, err)
	require.True(t, rollup.HasCost)
	assert.Equal(t, money.MustParseDollars("0.05"), rollup.Cost)
}

func TestStoreSessionUsageRollupIncludesUntimedRows(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_rollup_untimed_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('gpt-5.1', 3000000, 15000000, 3750000, 300000, '2026-01-01T00:00:00Z')`)
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
	assert.Equal(t, money.MustParseDollars("0.021"), rollup.Cost)
}

func TestStoreGetSessionUsageDedupesSourceUUIDWhenClaudePairIncomplete(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_source_uuid_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('claude-opus-4-6', 5000000, 25000000, 6250000, 500000, '2026-01-01T00:00:00Z')`)
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
	assert.Equal(t, money.MustParseDollars("0.0175"), got.Cost)
	assert.Equal(t, []string{"claude-opus-4-6"}, got.Models)
	require.Len(t, got.Breakdown, 1, "Breakdown")
	require.NotNil(t, got.Breakdown[0].MessageOrdinal)
	assert.Equal(t, 0, *got.Breakdown[0].MessageOrdinal)
}

func TestStoreGetSessionUsagePrefersCompleteClaudeSnapshot(t *testing.T) {
	_, store := prepareUsageSchema(
		t, "agentsview_session_usage_streamed_snapshot_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('claude-opus-4-6', 5000000, 25000000, 6250000, 500000, '2026-01-01T00:00:00Z');
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count,
			total_output_tokens, has_total_output_tokens
		) VALUES (
			'claude:streamed', 'test-machine', 'proj', 'claude-code',
			'2026-03-12T10:00:00Z'::timestamptz, 2, 1, 636, TRUE
		);
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
			('claude:streamed', 0, 'assistant', 'partial',
			 '2026-03-12T10:01:00Z'::timestamptz, 7,
			 'claude-opus-4-6', '{"input_tokens":1000,"output_tokens":5}',
			 'msg-stream', 'req-stream'),
			('claude:streamed', 1, 'assistant', 'complete',
			 '2026-03-12T10:02:00Z'::timestamptz, 8,
			 'claude-opus-4-6', '{"input_tokens":1000,"output_tokens":631}',
			 'msg-stream', 'req-stream')`)
	require.NoError(t, err)

	got, err := store.GetSessionUsage(ctx, "claude:streamed", true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 631, got.TotalOutputTokens)
	assert.Equal(t, money.MustParseDollars("0.020775"), got.Cost)
	require.Len(t, got.Breakdown, 1)
	assert.Equal(t, 631, got.Breakdown[0].OutputTokens)
	require.NotNil(t, got.Breakdown[0].MessageOrdinal)
	assert.Equal(t, 1, *got.Breakdown[0].MessageOrdinal)
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
	assert.Zero(t, got.Cost)
	assert.Nil(t, got.CostUSD, "cost_usd must be omitted when has_cost is false")
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
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('test-model-top', 1000000, 0, 0, 0, '2026-01-01T00:00:00Z')`)
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
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('test-model-top-source', 1000000, 0, 0, 0, '2026-01-01T00:00:00Z')`)
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

func TestStoreGetTopSessionsRanksBySelectedTokenTypes(t *testing.T) {
	_, store := prepareUsageSchema(
		t, "agentsview_top_sessions_selected_tokens_test",
	)
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('input-heavy', 'test-machine', 'demo', 'codex',
			 '2026-02-01T12:00:00Z'::timestamptz, 1, 1),
			('output-heavy', 'test-machine', 'demo', 'codex',
			 '2026-02-01T13:00:00Z'::timestamptz, 1, 1);
		INSERT INTO usage_events (
			session_id, source, model, input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			occurred_at, dedup_key
		) VALUES
			('input-heavy', 'provider', 'model', 1000, 1, 40, 800,
			 '2026-02-01T12:01:00Z'::timestamptz, 'input-heavy'),
			('output-heavy', 'provider', 'model', 10, 50, 2, 3,
			 '2026-02-01T13:01:00Z'::timestamptz, 'output-heavy')`)
	require.NoError(t, err)

	top, err := store.GetTopSessionsByCost(ctx, db.UsageFilter{
		From:                  "2026-02-01",
		To:                    "2026-02-01",
		Timezone:              "UTC",
		TopSessionsSort:       db.TopSessionsSortTokens,
		TopSessionsTokenTypes: db.UsageTokenTypeOutput,
	}, 1)
	require.NoError(t, err)
	require.Len(t, top, 1)
	assert.Equal(t, "output-heavy", top[0].SessionID)
	assert.Equal(t, 10, top[0].InputTokens)
	assert.Equal(t, 50, top[0].OutputTokens)
	assert.Equal(t, 2, top[0].CacheCreationTokens)
	assert.Equal(t, 3, top[0].CacheReadTokens)
	assert.Equal(t, 65, top[0].TotalTokens)
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

func TestStoreGetUsageSessionCountsFiltersAfterCrossSessionSnapshotSelection(
	t *testing.T,
) {
	_, store := prepareUsageSchema(
		t, "agentsview_usage_counts_filtered_snapshot_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('count-parent', 'test-machine', 'parent-project', 'claude',
			 '2026-05-20T10:00:00Z'::timestamptz, 1, 1),
			('count-child', 'test-machine', 'child-project', 'claude',
			 '2026-05-20T10:01:00Z'::timestamptz, 1, 1);
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
			('count-parent', 0, 'assistant', 'partial',
			 '2026-05-20T10:00:00Z'::timestamptz, 7, 'partial-model',
			 '{"input_tokens":10,"output_tokens":5}',
			 'count-message', 'count-request'),
			('count-child', 0, 'assistant', 'complete',
			 '2026-05-20T10:01:00Z'::timestamptz, 8, 'complete-model',
			 '{"input_tokens":1000,"output_tokens":631}',
			 'count-message', 'count-request')`)
	require.NoError(t, err)

	partialCounts, err := store.GetUsageSessionCounts(ctx, db.UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
		Model: "partial-model",
	})
	require.NoError(t, err)
	assert.Zero(t, partialCounts.Total,
		"the discarded partial model must not count a session")

	completeParentCounts, err := store.GetUsageSessionCounts(ctx, db.UsageFilter{
		From: "2026-05-20", To: "2026-05-20", Timezone: "UTC",
		Model: "complete-model", ProjectLabels: []string{"parent-project"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, completeParentCounts.Total)
	assert.Equal(t, 1, completeParentCounts.ByProject["parent-project"])
	assert.NotContains(t, completeParentCounts.ByProject, "child-project")
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
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES
			('claude-sonnet-4-20250514', 1000000, 1000000, 1000000, 1000000, '2026-01-01T00:00:00Z'),
			('gpt-5.4', 1000000, 1000000, 1000000, 1000000, '2026-01-01T00:00:00Z')`)
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
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('gpt-5.4', 1000000, 2000000, 0, 0, '2026-01-01T00:00:00Z')`)
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
	wantCost, err := money.CostPerMillion([]money.RatedTokens{
		{Tokens: int64(rawInput), Rate: money.MustParseDollars("1")},
		{Tokens: int64(rawOutput), Rate: money.MustParseDollars("2")},
	})
	require.NoError(t, err)
	assert.Equal(t, wantCost, usage.Cost, "session cost")
	require.Len(t, usage.Breakdown, 1, "Breakdown")
	entry := usage.Breakdown[0]
	assert.Equal(t, "session", entry.Source)
	assert.Equal(t, "Step 1", entry.Label)
	require.NotNil(t, entry.MessageOrdinal)
	assert.Equal(t, 0, *entry.MessageOrdinal)
	assert.Equal(t, rawInput, entry.InputTokens)
	assert.Equal(t, rawOutput, entry.OutputTokens)
	assert.True(t, entry.HasCost)
	assert.Equal(t, wantCost, entry.Cost, "breakdown cost")
}

func TestPostgresUsageCostsMessageReasoningTokens(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_message_reasoning_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('gpt-5.4', 1000000, 2000000, 0, 0, '2026-01-01T00:00:00Z')`)
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
	assert.Equal(t, money.MustParseDollars("4.001"), daily.Totals.TotalCost)

	usage, err := store.GetSessionUsage(ctx, "pg-message-reasoning", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, usage)
	assert.True(t, usage.HasCost)
	assert.Equal(t, money.MustParseDollars("4.001"), usage.Cost)
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
			cache_read_tokens, charged_microdollars, cursor_token_fee_microdollars,
			user_id, user_email, dedup_key
		) VALUES (
			'2026-05-14T10:05:00Z'::timestamptz,
			'claude-4.6-opus-high-thinking',
			'USAGE_EVENT_KIND_USAGE_BASED',
			1234, 567, 8901, 156600, 33200,
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
		InputPerMTok:         money.MustParseDollars("1.5"),
		OutputPerMTok:        money.MustParseDollars("2.5"),
		CacheCreationPerMTok: money.MustParseDollars("3.5"),
		CacheReadPerMTok:     money.MustParseDollars("0.5"),
		Bands: []db.PricingBand{{
			AboveInputTokens:     200_000,
			InputPerMTok:         money.MustParseDollars("3"),
			OutputPerMTok:        money.MustParseDollars("5"),
			CacheCreationPerMTok: money.MustParseDollars("7"),
			CacheReadPerMTok:     money.MustParseDollars("1"),
		}},
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
		SELECT model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok
		FROM model_pricing
		WHERE model_pattern = 'test-model-sync'`)
	require.NoError(t, err, "query pricing")
	defer rows.Close()

	require.True(t, rows.Next(), "expected synced pricing row")
	var (
		model                                   string
		input, output, cacheCreation, cacheRead int64
	)
	require.NoError(t, rows.Scan(
		&model, &input, &output, &cacheCreation, &cacheRead,
	), "scan pricing")
	assert.Equal(t, "test-model-sync", model)
	assert.Equal(t, int64(1_500_000), input)
	assert.Equal(t, int64(2_500_000), output)
	assert.Equal(t, int64(3_500_000), cacheCreation)
	assert.Equal(t, int64(500_000), cacheRead)
	require.NoError(t, rows.Close())

	var (
		threshold                                               int
		bandInput, bandOutput, bandCacheCreation, bandCacheRead int64
		firstUpdatedAt                                          string
	)
	require.NoError(t, store.DB().QueryRowContext(context.Background(), `
		SELECT b.above_input_tokens,
			b.input_microdollars_per_mtok,
			b.output_microdollars_per_mtok,
			b.cache_creation_microdollars_per_mtok,
			b.cache_read_microdollars_per_mtok,
			p.updated_at
		FROM model_pricing_bands b
		JOIN model_pricing p USING (model_pattern)
		WHERE b.model_pattern = 'test-model-sync'`).Scan(
		&threshold, &bandInput, &bandOutput,
		&bandCacheCreation, &bandCacheRead, &firstUpdatedAt,
	))
	assert.Equal(t, 200_000, threshold)
	assert.Equal(t, int64(3_000_000), bandInput)
	assert.Equal(t, int64(5_000_000), bandOutput)
	assert.Equal(t, int64(7_000_000), bandCacheCreation)
	assert.Equal(t, int64(1_000_000), bandCacheRead)

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "test-model-sync",
		InputPerMTok:         money.MustParseDollars("1.5"),
		OutputPerMTok:        money.MustParseDollars("2.5"),
		CacheCreationPerMTok: money.MustParseDollars("3.5"),
		CacheReadPerMTok:     money.MustParseDollars("0.5"),
	}}), "remove local pricing bands")
	_, err = ps.Push(context.Background(), false, nil)
	require.NoError(t, err, "push band removal")

	var bandCount int
	require.NoError(t, store.DB().QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM model_pricing_bands
		WHERE model_pattern = 'test-model-sync'`).Scan(&bandCount))
	assert.Zero(t, bandCount)
	var secondUpdatedAt string
	require.NoError(t, store.DB().QueryRowContext(context.Background(), `
		SELECT updated_at FROM model_pricing
		WHERE model_pattern = 'test-model-sync'`).Scan(&secondUpdatedAt))
	firstRevision, err := time.Parse(time.RFC3339Nano, firstUpdatedAt)
	require.NoError(t, err)
	secondRevision, err := time.Parse(time.RFC3339Nano, secondUpdatedAt)
	require.NoError(t, err)
	assert.True(t, secondRevision.After(firstRevision),
		"band removal must advance the parent pricing revision")
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

func TestStoreGetSessionUsage_CopilotExplicitCost(t *testing.T) {
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
			session_id, source, model, input_tokens, output_tokens, cost_microdollars, occurred_at
		) VALUES (
			'copilot:s1', 'api', 'gpt-4', 1000, 500, 100000, '2026-03-12T10:00:00Z'::timestamptz
		)`)
	require.NoError(t, err, "insert usage event")

	u, err := store.GetSessionUsage(ctx, "copilot:s1", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, u, "usage is nil")
	assert.True(t, u.HasCost, "HasCost")
	assert.Equal(t, money.MustParseDollars("0.10"), u.Cost, "Cost")
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
			cost_microdollars, cost_status, cost_source, occurred_at, dedup_key
		) VALUES
			('copilot:reported', 'shutdown', 'gpt-4', 1000, 500,
			 NULL, '', '', '2026-03-12T10:01:00Z'::timestamptz, 'segment-1'),
			('copilot:reported', 'shutdown', 'gpt-4', 1000, 500,
			 27500, 'exact', 'copilot-reported',
			 '2026-03-13T10:02:00Z'::timestamptz, 'segment-2')`)
	require.NoError(t, err)

	usage, err := store.GetSessionUsage(ctx, "copilot:reported", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, money.MustParseDollars("0.0275"), usage.Cost)
	assert.InDelta(t, 0.0275/0.01, usage.AICredits, 1e-9)
	require.Len(t, usage.Breakdown, 2)
	assert.Equal(t, money.MustParseDollars("0.01375"), usage.Breakdown[0].Cost)
	assert.Equal(t, money.MustParseDollars("0.01375"), usage.Breakdown[1].Cost)
	assert.Equal(t, usage.Cost,
		money.MustAdd(usage.Breakdown[0].Cost, usage.Breakdown[1].Cost))

	daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-03-12", To: "2026-03-13", Timezone: "UTC",
	})
	require.NoError(t, err)
	require.Len(t, daily.Daily, 2)
	assert.Equal(t, money.MustParseDollars("0.01375"), daily.Daily[0].TotalCost)
	assert.Equal(t, money.MustParseDollars("0.01375"), daily.Daily[1].TotalCost)
	for _, day := range daily.Daily {
		require.Len(t, day.ModelBreakdowns, 1)
		assert.Equal(t, day.TotalCost, day.ModelBreakdowns[0].Cost)
	}
	assert.Equal(t, money.MustParseDollars("0.0275"), daily.Totals.TotalCost)
	assert.InDelta(t, 2.75, daily.Totals.CopilotAICredits, 1e-9,
		"credits derive from the authoritative reported cost")
	require.NotNil(t, daily.Pricing)
	assert.Equal(t, export.CostSourceMixed, daily.Pricing.CostSource,
		"authoritative reported cost must surface in pricing provenance")
	assert.Equal(t, export.CostSourceComputed,
		daily.Pricing.Models["gpt-4"].CostSource)
}

func TestStoreGetSessionUsage_CopilotCostOnlyReported(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_copilot_cost_only_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'copilot:cost-only', 'test-machine', 'proj', 'copilot',
			'2026-03-12T10:00:00Z'::timestamptz, 1, 1
		)`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO usage_events (
			session_id, source, model, input_tokens, output_tokens,
			cost_microdollars, cost_status, cost_source, occurred_at, dedup_key
		) VALUES (
			'copilot:cost-only', 'shutdown', 'copilot', 0, 0,
			17500, 'exact', 'copilot-reported',
			'2026-03-12T10:01:00Z'::timestamptz, 'cost-only'
		)`)
	require.NoError(t, err)

	u, err := store.GetSessionUsage(ctx, "copilot:cost-only", true)
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.True(t, u.HasCost)
	assert.Equal(t, money.MustParseDollars("0.0175"), u.Cost)
	assert.False(t, u.HasTokenData,
		"a cost-only reported row is not token data")
	assert.Empty(t, u.Models,
		"a cost-only carrier row must not surface a model")
	assert.Zero(t, u.BreakdownCount)
	assert.Empty(t, u.Breakdown)
}

func TestStoreGetSessionUsage_CopilotUnpricedNoCost(t *testing.T) {
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
	assert.Zero(t, u.Cost, "Cost should be zero when unpriced")
	assert.Equal(t, 0.0, u.AICredits, "AICredits should be 0 when unpriced")
}

// TestStoreSessionUsageWithSubagentsParity pins PostgreSQL parity for the
// presentation-time subagent rollup: combined totals, cross-transcript
// deduplication of a shared claude_message_id/claude_request_id pair, and
// breakdown rows tagged with the subagent they came from.
func TestStoreSessionUsageWithSubagentsParity(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_session_usage_subagents_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES ('test-opus', 2000000, 10000000, 0, 0, '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, message_count,
			user_message_count, parent_session_id, relationship_type,
			total_output_tokens, has_total_output_tokens
		) VALUES
			('pg-sub-parent', 'test', 'project', 'claude', '2026-05-20T09:00:00Z', 3, 1, NULL, '', 705, true),
			('agent-pg-a', 'test', 'project', 'claude', '2026-05-20T09:01:00Z', 2, 0, 'pg-sub-parent', 'subagent', 1500, true),
			('agent-pg-b', 'test', 'project', 'claude', '2026-05-20T09:02:00Z', 1, 0, 'pg-sub-parent', 'subagent', 100, true),
			('agent-pg-dedup-only', 'test', 'project', 'claude', '2026-05-20T09:03:00Z', 1, 0, 'pg-sub-parent', 'subagent', 500, true)`)
	require.NoError(t, err)
	// agent-pg-a's second row repeats the parent's turn under the same
	// message identity, the way a sidechain transcript echoes it.
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
			('pg-sub-parent', 0, 'assistant', 'work', '2026-05-20T10:00:00Z', 4, 'test-opus', '{"input_tokens":1000,"output_tokens":5}', 'm-1', 'req-m-1'),
			('pg-sub-parent', 1, 'assistant', 'work', '2026-05-20T10:00:10Z', 4, 'test-opus', '{"input_tokens":1000,"output_tokens":500}', 'm-1', 'req-m-1'),
			('agent-pg-a', 0, 'assistant', 'work', '2026-05-20T10:01:00Z', 4, 'test-opus', '{"input_tokens":2000,"output_tokens":1000}', 'm-2', 'req-m-2'),
			('agent-pg-a', 1, 'assistant', 'work', '2026-05-20T10:02:00Z', 4, 'test-opus', '{"input_tokens":1000,"output_tokens":500}', 'm-1', 'req-m-1'),
			('agent-pg-b', 0, 'assistant', 'work', '2026-05-20T10:03:00Z', 4, 'test-opus', '{"input_tokens":500,"output_tokens":100}', 'm-3', 'req-m-3'),
			('agent-pg-dedup-only', 0, 'assistant', 'work', '2026-05-20T10:04:00Z', 4, 'test-opus', '{"input_tokens":1000,"output_tokens":500}', 'm-1', 'req-m-1')`)
	require.NoError(t, err)
	// This message contributes to the parser's stored output total but has
	// no raw token_usage payload, so it is intentionally absent from cost
	// rows and the usage breakdown.
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, output_tokens, has_output_tokens
		) VALUES
			('pg-sub-parent', 2, 'assistant', 'output-only', '2026-05-20T10:00:30Z', 11, 'test-opus', '', 200, true)`)
	require.NoError(t, err)

	own, err := store.GetSessionUsage(ctx, "pg-sub-parent", true)
	require.NoError(t, err)
	require.NotNil(t, own)
	assert.Equal(t, money.MustParseDollars("0.007"), own.Cost,
		"the own-session path stays own-session")
	assert.Equal(t, 700, own.TotalOutputTokens)
	assert.Zero(t, own.SubagentCount)
	rowSet, err := store.GetSessionUsageRows(ctx, []string{
		"pg-sub-parent", "agent-pg-a", "agent-pg-b", "agent-pg-dedup-only",
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{
		"pg-sub-parent": 505, "agent-pg-a": 1500,
		"agent-pg-b": 100, "agent-pg-dedup-only": 500,
	}, rowSet.RawOutputTokensBySession)
	assert.Equal(t, map[string]struct{}{
		"pg-sub-parent": {}, "agent-pg-a": {}, "agent-pg-dedup-only": {},
	}, rowSet.DiscardedContributingSessions)
	require.Len(t, rowSet.Rows, 3)
	assert.Equal(t, []string{
		"agent-pg-a", "agent-pg-b", "agent-pg-dedup-only",
	}, []string{
		rowSet.Rows[0].SourceSessionID,
		rowSet.Rows[1].SourceSessionID,
		rowSet.Rows[2].SourceSessionID,
	})

	got, err := service.SessionUsageWithSubagents(ctx, store, "pg-sub-parent", true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 3, got.SubagentCount)
	assert.False(t, got.HasCost,
		"the output-only parent tokens make the combined cost incomplete")
	assert.Zero(t, got.Cost)
	assert.Equal(t, 3, got.BreakdownCount)
	require.Len(t, got.Breakdown, 3)
	assert.Equal(t, []string{"agent-pg-a", "agent-pg-b", "agent-pg-dedup-only"}, []string{
		got.Breakdown[0].SubagentSessionID,
		got.Breakdown[1].SubagentSessionID,
		got.Breakdown[2].SubagentSessionID,
	})
	assert.Equal(t, "message", got.Breakdown[0].Source)
	assert.Equal(t, 2000, got.Breakdown[0].InputTokens)
	assert.Equal(t, 1000, got.Breakdown[0].OutputTokens)
	// The stored aggregates sum to 2805. Removing the incomplete 5-token
	// streaming snapshot and the two 500-token echoes, while preserving the
	// parent's output-only 200, yields 1800.
	assert.Equal(t, 1800, got.TotalOutputTokens,
		"output tokens are deduplicated without dropping output-only messages")
	assert.True(t, got.HasTokenData)
}

// TestStoreGetSessionUsage_CodebuffCostOnlyReported pins the
// PostgreSQL aggregator acceptance for Codebuff/Freebuff's
// parser-emitted cost-only usage event. The Codebuff parser
// (internal/parser/codebuff.go) attributes the session cost
// to the agent template (e.g. "base2-deepseek") rather than
// the agent name so the per-model breakdown in the usage
// report stays granular. The PG aggregator must accept that
// non-empty Model value and surface the cost unchanged.
// Without this pin, a future change that hard-codes the
// accepted model name would silently drop template-attributed
// codebuff/freebuff rows.
func TestStoreGetSessionUsage_CodebuffCostOnlyReported(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_codebuff_cost_only_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'codebuff:cost-only', 'test-machine', 'proj', 'codebuff',
			'2026-07-15T10:00:00Z'::timestamptz, 1, 1
		)`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO usage_events (
			session_id, source, model, input_tokens, output_tokens,
			cost_microdollars, cost_status, cost_source, occurred_at, dedup_key
		) VALUES (
			'codebuff:cost-only', 'session', 'base2-deepseek', 0, 0,
			5000, 'reported', 'session',
			'2026-07-15T10:05:00Z'::timestamptz, 'session:codebuff:cost-only'
		)`)
	require.NoError(t, err)

	u, err := store.GetSessionUsage(ctx, "codebuff:cost-only", true)
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.True(t, u.HasCost,
		"a codebuff cost-only event must surface HasCost")
	assert.Equal(t, money.Money{Microdollars: 5000}, u.Cost,
		"the reported cost must flow through unchanged")
	assert.Equal(t, []string{"base2-deepseek"}, u.Models,
		"the parser-attributed template name must surface in Models")
}
