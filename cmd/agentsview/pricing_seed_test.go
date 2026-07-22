package main

import (
	"testing"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/pricingrefresh"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func upsertPricingForTest(
	t *testing.T, database *db.DB, prices []pricing.ModelPricing,
) {
	t.Helper()
	rows := make([]db.ModelPricing, len(prices))
	for i, p := range prices {
		rows[i] = db.ModelPricing{
			ModelPattern:         p.ModelPattern,
			InputPerMTok:         p.InputPerMTok,
			OutputPerMTok:        p.OutputPerMTok,
			CacheCreationPerMTok: p.CacheCreationPerMTok,
			CacheReadPerMTok:     p.CacheReadPerMTok,
		}
	}
	require.NoError(t, database.UpsertModelPricing(rows))
}

// TestSeedFallbackPricing_UpgradesExistingDBWithSupplementals proves the
// upgrade path: a database seeded by an older binary (stored meta equals
// the bare snapshot version) re-seeds on startup, picking up the
// supplemental flat K3 aliases without a resync.
func TestSeedFallbackPricing_UpgradesExistingDBWithSupplementals(t *testing.T) {
	database := newTestDB(t)

	// Simulate a DB seeded by the pre-supplemental binary: meta holds
	// the bare snapshot version and the alias rows are absent.
	require.NoError(t,
		database.SetPricingMeta("_fallback_version", pricing.FallbackVersion))

	require.NoError(t, pricingrefresh.SeedFallback(database))

	for _, model := range []string{
		"k3",
		"k3-agent",
		"kimi-k3",
		"moonshot/kimi-k3",
	} {
		row, err := database.GetModelPricing(model)
		require.NoError(t, err)
		require.NotNil(t, row, "supplemental %q must be seeded", model)
		assert.Equal(t, 3.00, row.InputPerMTok, "%s input rate", model)
		assert.Equal(t, 15.00, row.OutputPerMTok, "%s output rate", model)
		assert.Zero(t, row.CacheCreationPerMTok, "%s cache creation rate", model)
		assert.Equal(t, 0.30, row.CacheReadPerMTok, "%s cache read rate", model)
	}

	meta, err := database.GetPricingMeta("_fallback_version")
	require.NoError(t, err)
	assert.Equal(t, pricing.SeedVersion, meta)
}

// TestSeedFallbackPricing_DeletesStaleDateAliasRows proves the
// supplemental-v2 upgrade deletes the flat K2.6 rows older binaries
// seeded for the date-ambiguous aliases: an exact-match row would
// otherwise shadow the date-based CanonicalModelForDate pricing path.
func TestSeedFallbackPricing_DeletesStaleDateAliasRows(t *testing.T) {
	database := newTestDB(t)

	// Simulate a DB seeded by the supplemental-v1 binary: meta holds
	// the v1 seed version and the three ambiguous names carry flat
	// K2.6 rows.
	upsertPricingForTest(t, database, []pricing.ModelPricing{
		{
			ModelPattern:     "kimi-for-coding",
			InputPerMTok:     0.95,
			OutputPerMTok:    4.0,
			CacheReadPerMTok: 0.16,
		},
		{
			ModelPattern:     "daimon-kimi-code",
			InputPerMTok:     0.95,
			OutputPerMTok:    4.0,
			CacheReadPerMTok: 0.16,
		},
		{
			ModelPattern:     "daimon-kimi-messages",
			InputPerMTok:     0.95,
			OutputPerMTok:    4.0,
			CacheReadPerMTok: 0.16,
		},
	})
	require.NoError(t,
		database.SetPricingMeta("_fallback_version",
			pricing.FallbackVersion+"+supplemental-1"))

	require.NoError(t, pricingrefresh.SeedFallback(database))

	for _, model := range pricing.DateAliasedModels() {
		row, err := database.GetModelPricing(model)
		require.NoError(t, err)
		assert.Nil(t, row,
			"stale flat row for date-ambiguous %q must be deleted", model)
	}

	// The new static K3 rows arrive in the same seed.
	for _, model := range []string{"k3", "k3-agent", "kimi-k3", "moonshot/kimi-k3"} {
		row, err := database.GetModelPricing(model)
		require.NoError(t, err)
		require.NotNil(t, row, "supplemental %q must be seeded", model)
		assert.Equal(t, 3.00, row.InputPerMTok, "%s input rate", model)
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
	require.NoError(t, pricingrefresh.SeedFallback(database))

	// Simulate a LiteLLM refresh overwriting the alias with real rates.
	upsertPricingForTest(t, database, []pricing.ModelPricing{{
		ModelPattern:  "kimi-k3",
		InputPerMTok:  9.9,
		OutputPerMTok: 99.9,
	}})

	require.NoError(t, pricingrefresh.SeedFallback(database))

	row, err := database.GetModelPricing("kimi-k3")
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
	require.NoError(t, pricingrefresh.SeedFallback(database))

	upsertPricingForTest(t, database, []pricing.ModelPricing{{
		ModelPattern:         "kimi-k3",
		InputPerMTok:         1.5,
		OutputPerMTok:        6.0,
		CacheCreationPerMTok: 0.5,
		CacheReadPerMTok:     0.05,
	}})

	row, err := database.GetModelPricing("kimi-k3")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, 1.5, row.InputPerMTok)
	assert.Equal(t, 6.0, row.OutputPerMTok)
	assert.Equal(t, 0.5, row.CacheCreationPerMTok)
	assert.Equal(t, 0.05, row.CacheReadPerMTok)
}
