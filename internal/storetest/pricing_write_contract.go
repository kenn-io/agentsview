package storetest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

// RunPricingWriteContract verifies that canonical base-price and band changes
// commit or roll back as one unit on a real target engine.
func RunPricingWriteContract(t *testing.T, name string, store bun.IDB) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		ctx := t.Context()
		require.NoError(t, db.UpsertModelPricingRows(
			ctx, store,
			[]bunmodel.ModelPricing{{
				ModelPattern: "atomic-contract", InputMicrodollarsPerMTok: 5,
				OutputMicrodollarsPerMTok: 6,
				UpdatedAt:                 mustPricingTimestamp(t, "2026-08-03T13:00:00Z"),
			}},
			[]bunmodel.ModelPricingBand{{
				ModelPattern: "atomic-contract", AboveInputTokens: 300,
				InputMicrodollarsPerMTok: 7,
				UpdatedAt:                mustPricingTimestamp(t, "2026-08-03T13:00:00Z"),
			}},
		))

		err := db.UpsertModelPricingRows(
			ctx, store,
			[]bunmodel.ModelPricing{{
				ModelPattern: "atomic-contract", InputMicrodollarsPerMTok: 99,
				OutputMicrodollarsPerMTok: 100,
				UpdatedAt:                 mustPricingTimestamp(t, "2026-08-03T14:00:00Z"),
			}},
			[]bunmodel.ModelPricingBand{
				{ModelPattern: "atomic-contract", AboveInputTokens: 400,
					InputMicrodollarsPerMTok: 8,
					UpdatedAt:                mustPricingTimestamp(t, "2026-08-03T14:00:00Z")},
				{ModelPattern: "atomic-contract", AboveInputTokens: 400,
					InputMicrodollarsPerMTok: 9,
					UpdatedAt:                mustPricingTimestamp(t, "2026-08-03T14:00:00Z")},
			},
		)
		require.Error(t, err)

		var price bunmodel.ModelPricing
		require.NoError(t, store.NewSelect().Model(&price).
			Where("model_pattern = ?", "atomic-contract").Scan(ctx))
		assert.Equal(t, int64(5), price.InputMicrodollarsPerMTok)
		var bands []bunmodel.ModelPricingBand
		require.NoError(t, store.NewSelect().Model(&bands).
			Where("model_pattern = ?", "atomic-contract").
			OrderExpr("above_input_tokens ASC").Scan(ctx))
		require.Len(t, bands, 1)
		assert.Equal(t, int64(300), bands[0].AboveInputTokens)
		assert.Equal(t, int64(7), bands[0].InputMicrodollarsPerMTok)
	})
}

func mustPricingTimestamp(t *testing.T, value string) bunmodel.Timestamp {
	t.Helper()
	timestamp, err := bunmodel.ParseTimestamp(value)
	require.NoError(t, err)
	return timestamp
}
