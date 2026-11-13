package db

import (
	"fmt"
	"slices"

	"go.kenn.io/agentsview/internal/pricing"
)

// PricingSyncPlan is what a push writes to a target so its pricing
// matches the local catalog.
type PricingSyncPlan struct {
	// Changed holds the rows to upsert.
	Changed []ModelPricing
	// Remove holds the patterns to delete.
	Remove []string
	// OpenRouterModels is the OpenRouter ownership list to store on the
	// target, as written by pricing.EncodeOpenRouterModels. It is set
	// only when it differs from the target's list.
	OpenRouterModels      string
	WriteOpenRouterModels bool
}

// PlanModelPricingSync compares a push target's pricing rows and
// OpenRouter ownership list with the local ones and returns the writes
// that make the target match.
//
// A shared target can hold rows other machines pushed, so the plan
// applies the local refresh's rules in both directions, driven by the
// OpenRouter ownership lists rather than by set difference. A local
// OpenRouter row is withheld when a target row from another source
// already covers its model, and a target-owned row is removed only when
// the local catalog covers its model under a different pattern, where
// keeping it would tie with the replacement. Rows the local catalog
// lacks entirely, such as a delisted model another machine still
// prices, are left alone. The pushed list merges target and local
// ownership, dropping only patterns this push withholds or retires or
// that a local non-OpenRouter row now supplies, so no machine strips
// ownership another machine recorded.
func PlanModelPricingSync(
	existing []ModelPricing, existingOpenRouter string,
	desired []ModelPricing, desiredOpenRouter string,
) (PricingSyncPlan, error) {
	targetOwned, err := pricing.DecodeOpenRouterModels(existingOpenRouter)
	if err != nil {
		return PricingSyncPlan{}, fmt.Errorf("target OpenRouter models: %w", err)
	}
	localOwned, err := pricing.DecodeOpenRouterModels(desiredOpenRouter)
	if err != nil {
		return PricingSyncPlan{}, fmt.Errorf("local OpenRouter models: %w", err)
	}
	targetForeign := make([]string, 0, len(existing))
	for _, row := range existing {
		if !slices.Contains(targetOwned, row.ModelPattern) {
			targetForeign = append(targetForeign, row.ModelPattern)
		}
	}
	withheld := pricing.CoveredPatterns(targetForeign, localOwned)

	local := make([]string, 0, len(desired))
	models := make([]ModelPricing, 0, len(desired))
	for _, row := range desired {
		if slices.Contains(withheld, row.ModelPattern) {
			continue
		}
		local = append(local, row.ModelPattern)
		models = append(models, row)
	}
	plan := PricingSyncPlan{Remove: pricing.ShadowedPatterns(local, targetOwned)}

	owned := make([]string, 0, len(localOwned)+len(targetOwned))
	for _, pattern := range localOwned {
		if !slices.Contains(withheld, pattern) {
			owned = append(owned, pattern)
		}
	}
	for _, pattern := range targetOwned {
		if slices.Contains(plan.Remove, pattern) ||
			(slices.Contains(local, pattern) &&
				!slices.Contains(localOwned, pattern)) {
			continue
		}
		owned = append(owned, pattern)
	}
	slices.Sort(owned)
	owned = slices.Compact(owned)

	_, plan.Changed = FilterChangedModelPricing(existing, models)
	if existingOpenRouter == "" && desiredOpenRouter == "" {
		return plan, nil
	}
	if merged := pricing.EncodeOpenRouterModels(owned); merged != existingOpenRouter {
		plan.OpenRouterModels = merged
		plan.WriteOpenRouterModels = true
	}
	return plan, nil
}
