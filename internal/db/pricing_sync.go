package db

import (
	"fmt"
	"slices"

	"go.kenn.io/agentsview/internal/pricing"
)

// PlanModelPricingSync compares a push target's pricing rows with the
// local rows and returns the rows to upsert and the patterns to delete.
//
// A shared target can hold rows other machines pushed, so the plan
// applies the local refresh's rules in both directions, driven by the
// OpenRouter sentinels rather than by set difference. A local OpenRouter
// row is withheld when a target row from another source already covers
// its model, and a target-owned row is removed only when the local
// catalog covers its model under a different pattern, where keeping it
// would tie with the replacement. Rows the local catalog lacks entirely,
// such as a delisted model another machine still prices, are left alone.
// The pushed sentinel merges target and local ownership, dropping only
// patterns this push withholds or retires or that a local non-OpenRouter
// row now supplies, so no machine strips ownership another machine
// recorded.
func PlanModelPricingSync(
	existing, desired []ModelPricing,
) (changed []ModelPricing, remove []string, err error) {
	targetValue, targetOwned, err := openRouterModels(existing)
	if err != nil {
		return nil, nil, fmt.Errorf("target OpenRouter models: %w", err)
	}
	localValue, localOwned, err := openRouterModels(desired)
	if err != nil {
		return nil, nil, fmt.Errorf("local OpenRouter models: %w", err)
	}
	targetForeign := make([]string, 0, len(existing))
	for _, row := range existing {
		if !isPricingMetaPattern(row.ModelPattern) &&
			!slices.Contains(targetOwned, row.ModelPattern) {
			targetForeign = append(targetForeign, row.ModelPattern)
		}
	}
	withheld := pricing.CoveredPatterns(targetForeign, localOwned)

	local := make([]string, 0, len(desired))
	models := make([]ModelPricing, 0, len(desired))
	for _, row := range desired {
		if isPricingMetaPattern(row.ModelPattern) ||
			slices.Contains(withheld, row.ModelPattern) {
			continue
		}
		local = append(local, row.ModelPattern)
		models = append(models, row)
	}
	remove = pricing.ShadowedPatterns(local, targetOwned)

	owned := make([]string, 0, len(localOwned)+len(targetOwned))
	for _, pattern := range localOwned {
		if !slices.Contains(withheld, pattern) {
			owned = append(owned, pattern)
		}
	}
	for _, pattern := range targetOwned {
		if slices.Contains(remove, pattern) ||
			(slices.Contains(local, pattern) &&
				!slices.Contains(localOwned, pattern)) {
			continue
		}
		owned = append(owned, pattern)
	}
	slices.Sort(owned)
	owned = slices.Compact(owned)

	_, changed = FilterChangedModelPricing(existing, models)
	if targetValue == "" && localValue == "" {
		return changed, remove, nil
	}
	if merged := pricing.EncodeOpenRouterModels(owned); merged != targetValue {
		changed = append(changed, ModelPricing{
			ModelPattern: pricing.OpenRouterModelsMetaKey,
			UpdatedAt:    merged,
		})
	}
	return changed, remove, nil
}

// openRouterModels finds the OpenRouter ownership sentinel in rows and
// returns its raw value and decoded patterns; both are empty when rows
// carry no sentinel.
func openRouterModels(rows []ModelPricing) (string, []string, error) {
	for _, row := range rows {
		if row.ModelPattern != pricing.OpenRouterModelsMetaKey {
			continue
		}
		patterns, err := pricing.DecodeOpenRouterModels(row.UpdatedAt)
		return row.UpdatedAt, patterns, err
	}
	return "", nil, nil
}
