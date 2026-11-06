//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

func TestAccountUsageAgentUpgradePreservesCursorRows(t *testing.T) {
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	_, err := conn.ExecContext(t.Context(), `CREATE TABLE cursor_usage_events (
		id Int64, occurred_at Nullable(DateTime64(6, 'UTC')), model String,
		kind String, input_tokens Int64, output_tokens Int64,
		cache_write_tokens Int64, cache_read_tokens Int64,
		charged_microdollars Int64, cursor_token_fee_microdollars Int64,
		user_id String, user_email String, is_headless Bool,
		dedup_key String, push_version UInt64
	) ENGINE = ReplacingMergeTree(push_version) ORDER BY (dedup_key, id)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO cursor_usage_events
		(id, model, input_tokens, charged_microdollars, dedup_key, push_version)
		VALUES (1, 'grok-test', 12, 17, 'legacy-cursor', 1)`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchemaOn(t.Context(), conn))
	require.NoError(t, EnsureSchemaOn(t.Context(), conn))
	var agent, model string
	var tokens, cost int64
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT
		agent, model, input_tokens, charged_microdollars
		FROM cursor_usage_events WHERE dedup_key = 'legacy-cursor'`).Scan(
		&agent, &model, &tokens, &cost))
	assert.Equal(t, "cursor", agent)
	assert.Equal(t, "grok-test", model)
	assert.EqualValues(t, 12, tokens)
	assert.EqualValues(t, 17, cost)
}

func TestAccountUsagePushKeepsAgentsUnknownModelsAndReportedCosts(t *testing.T) {
	const billingScale = 10_000_000
	local := dbtest.OpenTestDB(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "grok-test", InputPerMTok: money.MustParseDollars("100"),
		OutputPerMTok: money.MustParseDollars("100"),
	}}))
	require.NoError(t, local.InsertCursorUsageEvents(t.Context(), []db.CursorUsageEvent{
		{Agent: "grok", OccurredAt: "2026-11-05T00:30:00Z", InputTokens: 12 * billingScale,
			OutputTokens: 7 * billingScale, CacheReadTokens: 128 * billingScale, DedupKey: "xai:item-a"},
		{Agent: "grok-bot", OccurredAt: "2026-11-04T13:00:00Z", InputTokens: 11 * billingScale,
			OutputTokens: 5 * billingScale, CacheWriteTokens: 32 * billingScale, CacheReadTokens: 64 * billingScale,
			Charged: money.Money{Microdollars: 17}, DedupKey: "xai:item-b"},
		{OccurredAt: "2026-11-04T13:00:00Z", Model: "grok-test",
			InputTokens: 9 * billingScale, OutputTokens: 3 * billingScale, DedupKey: "cursor:item-c"},
	}))
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	assert.Equal(t, 3, chtest.Count(t, store.conn, "cursor_usage_events", ""))
	assert.Equal(t, 2, chtest.Count(t, store.conn, "cursor_usage_events", "model = ''"))
	assert.Zero(t, chtest.Count(t, store.conn, "sessions", ""))

	for _, tc := range []struct {
		name                       string
		filter                     db.UsageFilter
		input, output, write, read int
		cost                       int64
		models                     []string
	}{
		{"grok", db.UsageFilter{Agent: "grok"}, 12, 7, 0, 128, 0, []string{"<unknown>"}},
		{"grok bot", db.UsageFilter{Agent: "grok-bot"}, 11, 5, 32, 64, 17, []string{"<unknown>"}},
		{"cursor default", db.UsageFilter{Agent: "cursor"}, 9, 3, 0, 0, 0, []string{"grok-test"}},
		{"agents", db.UsageFilter{Agent: "grok, grok-bot"}, 23, 12, 32, 192, 17, []string{"<unknown>"}},
		{"exclude agent", db.UsageFilter{ExcludeAgent: "grok-bot"}, 21, 10, 0, 128, 0, []string{"<unknown>", "grok-test"}},
		{"unknown model", db.UsageFilter{Model: "<unknown>"}, 23, 12, 32, 192, 17, []string{"<unknown>"}},
		{"exclude unknown model", db.UsageFilter{ExcludeModel: "<unknown>"}, 9, 3, 0, 0, 0, []string{"grok-test"}},
		{"all", db.UsageFilter{}, 32, 15, 32, 192, 17, []string{"<unknown>", "grok-test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.filter
			f.From, f.To = "2026-11-04", "2026-11-04"
			f.Timezone, f.Breakdowns = "America/New_York", true
			got, err := store.GetDailyUsage(t.Context(), f)
			require.NoError(t, err)
			require.Len(t, got.Daily, 1)
			assert.Equal(t, "2026-11-04", got.Daily[0].Date)
			assert.Equal(t, tc.input*billingScale, got.Daily[0].InputTokens)
			assert.Equal(t, tc.output*billingScale, got.Daily[0].OutputTokens)
			assert.Equal(t, tc.write*billingScale, got.Daily[0].CacheCreationTokens)
			assert.Equal(t, tc.read*billingScale, got.Daily[0].CacheReadTokens)
			assert.Equal(t, tc.cost, got.Daily[0].TotalCost.Microdollars)
			assert.Equal(t, tc.models, got.Daily[0].ModelsUsed)
			assert.Zero(t, got.SessionCounts.Total)
			assert.Empty(t, got.Projects)
			if f.Agent == "grok" || f.Agent == "grok-bot" || f.Agent == "cursor" {
				require.Len(t, got.Daily[0].AgentBreakdowns, 1)
				assert.Equal(t, f.Agent, got.Daily[0].AgentBreakdowns[0].Agent)
			}
		})
	}
}
