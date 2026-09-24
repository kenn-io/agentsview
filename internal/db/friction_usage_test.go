package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/money"
)

// Port of jilog archive_spend.rs:270 parses_daily_rows_and_summarizes_yesterday_and_week,
// fed as the native DailyUsageResult instead of `usage daily --json`.
func TestFrictionArchiveSpendFromDaily(t *testing.T) {
	m := func(micros int64) money.Money { return money.Money{Microdollars: micros} }
	daily := DailyUsageResult{Daily: []DailyUsageEntry{
		{
			Date: "2026-09-14", TotalCost: m(1500000),
			ModelBreakdowns: []ModelBreakdown{{ModelName: "gpt-5.6-sol", Cost: m(1500000)}},
			AgentBreakdowns: []AgentBreakdown{{Agent: "codex", Cost: m(1500000)}},
		},
		{
			Date: "2026-09-15", TotalCost: m(332138392),
			ModelBreakdowns: []ModelBreakdown{
				{ModelName: "gpt-6-astra", Cost: m(125784488)},
				{ModelName: "claude-opus-5", Cost: m(107584332)},
				{ModelName: "gpt-5.6-sol", Cost: m(98769572)},
			},
			AgentBreakdowns: []AgentBreakdown{
				{Agent: "codex", Cost: m(224554060)},
				{Agent: "claude", Cost: m(104943456)},
				{Agent: "cowork", Cost: m(2640876)},
			},
		},
		{Date: "2026-09-16", TotalCost: m(999)},
	}}

	t.Run("parses_daily_rows_and_summarizes_yesterday_and_week", func(t *testing.T) {
		from, to, err := FrictionArchiveWindow("2026-09-16")
		require.NoError(t, err)
		assert.Equal(t, []string{"2026-09-09", "2026-09-15"}, []string{from, to})
		spend := FrictionArchiveSpendFromDaily(daily, from, to, "Asia/Dhaka")
		require.NotNil(t, spend)
		assert.Equal(t, "Asia/Dhaka", spend.Timezone)
		require.NotNil(t, spend.Yesterday, "2026-09-15 present")
		assert.Equal(t, "332.138392", spend.Yesterday.Total.String())
		assert.Equal(t, 2, spend.Week.Days, "the 16th is today and excluded")
		assert.Equal(t, "333.638392", spend.Week.Total.String())
		assert.Equal(t, "226.054060", spend.Week.Agents["codex"].String())
		assert.Equal(t, "125.784488", spend.Week.Models["gpt-6-astra"].String())
	})
	t.Run("yesterday_absent", func(t *testing.T) {
		from, to, err := FrictionArchiveWindow("2026-09-18")
		require.NoError(t, err)
		spend := FrictionArchiveSpendFromDaily(daily, from, to, "UTC")
		require.NotNil(t, spend)
		assert.Nil(t, spend.Yesterday)
		assert.Equal(t, 3, spend.Week.Days)
	})
	t.Run("nothing_in_window", func(t *testing.T) {
		from, to, err := FrictionArchiveWindow("2027-01-01")
		require.NoError(t, err)
		assert.Nil(t, FrictionArchiveSpendFromDaily(daily, from, to, "UTC"))
		assert.Nil(t, FrictionArchiveSpendFromDaily(DailyUsageResult{}, from, to, "UTC"))
	})
	t.Run("window_edges", func(t *testing.T) {
		from, to, err := FrictionArchiveWindow("2026-09-16")
		require.NoError(t, err)
		edge := DailyUsageResult{Daily: []DailyUsageEntry{
			{Date: "2026-09-08", TotalCost: m(1000000)},
			{Date: "2026-09-09", TotalCost: m(1000000)},
		}}
		spend := FrictionArchiveSpendFromDaily(edge, from, to, "UTC")
		require.NotNil(t, spend)
		assert.Equal(t, 1, spend.Week.Days, "2026-09-08 is outside the 7-day window ending 2026-09-15")
	})
}

func TestFrictionUsageFromSessionUsage(t *testing.T) {
	tests := []struct {
		name   string
		in     *SessionUsage
		ok     bool
		input  uint64
		output uint64
		cost   string
		models map[string]string
	}{
		{name: "nil", in: nil},
		{name: "no token or cost data", in: &SessionUsage{}},
		{
			name: "tokens only",
			in: &SessionUsage{HasTokenData: true, Breakdown: []SessionUsageBreakdownEntry{
				{Model: "unpriced", InputTokens: 100, CacheReadInputTokens: 20, CacheCreationInputTokens: 5, OutputTokens: 7},
			}},
			ok: true, input: 125, output: 7, models: map[string]string{},
		},
		{
			name: "priced rows",
			in: &SessionUsage{
				HasTokenData: true, HasCost: true, Cost: money.Money{Microdollars: 1500000},
				Breakdown: []SessionUsageBreakdownEntry{
					{Model: "m1", InputTokens: 10, OutputTokens: 1, Cost: money.Money{Microdollars: 1000000}, HasCost: true},
					{Model: "m1", InputTokens: 10, OutputTokens: 1, Cost: money.Money{Microdollars: 500000}, HasCost: true},
					{Model: "m2", InputTokens: 1, OutputTokens: 1},
				},
			},
			ok: true, input: 21, output: 3, cost: "1.500000", models: map[string]string{"m1": "1.500000"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := FrictionUsageFromSessionUsage(tt.in)
			require.Equal(t, tt.ok, ok)
			if !ok {
				return
			}
			assert.Equal(t, tt.input, got.InputTokens)
			assert.Equal(t, tt.output, got.OutputTokens)
			if tt.cost == "" {
				assert.Nil(t, got.CostUSD)
			} else {
				require.NotNil(t, got.CostUSD)
				assert.Equal(t, tt.cost, got.CostUSD.String())
			}
			models := map[string]string{}
			for k, v := range got.ModelCosts {
				models[k] = v.String()
			}
			assert.Equal(t, tt.models, models)
			assert.Empty(t, got.Role, "role is the runner's decision")
		})
	}
}

func TestFrictionUsageForSessionsSQLite(t *testing.T) {
	d := testDB(t)
	seedFrictionSession(t, d, "priced", "", "2026-09-15T01:00:00Z", nil)
	seedFrictionSession(t, d, "silent", "", "2026-09-15T02:00:00Z", nil)
	cost := money.MustParseDollars("1.5")
	require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), "priced", []UsageEvent{{
		Source: "shutdown", Model: "test-model", InputTokens: 800, OutputTokens: 40,
		Cost: &cost, CostStatus: "exact", OccurredAt: "2026-09-15T01:00:00Z", DedupKey: "one",
	}}))
	got, err := d.FrictionUsageForSessions(t.Context(), []string{"priced", "silent", "missing"})
	require.NoError(t, err)
	require.Contains(t, got, "priced")
	assert.NotContains(t, got, "silent", "no usage data means no stats")
	assert.Equal(t, uint64(800), got["priced"].InputTokens)
	require.NotNil(t, got["priced"].CostUSD)
	assert.Equal(t, "1.500000", got["priced"].CostUSD.String())
	assert.Equal(t, friction.USDFromMicros(1500000).String(), got["priced"].CostUSD.String())
}
