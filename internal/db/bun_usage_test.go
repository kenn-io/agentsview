package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

func TestUpsertModelPricingRowsReplacesBandsAtomically(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	base := bunmodel.ModelPricing{
		ModelPattern: "atomic-model", InputMicrodollarsPerMTok: 1,
		OutputMicrodollarsPerMTok: 2,
		UpdatedAt:                 mustBunTimestamp(t, "2026-08-03T12:00:00Z"),
	}
	require.NoError(t, UpsertModelPricingRows(
		ctx, database.bunWriter,
		[]bunmodel.ModelPricing{base},
		[]bunmodel.ModelPricingBand{
			{ModelPattern: "atomic-model", AboveInputTokens: 100,
				InputMicrodollarsPerMTok: 3,
				UpdatedAt:                mustBunTimestamp(t, "2026-08-03T12:00:00Z")},
			{ModelPattern: "atomic-model", AboveInputTokens: 200,
				InputMicrodollarsPerMTok: 4,
				UpdatedAt:                mustBunTimestamp(t, "2026-08-03T12:00:00Z")},
		},
	))

	require.NoError(t, UpsertModelPricingRows(
		ctx, database.bunWriter,
		[]bunmodel.ModelPricing{{
			ModelPattern: "atomic-model", InputMicrodollarsPerMTok: 5,
			OutputMicrodollarsPerMTok: 6,
			UpdatedAt:                 mustBunTimestamp(t, "2026-08-03T13:00:00Z"),
		}},
		[]bunmodel.ModelPricingBand{{
			ModelPattern: "atomic-model", AboveInputTokens: 300,
			InputMicrodollarsPerMTok: 7,
			UpdatedAt:                mustBunTimestamp(t, "2026-08-03T13:00:00Z"),
		}},
	))
	stored, err := database.GetModelPricing("atomic-model")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, int64(5), stored.InputPerMTok.Microdollars)
	require.Len(t, stored.Bands, 1)
	assert.Equal(t, 300, stored.Bands[0].AboveInputTokens)
	assert.Equal(t, int64(7), stored.Bands[0].InputPerMTok.Microdollars)

	err = UpsertModelPricingRows(
		ctx, database.bunWriter,
		[]bunmodel.ModelPricing{{
			ModelPattern: "atomic-model", InputMicrodollarsPerMTok: 99,
			OutputMicrodollarsPerMTok: 100,
			UpdatedAt:                 mustBunTimestamp(t, "2026-08-03T14:00:00Z"),
		}},
		[]bunmodel.ModelPricingBand{
			{ModelPattern: "atomic-model", AboveInputTokens: 400,
				InputMicrodollarsPerMTok: 8,
				UpdatedAt:                mustBunTimestamp(t, "2026-08-03T14:00:00Z")},
			{ModelPattern: "atomic-model", AboveInputTokens: 400,
				InputMicrodollarsPerMTok: 9,
				UpdatedAt:                mustBunTimestamp(t, "2026-08-03T14:00:00Z")},
		},
	)
	require.Error(t, err)

	stored, err = database.GetModelPricing("atomic-model")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, int64(5), stored.InputPerMTok.Microdollars,
		"base update must roll back with band replacement")
	require.Len(t, stored.Bands, 1)
	assert.Equal(t, 300, stored.Bands[0].AboveInputTokens,
		"deleted bands must roll back with the failed insert")
}

func mustBunTimestamp(t *testing.T, value string) bunmodel.Timestamp {
	t.Helper()
	timestamp, err := bunmodel.ParseTimestamp(value)
	require.NoError(t, err)
	return timestamp
}
