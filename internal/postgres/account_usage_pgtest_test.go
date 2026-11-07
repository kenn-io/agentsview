//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

func TestAccountUsageAgentsPostgresParity(t *testing.T) {
	const billingScale = 1_000_000_000
	const schema = "agentsview_account_usage_parity_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	local := testDB(t)
	seedUsageParityFixture(t, local)
	require.NoError(t, local.InsertCursorUsageEvents(t.Context(), []db.CursorUsageEvent{
		{Agent: "grok", OccurredAt: "2026-08-13T00:30:00Z", InputTokens: 12 * billingScale, OutputTokens: 7 * billingScale, CacheReadTokens: 128 * billingScale, Charged: money.Money{Microdollars: 250_000}, DedupKey: "xai:paid"},
		{Agent: "grok-bot", OccurredAt: "2026-08-12T13:00:00Z", InputTokens: 11 * billingScale, OutputTokens: 5 * billingScale, CacheWriteTokens: 32 * billingScale, CacheReadTokens: 64 * billingScale, DedupKey: "xai:free"},
		{OccurredAt: "2026-08-12T13:00:00Z", Model: "model-priced", InputTokens: 9 * billingScale, OutputTokens: 3 * billingScale, DedupKey: "cursor:free"},
		{OccurredAt: "2026-08-12T13:00:00Z", InputTokens: 2 * billingScale, OutputTokens: 1 * billingScale, Charged: money.Money{Microdollars: 30_000}, DedupKey: "cursor:paid"},
	}))
	syncer, err := New(t.Context(), pgURL, schema, local, "parity-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	remote, err := NewStore(t.Context(), pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remote.Close()) })
	stats, err := remote.GetStats(t.Context(), false, false)
	require.NoError(t, err)
	require.NotNil(t, stats.EarliestAccountUsage)
	assert.Equal(t, "2026-08-12T13:00:00Z", *stats.EarliestAccountUsage)

	for _, tc := range []struct {
		name, agent, excludeAgent, model, excludeModel string
		input, output, cacheWrite, cacheRead           int
		cost                                           int64
		models                                         []string
	}{
		{name: "grok", agent: "grok", input: 12, output: 7, cacheRead: 128, cost: 250_000, models: []string{"<unknown>"}},
		{name: "grok-bot zero", agent: "grok-bot", input: 11, output: 5, cacheWrite: 32, cacheRead: 64, models: []string{"<unknown>"}},
		{name: "cursor default", agent: "cursor", input: 11, output: 4, cost: 30_000, models: []string{"<unknown>", "model-priced"}},
		{name: "multiple agents", agent: "grok,grok-bot,cursor", input: 34, output: 16, cacheWrite: 32, cacheRead: 192, cost: 280_000, models: []string{"<unknown>", "model-priced"}},
		{name: "exclude agent", agent: "grok,grok-bot,cursor", excludeAgent: "cursor,grok-bot", input: 12, output: 7, cacheRead: 128, cost: 250_000, models: []string{"<unknown>"}},
		{name: "unknown model", agent: "grok,grok-bot,cursor", model: "<unknown>", input: 25, output: 13, cacheWrite: 32, cacheRead: 192, cost: 280_000, models: []string{"<unknown>"}},
		{name: "exclude unknown model", agent: "grok,grok-bot,cursor", excludeModel: "<unknown>", input: 9, output: 3, models: []string{"model-priced"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter := db.UsageFilter{
				From: "2026-08-12", To: "2026-08-12", Timezone: "America/New_York",
				Agent: tc.agent, ExcludeAgent: tc.excludeAgent,
				Model: tc.model, ExcludeModel: tc.excludeModel, Breakdowns: true,
			}
			requireCompleteUsageParity(t, local, remote, filter)
			got, err := remote.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			require.Len(t, got.Daily, 1)
			assert.Equal(t, "2026-08-12", got.Daily[0].Date)
			assert.Equal(t, tc.input*billingScale, got.Totals.InputTokens)
			assert.Equal(t, tc.output*billingScale, got.Totals.OutputTokens)
			assert.Equal(t, tc.cacheWrite*billingScale, got.Totals.CacheCreationTokens)
			assert.Equal(t, tc.cacheRead*billingScale, got.Totals.CacheReadTokens)
			assert.Equal(t, tc.cost, got.Totals.TotalCost.Microdollars)
			assert.Equal(t, tc.models, got.Daily[0].ModelsUsed)
			assert.Zero(t, got.SessionCounts.Total)
			assert.Empty(t, got.Projects)
		})
	}
	requireCompleteUsageParity(t, local, remote, db.UsageFilter{
		From: "2026-08-12", To: "2026-08-12", Timezone: "America/New_York", Breakdowns: true,
	})

	t.Run("aggregate billing cache savings", func(t *testing.T) {
		require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
			ModelPattern: "billing-banded", InputPerMTok: money.MustParseDollars("10"),
			CacheCreationPerMTok: money.MustParseDollars("12"), CacheReadPerMTok: money.MustParseDollars("1"),
			Bands: []db.PricingBand{{
				AboveInputTokens: 200_000, InputPerMTok: money.MustParseDollars("20"),
				CacheCreationPerMTok: money.MustParseDollars("24"), CacheReadPerMTok: money.MustParseDollars("2"),
			}},
		}}))
		require.NoError(t, local.InsertCursorUsageEvents(t.Context(), []db.CursorUsageEvent{{
			Agent: "grok", OccurredAt: "2026-11-04T13:00:00Z", Model: "billing-banded",
			InputTokens: 300_000, CacheWriteTokens: 100_000, CacheReadTokens: 400_000, DedupKey: "xai:banded",
		}}))
		_, err := syncer.Push(t.Context(), false, nil)
		require.NoError(t, err)
		filter := db.UsageFilter{From: "2026-11-04", To: "2026-11-04", Timezone: "UTC", Agent: "grok", Model: "billing-banded"}
		requireCompleteUsageParity(t, local, remote, filter)
		got, err := remote.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err)
		assert.Zero(t, got.Totals.TotalCost.Microdollars)
		assert.Equal(t, money.MustParseDollars("3.40"), got.Totals.CacheSavings)
	})
}

