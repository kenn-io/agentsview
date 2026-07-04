//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
)

// TestLoadPricingMapAppliesCustomWhenTableMissing covers the fresh-PG
// case where `agentsview pg push` has not seeded model_pricing yet.
// loadPricingMap must still honor config.CustomModelPricing on that
// fallback path.
func TestLoadPricingMapAppliesCustomWhenTableMissing(t *testing.T) {
	_, store := prepareUsageSchema(
		t, "agentsview_pricing_missing_table_test",
	)

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `DROP TABLE model_pricing`)
	require.NoError(t, err, "drop model_pricing")

	store.SetCustomPricing(map[string]config.CustomModelRate{
		"acme-ultra-2.1": {Input: 9.0, Output: 18.0},
	})

	out, err := store.loadPricingMap(ctx)
	require.NoError(t, err, "loadPricingMap")

	byPattern := pricingRowsByPattern(out)
	got, ok := byPattern["acme-ultra-2.1"]
	require.True(t, ok, "custom model missing from pricing map")
	assert.InDelta(t, 9.0, got.InputPerMTok, 0.001)
	assert.InDelta(t, 18.0, got.OutputPerMTok, 0.001)
	assert.Equal(t, export.PricingRowSourceCustom, got.Source)

	// Fallback pricing must still populate the map so real models
	// continue to resolve when custom_model_pricing only covers a
	// subset.
	assert.GreaterOrEqual(t, len(out), 2,
		"pricing map should have fallback + custom entries")
}
