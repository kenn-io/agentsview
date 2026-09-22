package clickhouse

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestActivityUsagePricingKeepsReportedCosts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		input       int
		reported    sql.NullInt64
		wantCost    int64
		contributes bool
	}{
		{"computed provider billing", 1000, sql.NullInt64{}, 2200, true},
		{"reported cost", 1000, sql.NullInt64{Int64: 123, Valid: true}, 123, true},
		{"reported zero", 1000, sql.NullInt64{Valid: true}, 0, true},
		{"no usage", 0, sql.NullInt64{}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
				ModelPattern: "activity-price-test",
				Rates: export.ModelRates{
					InputPerMTok: money.MustParseDollars("2"),
					Source:       export.PricingRowSourceFetched,
				},
			}})
			cost, priced, contributes, err := clickActivityReportRowStatus(clickActivityReportUsageRow{
				model: "activity-price-test", providerID: "positai",
				inputTok: tc.input, cost: tc.reported,
			}, resolver)
			require.NoError(t, err)
			assert.Equal(t, tc.wantCost, cost.Microdollars)
			assert.True(t, priced)
			assert.Equal(t, tc.contributes, contributes)
		})
	}
}
