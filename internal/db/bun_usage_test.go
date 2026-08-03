package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/export"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func TestBunLoadPricingMapUsesEmbeddedGenAIWithoutStoredDocument(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	_, err := database.bunWriter.NewDelete().
		Model((*bunmodel.GenAIPricing)(nil)).Where("singleton = ?", 1).Exec(ctx)
	require.NoError(t, err)

	rows, err := database.LoadPricingMap(ctx)
	require.NoError(t, err)
	var genAI export.EffectivePricingRow
	for _, row := range rows {
		if row.GenAI != nil {
			genAI = row
			break
		}
	}
	embedded := pricingpkg.EmbeddedGenAIDocument()
	require.NotNil(t, genAI.GenAI)
	assert.Equal(t, embedded.Version, genAI.GenAIVersion)
	assert.Equal(t, export.PricingRowSourceEmbedded, genAI.GenAISource)
	assert.Nil(t, genAI.GenAIUpdatedAt)
}

func TestBunLoadPricingMapUsesStoredGenAIProvenance(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	embedded := pricingpkg.EmbeddedGenAIDocument()
	updatedAt := "2026-08-26T12:34:56.123456789Z"
	_, err := database.bunWriter.NewInsert().Model(&bunmodel.GenAIPricing{
		Singleton: 1, Version: embedded.Version, SourceRef: "upstream-ref",
		Source: GenAIPricingSourceFetched, DataJSON: embedded.RawJSON(),
		UpdatedAt: updatedAt,
	}).On("CONFLICT (singleton) DO UPDATE").
		Set("version = EXCLUDED.version").
		Set("source_ref = EXCLUDED.source_ref").
		Set("source = EXCLUDED.source").
		Set("data_json = EXCLUDED.data_json").
		Set("updated_at = EXCLUDED.updated_at").Exec(ctx)
	require.NoError(t, err)

	rows, err := database.LoadPricingMap(ctx)
	require.NoError(t, err)
	var genAI export.EffectivePricingRow
	for _, row := range rows {
		if row.GenAI != nil {
			genAI = row
			break
		}
	}
	require.NotNil(t, genAI.GenAI)
	assert.Equal(t, embedded.Version, genAI.GenAIVersion)
	assert.Equal(t, export.PricingRowSourceFetched, genAI.GenAISource)
	require.NotNil(t, genAI.GenAIUpdatedAt)
	assert.Equal(t, updatedAt, genAI.GenAIUpdatedAt.Format(time.RFC3339Nano))
}

func TestBunUsageProjectionPreservesRawPricingTimestamp(t *testing.T) {
	usageAt := mustBunTimestamp(t, "2026-07-01T12:00:00Z")
	startedAt := mustBunTimestamp(t, "2026-08-01T12:00:00Z")
	row := usageProjectionToDailyRow(bunUsageProjection{
		UsageTimestamp: &usageAt, SessionStartedAt: &startedAt,
	})

	assert.Equal(t, "2026-07-01T12:00:00Z", row.ts)
	assert.Equal(t, "2026-07-01T12:00:00Z", row.pricingTS)
	full := usageProjectionToFullRow(bunUsageProjection{
		UsageTimestamp: &usageAt, SessionStartedAt: &startedAt,
	})
	assert.Equal(t, "2026-07-01T12:00:00Z", full.pricingTS)
}

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
