package postgres

import (
	"math"
	"testing"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
)

func TestCustomPricingOverridesPricingMap(t *testing.T) {
	tests := []struct {
		name      string
		dbPrices  []db.ModelPricing
		custom    map[string]config.CustomModelRate
		model     string
		wantInput float64
	}{
		{
			name:      "db pricing only",
			dbPrices:  []db.ModelPricing{{ModelPattern: "acme-ultra-2.1", InputPerMTok: 1.0}},
			model:     "acme-ultra-2.1",
			wantInput: 1.0,
		},
		{
			name:      "custom overrides db",
			dbPrices:  []db.ModelPricing{{ModelPattern: "acme-ultra-2.1", InputPerMTok: 1.0}},
			custom:    map[string]config.CustomModelRate{"acme-ultra-2.1": {Input: 9.0}},
			model:     "acme-ultra-2.1",
			wantInput: 9.0,
		},
		{
			name:      "custom adds new model",
			custom:    map[string]config.CustomModelRate{"new-model": {Input: 4.0}},
			model:     "new-model",
			wantInput: 4.0,
		},
		{
			name:      "custom does not affect other models",
			dbPrices:  []db.ModelPricing{{ModelPattern: "db-model", InputPerMTok: 2.0}},
			custom:    map[string]config.CustomModelRate{"other": {Input: 99.0}},
			model:     "db-model",
			wantInput: 2.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Store{}
			s.SetCustomPricing(tt.custom)
			out := pricingRowsToMap(tt.dbPrices)
			s.applyCustomPricing(out)
			got, ok := out[tt.model]
			if !ok {
				t.Fatalf("model %q not in map", tt.model)
			}
			if math.Abs(got.input-tt.wantInput) > 0.001 {
				t.Errorf("input = %.4f, want %.4f", got.input, tt.wantInput)
			}
		})
	}
}
