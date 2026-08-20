package export_test

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestPricingBlockJSONShape(t *testing.T) {
	latestRowUpdatedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	matchedPattern := "claude-*"
	block := export.PricingBlock{
		Source:              "custom+embedded",
		TableVersion:        "2026-07-03",
		LatestRowUpdatedAt:  &latestRowUpdatedAt,
		CustomOverrideCount: 1,
		EffectiveRowCount:   2,
		Digest:              "sha256:test",
		CostSource:          export.CostSourceMixed,
		Fallback: export.PricingFallback{
			Used:   true,
			Models: []string{"unknown-model"},
		},
		Models: map[string]export.ModelPricingProvenance{
			"claude-test": {
				CostSource: export.CostSourceComputed,
				Resolutions: []export.EffectiveModelRate{{
					PricedModel:           "claude-test",
					MatchedPattern:        &matchedPattern,
					InputCostPerMTok:      money.MustParseDollars("3"),
					OutputCostPerMTok:     money.MustParseDollars("15"),
					CacheWriteCostPerMTok: money.MustParseDollars("3.75"),
					CacheReadCostPerMTok:  money.MustParseDollars("0.30"),
					CostSource:            export.CostSourceComputed,
					Bands: []export.PricingBand{{
						AboveInputTokens:  200_000,
						InputPerMTok:      money.MustParseDollars("6"),
						OutputPerMTok:     money.MustParseDollars("22.50"),
						CacheWritePerMTok: money.MustParseDollars("7.50"),
						CacheReadPerMTok:  money.MustParseDollars("0.60"),
					}},
					Application: export.PricingApplication{
						BaseRequestCount:  2,
						AggregateRowCount: 1,
						Bands: []export.AppliedPricingBand{{
							AboveInputTokens: 200_000,
							RequestCount:     3,
						}},
					},
				}},
			},
		},
	}

	got := mustMarshalJSON(t, block)
	assert.JSONEq(t, `{
		"source": "custom+embedded",
		"table_version": "2026-07-03",
		"latest_row_updated_at": "2026-07-03T12:00:00Z",
		"custom_override_count": 1,
		"effective_row_count": 2,
		"digest": "sha256:test",
		"cost_source": "mixed",
		"fallback": {
			"used": true,
			"models": ["unknown-model"]
		},
		"models": {
			"claude-test": {
				"cost_source": "computed",
				"resolutions": [{
					"priced_model": "claude-test",
					"matched_pattern": "claude-*",
					"input_cost_per_mtok": {"microdollars": 3000000},
					"output_cost_per_mtok": {"microdollars": 15000000},
					"cache_write_cost_per_mtok": {"microdollars": 3750000},
					"cache_read_cost_per_mtok": {"microdollars": 300000},
					"cost_source": "computed",
					"bands": [{
						"above_input_tokens": 200000,
						"input_cost_per_mtok": {"microdollars": 6000000},
						"output_cost_per_mtok": {"microdollars": 22500000},
						"cache_write_cost_per_mtok": {"microdollars": 7500000},
						"cache_read_cost_per_mtok": {"microdollars": 600000}
					}],
					"application": {
						"base_request_count": 2,
						"aggregate_row_count": 1,
						"bands": [{"above_input_tokens": 200000, "request_count": 3}]
					}
				}]
			}
		}
	}`, got)
	assert.Contains(t, got, `"models"`)
	assert.NotContains(t, got, `"effective_model_rates"`)
	assert.Contains(t, got, `"cost_source":"mixed"`)
}

func TestCostSourceEnumJSONShape(t *testing.T) {
	got := mustMarshalJSON(t, []export.CostSource{
		export.CostSourceComputed,
		export.CostSourceReported,
		export.CostSourceMixed,
	})

	assert.JSONEq(t, `["computed","reported","mixed"]`, got)
}

func TestProjectResolutionEnumJSONShape(t *testing.T) {
	got := mustMarshalJSON(t, []export.ProjectResolution{
		export.ProjectResolutionResolved,
		export.ProjectResolutionUnknown,
		export.ProjectResolutionAmbiguous,
	})

	assert.JSONEq(t, `["resolved","unknown","ambiguous"]`, got)
}

func TestSessionClassificationEnumJSONShape(t *testing.T) {
	got := mustMarshalJSON(t, []export.SessionClassification{
		export.SessionClassificationInteractive,
		export.SessionClassificationAutomated,
	})

	assert.JSONEq(t, `["interactive","automated"]`, got)
}

func mustMarshalJSON(t *testing.T, v any) string {
	t.Helper()

	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
