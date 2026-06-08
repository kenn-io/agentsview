package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrationCreatesModelPricingTable(t *testing.T) {
	d := testDB(t)

	var count int
	err := d.getReader().QueryRow(
		`SELECT count(*) FROM pragma_table_info('model_pricing')`,
	).Scan(&count)
	require.NoError(t, err, "pragma_table_info")

	assert.NotZero(t, count, "model_pricing table not created by schema")
}

func TestUpsertModelPricing(t *testing.T) {
	d := testDB(t)

	prices := []ModelPricing{
		{
			ModelPattern:         "claude-sonnet-4",
			InputPerMTok:         3.0,
			OutputPerMTok:        15.0,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.30,
		},
	}

	err := d.UpsertModelPricing(prices)
	require.NoError(t, err, "UpsertModelPricing")

	got, err := d.GetModelPricing("claude-sonnet-4")
	require.NoError(t, err, "GetModelPricing")
	require.NotNil(t, got, "expected pricing")

	assert.Equal(t, "claude-sonnet-4", got.ModelPattern)
	assert.Equal(t, 3.0, got.InputPerMTok)
	assert.Equal(t, 15.0, got.OutputPerMTok)
	assert.Equal(t, 3.75, got.CacheCreationPerMTok)
	assert.Equal(t, 0.30, got.CacheReadPerMTok)
	assert.NotEmpty(t, got.UpdatedAt, "expected UpdatedAt to be set")
}

func TestUpsertModelPricingOverwrites(t *testing.T) {
	d := testDB(t)

	initial := []ModelPricing{
		{
			ModelPattern:         "claude-opus-4",
			InputPerMTok:         15.0,
			OutputPerMTok:        75.0,
			CacheCreationPerMTok: 18.75,
			CacheReadPerMTok:     1.50,
		},
	}
	err := d.UpsertModelPricing(initial)
	require.NoError(t, err, "UpsertModelPricing initial")

	updated := []ModelPricing{
		{
			ModelPattern:         "claude-opus-4",
			InputPerMTok:         10.0,
			OutputPerMTok:        50.0,
			CacheCreationPerMTok: 12.50,
			CacheReadPerMTok:     1.00,
		},
	}
	err = d.UpsertModelPricing(updated)
	require.NoError(t, err, "UpsertModelPricing updated")

	got, err := d.GetModelPricing("claude-opus-4")
	require.NoError(t, err, "GetModelPricing after update")
	require.NotNil(t, got, "expected pricing")

	assert.Equal(t, 10.0, got.InputPerMTok)
	assert.Equal(t, 50.0, got.OutputPerMTok)
	assert.Equal(t, 12.50, got.CacheCreationPerMTok)
	assert.Equal(t, 1.00, got.CacheReadPerMTok)
}

func TestFilterChangedModelPricingIgnoresUpdatedAtOnlyDifferences(t *testing.T) {
	existing := []ModelPricing{
		{
			ModelPattern:         "_fallback_version",
			InputPerMTok:         0,
			OutputPerMTok:        0,
			CacheCreationPerMTok: 0,
			CacheReadPerMTok:     0,
			UpdatedAt:            "v1",
		},
		{
			ModelPattern:         "same-model",
			InputPerMTok:         1,
			OutputPerMTok:        2,
			CacheCreationPerMTok: 3,
			CacheReadPerMTok:     4,
			UpdatedAt:            "old",
		},
		{
			ModelPattern:         "changed-model",
			InputPerMTok:         1,
			OutputPerMTok:        2,
			CacheCreationPerMTok: 3,
			CacheReadPerMTok:     4,
			UpdatedAt:            "old",
		},
	}
	desired := []ModelPricing{
		{
			ModelPattern:         "_fallback_version",
			InputPerMTok:         0,
			OutputPerMTok:        0,
			CacheCreationPerMTok: 0,
			CacheReadPerMTok:     0,
			UpdatedAt:            "v2",
		},
		{
			ModelPattern:         "same-model",
			InputPerMTok:         1,
			OutputPerMTok:        2,
			CacheCreationPerMTok: 3,
			CacheReadPerMTok:     4,
			UpdatedAt:            "new",
		},
		{
			ModelPattern:         "changed-model",
			InputPerMTok:         1,
			OutputPerMTok:        9,
			CacheCreationPerMTok: 3,
			CacheReadPerMTok:     4,
			UpdatedAt:            "new",
		},
		{
			ModelPattern:         "missing-model",
			InputPerMTok:         5,
			OutputPerMTok:        6,
			CacheCreationPerMTok: 7,
			CacheReadPerMTok:     8,
			UpdatedAt:            "new",
		},
	}

	gotSummary, gotRows := FilterChangedModelPricing(existing, desired)

	assert.Equal(t, PricingChangeSummary{
		Total:     4,
		Missing:   1,
		Changed:   1,
		Unchanged: 2,
	}, gotSummary)
	require.Len(t, gotRows, 2)
	assert.Equal(t, "changed-model", gotRows[0].ModelPattern)
	assert.Equal(t, "missing-model", gotRows[1].ModelPattern)
}

