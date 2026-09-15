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

		require.NoError(t, db.UpsertModelPricingRows(ctx, store,
			[]bunmodel.ModelPricing{{
				ModelPattern: "atomic-contract", InputMicrodollarsPerMTok: 10,
				UpdatedAt: mustPricingTimestamp(t, "2026-08-03T12:00:00Z"),
			}}, []bunmodel.ModelPricingBand{{
				ModelPattern: "atomic-contract", AboveInputTokens: 500,
				UpdatedAt: mustPricingTimestamp(t, "2026-08-03T12:00:00Z"),
			}}))
		require.NoError(t, store.NewSelect().Model(&price).
			Where("model_pattern = ?", "atomic-contract").Scan(ctx))
		assert.Equal(t, mustPricingTimestamp(t, "2026-08-03T13:00:00.000001Z"), price.UpdatedAt)

		revisionPrice := bunmodel.ModelPricing{
			ModelPattern: "revision-contract", InputMicrodollarsPerMTok: 11,
			UpdatedAt: mustPricingTimestamp(t, "2026-08-03T15:00:00Z"),
		}
		revisionBands := []bunmodel.ModelPricingBand{
			{ModelPattern: "revision-contract", AboveInputTokens: 100,
				InputMicrodollarsPerMTok: 21, UpdatedAt: mustPricingTimestamp(t, "2026-08-03T15:00:00Z")},
			{ModelPattern: "revision-contract", AboveInputTokens: 200,
				InputMicrodollarsPerMTok: 22, UpdatedAt: mustPricingTimestamp(t, "2026-08-03T15:00:00Z")},
		}
		require.NoError(t, db.UpsertModelPricingRows(
			ctx, store, []bunmodel.ModelPricing{revisionPrice}, revisionBands,
		))
		revisionPrice.UpdatedAt = mustPricingTimestamp(t, "2026-08-03T16:00:00Z")
		for i := range revisionBands {
			revisionBands[i].UpdatedAt = mustPricingTimestamp(t, "2026-08-03T16:00:00Z")
		}
		require.NoError(t, db.UpsertModelPricingRows(
			ctx, store, []bunmodel.ModelPricing{revisionPrice}, revisionBands,
		))
		require.NoError(t, store.NewSelect().Model(&price).
			Where("model_pattern = ?", revisionPrice.ModelPattern).Scan(ctx))
		assert.Equal(t, mustPricingTimestamp(t, "2026-08-03T15:00:00Z"), price.UpdatedAt)
		bands = nil
		require.NoError(t, store.NewSelect().Model(&bands).
			Where("model_pattern = ?", revisionPrice.ModelPattern).
			OrderExpr("above_input_tokens ASC").Scan(ctx))
		require.Len(t, bands, 2)
		assert.Equal(t, mustPricingTimestamp(t, "2026-08-03T15:00:00Z"), bands[0].UpdatedAt)
		assert.Equal(t, mustPricingTimestamp(t, "2026-08-03T15:00:00Z"), bands[1].UpdatedAt)

		revisionBands[1].InputMicrodollarsPerMTok = 23
		revisionBands[1].UpdatedAt = mustPricingTimestamp(t, "2026-08-03T14:00:00Z")
		require.NoError(t, db.UpsertModelPricingRows(
			ctx, store, []bunmodel.ModelPricing{revisionPrice}, revisionBands,
		))
		require.NoError(t, store.NewSelect().Model(&price).
			Where("model_pattern = ?", revisionPrice.ModelPattern).Scan(ctx))
		assert.Equal(t, mustPricingTimestamp(t, "2026-08-03T16:00:00Z"), price.UpdatedAt)
		bands = nil
		require.NoError(t, store.NewSelect().Model(&bands).
			Where("model_pattern = ?", revisionPrice.ModelPattern).
			OrderExpr("above_input_tokens ASC").Scan(ctx))
		require.Len(t, bands, 2)
		assert.Equal(t, mustPricingTimestamp(t, "2026-08-03T15:00:00Z"), bands[0].UpdatedAt)
		assert.Equal(t, mustPricingTimestamp(t, "2026-08-03T15:00:00.000001Z"), bands[1].UpdatedAt)
	})
}

// RunCursorUsageWriteContract verifies targetless deduplication and portable
// empty-model compatibility on a real target engine.
func RunCursorUsageWriteContract(t *testing.T, name string, store bun.IDB) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		rows, err := db.CanonicalCursorUsageEventRows([]db.CursorUsageEvent{{
			OccurredAt: "2026-08-03T13:00:00Z", Model: "", DedupKey: "cursor-contract",
		}})
		require.NoError(t, err)
		require.NoError(t, db.AppendCursorUsageEventRows(t.Context(), store, rows))
		require.NoError(t, db.AppendCursorUsageEventRows(t.Context(), store, rows))
		count, err := store.NewSelect().Model((*bunmodel.CursorUsageEvent)(nil)).
			Where("dedup_key = ?", "cursor-contract").Count(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 1, count)
	})
}

func mustPricingTimestamp(t *testing.T, value string) bunmodel.Timestamp {
	t.Helper()
	timestamp, err := bunmodel.ParseTimestamp(value)
	require.NoError(t, err)
	return timestamp
}