func TestAccountUsageAgentMigrationPreservesCursorRows(t *testing.T) {
	const schema = "agentsview_account_usage_migration_test"
	_, store := prepareUsageSchema(t, schema)
	_, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO cursor_usage_events (occurred_at, model, input_tokens,
			output_tokens, cache_write_tokens, cache_read_tokens, charged_microdollars, dedup_key)
		VALUES ('2026-08-12T13:00:00Z', 'model-priced', 12, 7, 32, 64, 30000, 'cursor:legacy');
		ALTER TABLE cursor_usage_events
			ALTER COLUMN input_tokens TYPE INT,
			ALTER COLUMN output_tokens TYPE INT,
			ALTER COLUMN cache_write_tokens TYPE INT,
			ALTER COLUMN cache_read_tokens TYPE INT`)
	require.NoError(t, err)
	assert.False(t, pushSchemaCurrent(t.Context(), store.DB()), "32-bit billing counters need migration")
	_, err = store.DB().ExecContext(t.Context(), `ALTER TABLE cursor_usage_events DROP COLUMN agent`)
	require.NoError(t, err)
	assert.False(t, pushSchemaCurrent(t.Context(), store.DB()))
	require.NoError(t, EnsureSchema(t.Context(), store.DB(), schema))
	require.NoError(t, EnsureSchema(t.Context(), store.DB(), schema))
	var agent string
	var cost int64
	var input, output, cacheWrite, cacheRead int64
	require.NoError(t, store.DB().QueryRowContext(t.Context(), `
		SELECT agent, charged_microdollars, input_tokens, output_tokens,
			cache_write_tokens, cache_read_tokens FROM cursor_usage_events
		WHERE dedup_key = 'cursor:legacy'`).Scan(&agent, &cost, &input, &output, &cacheWrite, &cacheRead))
	assert.Equal(t, "cursor", agent)
	assert.Equal(t, int64(30_000), cost)
	assert.Equal(t, []int64{12, 7, 32, 64}, []int64{input, output, cacheWrite, cacheRead})
	_, err = store.DB().ExecContext(t.Context(), `
		INSERT INTO cursor_usage_events (occurred_at, model, input_tokens,
			output_tokens, cache_write_tokens, cache_read_tokens, dedup_key)
		VALUES ('2026-08-12T13:00:00Z', '', 3000000001, 3000000002,
			3000000003, 3000000004, 'xai:aggregate')`)
	require.NoError(t, err)
	require.NoError(t, store.DB().QueryRowContext(t.Context(), `
		SELECT input_tokens, output_tokens, cache_write_tokens, cache_read_tokens
		FROM cursor_usage_events WHERE dedup_key = 'xai:aggregate'`).Scan(&input, &output, &cacheWrite, &cacheRead))
	assert.Equal(t, []int64{3_000_000_001, 3_000_000_002, 3_000_000_003, 3_000_000_004},
		[]int64{input, output, cacheWrite, cacheRead})
	assert.True(t, pushSchemaCurrent(t.Context(), store.DB()))
}

func TestHostedTenantUpgradePreservesAccountUsage(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	_, err := f.runtime.ExecContext(t.Context(), `
		INSERT INTO cursor_usage_events (occurred_at, model, input_tokens,
			charged_microdollars, dedup_key)
		VALUES ('2026-08-12T13:00:00Z', '', 12, 30000, 'cursor:legacy')`)
	require.NoError(t, err)
	_, err = f.admin.ExecContext(t.Context(), `ALTER TABLE cursor_usage_events
		DROP COLUMN agent,
		ALTER COLUMN input_tokens TYPE INT,
		ALTER COLUMN output_tokens TYPE INT,
		ALTER COLUMN cache_write_tokens TYPE INT,
		ALTER COLUMN cache_read_tokens TYPE INT`)
	require.NoError(t, err)

	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	store, err := NewHostedStore(t.Context(), f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close()) })
	filter := db.UsageFilter{From: "2026-08-12", To: "2026-08-12", Timezone: "UTC"}
	got, err := store.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 12, got.Totals.InputTokens)
	assert.Equal(t, money.MustParseDollars("0.03"), got.Totals.TotalCost)

	_, err = f.runtime.ExecContext(t.Context(), `
		INSERT INTO cursor_usage_events (agent, occurred_at, model, input_tokens,
			output_tokens, cache_write_tokens, cache_read_tokens, dedup_key)
		VALUES ('grok', '2026-08-12T13:00:00Z', '', 3000000001,
			3000000002, 3000000003, 3000000004, 'xai:aggregate')`)
	require.NoError(t, err)
	filter.Agent = "grok"
	got, err = store.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 3_000_000_001, got.Totals.InputTokens)
	assert.Equal(t, 3_000_000_002, got.Totals.OutputTokens)
	assert.Equal(t, 3_000_000_003, got.Totals.CacheCreationTokens)
	assert.Equal(t, 3_000_000_004, got.Totals.CacheReadTokens)
	assert.Zero(t, got.Totals.TotalCost.Microdollars)
}
