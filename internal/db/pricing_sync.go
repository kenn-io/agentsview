package db

import (
	"encoding/json"
	"fmt"
	"sort"

	"go.kenn.io/agentsview/internal/pricing"
)

// PlanModelPricingSync compares a backend's existing pricing rows with the
// local SQLite rows and returns the rows to upsert and patterns to remove.
//
// Deletions are provenance-driven instead of a set difference because a
// shared backend can contain pricing another machine pushed:
//
//   - _openrouter_aliases lists the aliases a refresh currently emits.
//     Aliases present in the backend's previous list but absent locally were
//     retired.
//   - _openrouter_shadowed lists patterns suppressed because a
//     higher-priority source covers the same canonical model. Every listed
//     pattern must be absent from the backend.
func PlanModelPricingSync(
	existing, desired []ModelPricing,
) ([]ModelPricing, []string, error) {
	removeSet := make(map[string]struct{})

	shadowedMeta, shadowedFound := pricingMetaRow(
		desired, pricing.OpenRouterShadowedMetaKey,
	)
	if shadowedFound {
		shadowed, err := decodePricingPatterns(shadowedMeta.UpdatedAt)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"decoding local OpenRouter shadowed patterns: %w", err,
			)
		}
		for _, pattern := range shadowed {
			removeSet[pattern] = struct{}{}
		}
	}

	aliasMeta, aliasFound := pricingMetaRow(
		desired, pricing.OpenRouterAliasesMetaKey,
	)
	existingAliasMeta, existingAliasFound := pricingMetaRow(
		existing, pricing.OpenRouterAliasesMetaKey,
	)
	if aliasFound && existingAliasFound {
		currentAliases, err := decodePricingPatterns(aliasMeta.UpdatedAt)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"decoding local OpenRouter aliases: %w", err,
			)
		}
		previousAliases, err := decodePricingPatterns(
			existingAliasMeta.UpdatedAt,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"decoding backend OpenRouter aliases: %w", err,
			)
		}
		current := make(map[string]struct{}, len(currentAliases))
		for _, alias := range currentAliases {
			current[alias] = struct{}{}
		}
		for _, alias := range previousAliases {
			if _, ok := current[alias]; !ok {
				removeSet[alias] = struct{}{}
			}
		}
	}

	remove := make([]string, 0, len(removeSet))
	for pattern := range removeSet {
		remove = append(remove, pattern)
	}
	sort.Strings(remove)

	// Removed patterns are dropped from the comparison baseline so a pattern
	// a surviving source still publishes is re-upserted after the delete.
	kept := make([]ModelPricing, 0, len(existing))
	for _, row := range existing {
		if _, removing := removeSet[row.ModelPattern]; !removing {
			kept = append(kept, row)
		}
	}
	_, changed := FilterChangedModelPricing(kept, desired)
	if aliasFound {
		changed = appendChangedPricingMeta(changed, existing, aliasMeta)
	}
	if shadowedFound {
		changed = appendChangedPricingMeta(changed, existing, shadowedMeta)
	}
	return changed, remove, nil
}

// appendChangedPricingMeta re-adds a provenance sentinel whose payload
// changed. FilterChangedModelPricing compares only rate fields and sentinels
// carry all zeros, so a new payload alone does not mark the row changed.
func appendChangedPricingMeta(
	changed, existing []ModelPricing, meta ModelPricing,
) []ModelPricing {
	previous, found := pricingMetaRow(existing, meta.ModelPattern)
	if !found || previous.UpdatedAt == meta.UpdatedAt {
		return changed
	}
	for _, row := range changed {
		if row.ModelPattern == meta.ModelPattern {
			return changed
		}
	}
	return append(changed, meta)
}

func pricingMetaRow(
	rows []ModelPricing, key string,
) (ModelPricing, bool) {
	for _, row := range rows {
		if row.ModelPattern == key {
			return row, true
		}
	}
	return ModelPricing{}, false
}

func decodePricingPatterns(value string) ([]string, error) {
	var patterns []string
	if err := json.Unmarshal([]byte(value), &patterns); err != nil {
		return nil, err
	}
	return patterns, nil
}
