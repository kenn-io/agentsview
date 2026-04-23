//go:build pgtest

package postgres

import (
	"context"
	"math"
	"testing"

	"github.com/wesm/agentsview/internal/config"
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
	if _, err := store.DB().ExecContext(
		ctx, `DROP TABLE model_pricing`,
	); err != nil {
		t.Fatalf("drop model_pricing: %v", err)
	}

	store.SetCustomPricing(map[string]config.CustomModelRate{
		"acme-ultra-2.1": {Input: 9.0, Output: 18.0},
	})

	out, err := store.loadPricingMap(ctx)
	if err != nil {
		t.Fatalf("loadPricingMap: %v", err)
	}

	got, ok := out["acme-ultra-2.1"]
	if !ok {
		t.Fatalf("custom model missing from pricing map")
	}
	if math.Abs(got.input-9.0) > 0.001 {
		t.Errorf("input = %.4f, want 9.0", got.input)
	}
	if math.Abs(got.output-18.0) > 0.001 {
		t.Errorf("output = %.4f, want 18.0", got.output)
	}

	// Fallback pricing must still populate the map so real models
	// continue to resolve when custom_model_pricing only covers a
	// subset.
	if len(out) < 2 {
		t.Errorf(
			"pricing map only has %d entries, expected fallback + custom",
			len(out),
		)
	}
}
