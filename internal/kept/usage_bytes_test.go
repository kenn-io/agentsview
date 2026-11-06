package kept

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

func TestUsagePlanByteCap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		loads int
	}{
		{"small result retained", "model-a", 1},
		{"large model map bypasses memo", strings.Repeat("m", 4096), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage Usage
			usage.plan.MaxBytes = 1024
			loads := 0
			load := func(context.Context, db.UsageFilter) (db.PlanUsageSessionCounts, error) {
				loads++
				return db.PlanUsageSessionCounts{PlanOnly: 1, PlanOnlyByModel: map[string]int{tc.model: 1}}, nil
			}
			for range 2 {
				result, err := usage.Plan(t.Context(), db.UsageFilter{}, "v1", load)
				require.NoError(t, err)
				require.Equal(t, 1, result.PlanOnlyByModel[tc.model])
			}
			require.Equal(t, tc.loads, loads)
		})
	}
}

func TestUsageDailyMetadataByteCap(t *testing.T) {
	large := strings.Repeat("m", 4096)
	for _, tc := range []struct {
		name   string
		result db.DailyUsageResult
		loads  int
	}{
		{"small result retained", db.DailyUsageResult{}, 1},
		{"pricing source", db.DailyUsageResult{Pricing: &export.PricingBlock{Source: large}}, 2},
		{"pricing table", db.DailyUsageResult{Pricing: &export.PricingBlock{TableVersion: large}}, 2},
		{"pricing digest", db.DailyUsageResult{Pricing: &export.PricingBlock{Digest: large}}, 2},
		{"pricing cost source", db.DailyUsageResult{Pricing: &export.PricingBlock{CostSource: export.CostSource(large)}}, 2},
		{"fallback models", db.DailyUsageResult{Pricing: &export.PricingBlock{Fallback: export.PricingFallback{Models: []string{large}}}}, 2},
		{"provenance model", db.DailyUsageResult{Pricing: &export.PricingBlock{Models: map[string]export.ModelPricingProvenance{large: {}}}}, 2},
		{"provenance cost source", db.DailyUsageResult{Pricing: &export.PricingBlock{Models: map[string]export.ModelPricingProvenance{"model-a": {CostSource: export.CostSource(large)}}}}, 2},
		{"priced model", db.DailyUsageResult{Pricing: &export.PricingBlock{Models: map[string]export.ModelPricingProvenance{"model-a": {Resolutions: []export.EffectiveModelRate{{PricedModel: large}}}}}}, 2},
		{"matched pattern", db.DailyUsageResult{Pricing: &export.PricingBlock{Models: map[string]export.ModelPricingProvenance{"model-a": {Resolutions: []export.EffectiveModelRate{{MatchedPattern: &large}}}}}}, 2},
		{"rate cost source", db.DailyUsageResult{Pricing: &export.PricingBlock{Models: map[string]export.ModelPricingProvenance{"model-a": {Resolutions: []export.EffectiveModelRate{{CostSource: export.CostSource(large)}}}}}}, 2},
		{"pricing bands", db.DailyUsageResult{Pricing: &export.PricingBlock{Models: map[string]export.ModelPricingProvenance{"model-a": {Resolutions: []export.EffectiveModelRate{{Bands: make([]export.PricingBand, 128)}}}}}}, 2},
		{"applied bands", db.DailyUsageResult{Pricing: &export.PricingBlock{Models: map[string]export.ModelPricingProvenance{"model-a": {Resolutions: []export.EffectiveModelRate{{Application: export.PricingApplication{Bands: make([]export.AppliedPricingBand, 128)}}}}}}}, 2},
		{"project identity", db.DailyUsageResult{Projects: map[string]export.ProjectMapEntry{"project-a": {Identity: &export.ProjectIdentity{NormalizedRemote: large}}}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage Usage
			usage.daily.MaxBytes = 1024
			loads := 0
			load := func(context.Context, db.UsageFilter) (db.DailyUsageResult, error) {
				loads++
				return tc.result, nil
			}
			for range 2 {
				result, err := usage.Daily(t.Context(), db.UsageFilter{}, "v1", load)
				require.NoError(t, err)
				require.Equal(t, tc.result, result)
			}
			require.Equal(t, tc.loads, loads)
		})
	}
}