func TestPricingMeta(t *testing.T) {
	d := testDB(t)

	// Initially empty.
	got, err := d.GetPricingMeta("_fallback_version")
	require.NoError(t, err, "GetPricingMeta empty")
	require.Empty(t, got)

	// Set and read back.
	require.NoError(t,
		d.SetPricingMeta("_fallback_version", "v1"),
		"SetPricingMeta v1")
	got, err = d.GetPricingMeta("_fallback_version")
	require.NoError(t, err, "GetPricingMeta v1")
	require.Equal(t, "v1", got)

	// Update overwrites.
	require.NoError(t,
		d.SetPricingMeta("_fallback_version", "v2"),
		"SetPricingMeta v2")
	got, err = d.GetPricingMeta("_fallback_version")
	require.NoError(t, err, "GetPricingMeta v2")
	require.Equal(t, "v2", got)

	// Sentinel row does not interfere with model lookups.
	p, err := d.GetModelPricing("_fallback_version")
	require.NoError(t, err, "GetModelPricing sentinel")
	if p != nil {
		assert.Zero(t, p.InputPerMTok,
			"sentinel should have zero pricing, got %+v", p)
	}
}

func TestGetModelPricingNotFound(t *testing.T) {
	d := testDB(t)

	got, err := d.GetModelPricing("nonexistent-model")
	require.NoError(t, err, "GetModelPricing not found")
	assert.Nil(t, got, "expected nil")
}

func TestInsertMissingModelPricing_DoesNotOverwrite(t *testing.T) {
	d := testDB(t)

	// Seed an existing row (simulating a LiteLLM rate already present).
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         5.0,
		OutputPerMTok:        25.0,
		CacheCreationPerMTok: 6.25,
		CacheReadPerMTok:     0.5,
	}}), "UpsertModelPricing")

	// Insert-missing with a DIFFERENT rate for the same pattern, plus a
	// brand-new pattern.
	err := d.InsertMissingModelPricing([]ModelPricing{
		{ModelPattern: "claude-opus-4-6", InputPerMTok: 999.0, OutputPerMTok: 999.0},
		{ModelPattern: "gpt-5.4", InputPerMTok: 2.5, OutputPerMTok: 15.0},
	})
	require.NoError(t, err, "InsertMissingModelPricing")

	// Existing row is untouched.
	opus, err := d.GetModelPricing("claude-opus-4-6")
	require.NoError(t, err, "GetModelPricing opus")
	require.NotNil(t, opus)
	assert.Equal(t, 5.0, opus.InputPerMTok, "opus InputPerMTok not overwritten")
	// New row was inserted.
	gpt, err := d.GetModelPricing("gpt-5.4")
	require.NoError(t, err, "GetModelPricing gpt")
	require.NotNil(t, gpt)
	assert.Equal(t, 2.5, gpt.InputPerMTok, "gpt-5.4 InputPerMTok inserted")
}
