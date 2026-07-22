package main

import (
	"testing"

	"go.kenn.io/agentsview/internal/pricing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSeedFallbackPricing_UpgradesExistingDBWithSupplementals proves the
// upgrade path: a database seeded by an older binary (stored meta equals
// the bare snapshot version) re-seeds on startup, picking up the
// supplemental Kimi internal-model aliases without a resync.
func TestSeedFallbackPricing_UpgradesExistingDBWithSupplementals(t *testing.T) {
	database := newTestDB(t)

	// Simulate a DB seeded by the pre-supplemental binary: meta holds
	// the bare snapshot version and the alias rows are absent.
	require.NoError(t,
		database.SetPricingMeta("_fallback_version", pricing.FallbackVersion))

	require.NoError(t, seedFallbackPricing(database))

	for _, model := range []string{
		"daimon-kimi-code",
		"daimon-kimi-messages",
		"k3-agent",
		"kimi-for-coding",
		"k3",
	} {
		row, err := database.GetModelPricing(model)
		require.NoError(t, err)
		require.NotNil(t, row, "supplemental %q must be seeded", model)
		assert.Equal(t, 0.95, row.InputPerMTok, "%s input rate", model)
		assert.Equal(t, 4.0, row.OutputPerMTok, "%s output rate", model)
		assert.Zero(t, row.CacheCreationPerMTok, "%s cache creation rate", model)
		assert.Equal(t, 0.16, row.CacheReadPerMTok, "%s cache read rate", model)
	}

	meta, err := database.GetPricingMeta("_fallback_version")
	require.NoError(t, err)
	assert.Equal(t, pricing.SeedVersion, meta)
}

// TestSeedFallbackPricing_SkipsWhenSeedVersionCurrent proves the gate
// still protects live (LiteLLM-refreshed) rates: once the stored meta
// matches SeedVersion, the seed is a no-op and must not overwrite rows
// a refresh has since updated.
func TestSeedFallbackPricing_SkipsWhenSeedVersionCurrent(t *testing.T) {
	database := newTestDB(t)
	require.NoError(t, seedFallbackPricing(database))

	// Simulate a LiteLLM refresh overwriting the alias with real rates.
	require.NoError(t, upsertPricing(database, []pricing.ModelPricing{{
		ModelPattern:  "daimon-kimi-code",
		InputPerMTok:  9.9,
		OutputPerMTok: 99.9,
	}}))

	require.NoError(t, seedFallbackPricing(database))

	row, err := database.GetModelPricing("daimon-kimi-code")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, 9.9, row.InputPerMTok,
		"seed must not overwrite refreshed rates when SeedVersion is current")
	assert.Equal(t, 99.9, row.OutputPerMTok)
}

// TestSeedFallbackPricing_RefreshOverwritesSupplementals documents that
// supplemental aliases are not pinned: a later LiteLLM upsert replaces
// them if upstream ever lists the real models.
func TestSeedFallbackPricing_RefreshOverwritesSupplementals(t *testing.T) {
	database := newTestDB(t)
	require.NoError(t, seedFallbackPricing(database))

	require.NoError(t, upsertPricing(database, []pricing.ModelPricing{{
		ModelPattern:         "kimi-for-coding",
		InputPerMTok:         1.5,
		OutputPerMTok:        6.0,
		CacheCreationPerMTok: 0.5,
		CacheReadPerMTok:     0.05,
	}}))

	row, err := database.GetModelPricing("kimi-for-coding")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, 1.5, row.InputPerMTok)
	assert.Equal(t, 6.0, row.OutputPerMTok)
	assert.Equal(t, 0.5, row.CacheCreationPerMTok)
	assert.Equal(t, 0.05, row.CacheReadPerMTok)
}
