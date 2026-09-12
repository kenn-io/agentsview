// ABOUTME: Tests that the PostgreSQL row-cost helpers bill Anthropic web
// ABOUTME: searches exactly like the SQLite ones do.
package postgres

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// webSearchTokenJSON is a Claude usage blob billing 100,000 input and
// 100,000 output tokens plus the given number of server-side web searches.
func webSearchTokenJSON(requests string) string {
	return `{"input_tokens":100000,"output_tokens":100000,` +
		`"server_tool_use":{"web_search_requests":` + requests +
		`,"web_fetch_requests":0}}`
}

// webSearchResolver prices the test model at $1/MTok in and $2/MTok out, so
// webSearchTokenJSON's tokens cost exactly $0.30.
func webSearchResolver() *export.PricingResolver {
	return export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "claude-websearch-test",
		Rates: export.ModelRates{
			InputPerMTok:  money.MustParseDollars("1.0"),
			OutputPerMTok: money.MustParseDollars("2.0"),
		},
	}})
}

func TestPGUsageRowWebSearchRequests(t *testing.T) {
	tests := []struct {
		name        string
		usageSource string
		tokenJSON   string
		want        int
	}{
		{
			name:        "message row reports its counter",
			usageSource: "message",
			tokenJSON:   webSearchTokenJSON("3"),
			want:        3,
		},
		{
			name:        "zero counter",
			usageSource: "message",
			tokenJSON:   webSearchTokenJSON("0"),
			want:        0,
		},
		{
			name:        "negative counter reads as zero",
			usageSource: "message",
			tokenJSON:   webSearchTokenJSON("-2"),
			want:        0,
		},
		{
			name:        "absent counter",
			usageSource: "message",
			tokenJSON:   `{"input_tokens":10,"output_tokens":5}`,
			want:        0,
		},
		{
			name:        "usage events never report server tool use",
			usageSource: "session",
			tokenJSON:   webSearchTokenJSON("3"),
			want:        0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pgUsageRowWebSearchRequests(
				tt.usageSource, tt.tokenJSON))
		})
	}
}

func TestPGSessionRowCostBillsWebSearchRequests(t *testing.T) {
	resolver := webSearchResolver()
	cost, priced, contributes, err := pgSessionRowCost(pgUsageScanRow{
		usageSource: "message",
		model:       "claude-websearch-test",
		tokenJSON:   webSearchTokenJSON("2"),
	}, resolver)
	require.NoError(t, err)
	assert.True(t, priced)
	assert.True(t, contributes)
	assert.Equal(t, money.MustParseDollars("0.32"), cost)
}

func TestPGSessionRowCostSkipsFeeOnReportedCost(t *testing.T) {
	resolver := webSearchResolver()
	cost, priced, contributes, err := pgSessionRowCost(pgUsageScanRow{
		usageSource: "message",
		model:       "claude-websearch-test",
		tokenJSON:   webSearchTokenJSON("2"),
		cost:        sql.NullInt64{Int64: 500_000, Valid: true},
	}, resolver)
	require.NoError(t, err)
	assert.True(t, priced)
	assert.True(t, contributes)
	assert.Equal(t, money.MustParseDollars("0.50"), cost)
}

func TestPGSessionRowCostBillsWebSearchOnUnpricedModel(t *testing.T) {
	resolver := webSearchResolver()
	cost, priced, contributes, err := pgSessionRowCost(pgUsageScanRow{
		usageSource: "message",
		model:       "some-unlisted-model",
		tokenJSON:   webSearchTokenJSON("2"),
	}, resolver)
	require.NoError(t, err)
	assert.False(t, priced)
	assert.True(t, contributes)
	assert.Equal(t, money.MustParseDollars("0.02"), cost)
}

func TestPGDailyUsageAmountsBillWebSearchRequests(t *testing.T) {
	resolver := webSearchResolver()
	_, _, _, _, cost, _, err := pgDailyUsageAmounts(pgDailyUsageScanRow{
		usageSource: "message",
		model:       "claude-websearch-test",
		tokenJSON:   webSearchTokenJSON("2"),
	}, resolver)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.32"), cost)
}

func TestPGActivityReportRowStatusBillsWebSearchRequests(t *testing.T) {
	resolver := webSearchResolver()
	cost, priced, contributes, err := pgActivityReportRowStatus(
		pgDailyUsageScanRow{
			usageSource: "message",
			model:       "claude-websearch-test",
			tokenJSON:   webSearchTokenJSON("2"),
		}, resolver)
	require.NoError(t, err)
	assert.True(t, priced)
	assert.True(t, contributes)
	assert.Equal(t, money.MustParseDollars("0.32"), cost)
}
