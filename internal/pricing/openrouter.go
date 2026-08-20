package pricing

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"

	"go.kenn.io/agentsview/internal/pricing/catalog"
)

// OpenRouterModelsMetaKey names the model_pricing sentinel row whose
// updated_at holds the JSON list of patterns OpenRouter contributed to the
// stored catalog. Refreshes use it to retire those rows once LiteLLM lists
// the same model, and push targets use it to mirror that retirement.
const OpenRouterModelsMetaKey = "_openrouter_models"

// FetchOpenRouterPricingContext downloads the OpenRouter model catalog and
// binds the request lifetime to ctx.
func FetchOpenRouterPricingContext(
	ctx context.Context,
) ([]ModelPricing, error) {
	return catalog.FetchOpenRouterPricingContext(ctx)
}

// ParseOpenRouterPricing parses the OpenRouter /models JSON into
// ModelPricing entries. See catalog.ParseOpenRouterPricing.
func ParseOpenRouterPricing(data []byte) ([]ModelPricing, error) {
	return catalog.ParseOpenRouterPricing(data)
}

// Catalog is one upstream pricing snapshot: the LiteLLM and OpenRouter
// rows as fetched. Reconcile merges them over a stored table.
type Catalog struct {
	LiteLLM    []ModelPricing
	OpenRouter []ModelPricing
}

// FetchCatalog fetches LiteLLM and OpenRouter. A LiteLLM failure fails
// the whole snapshot and the caller keeps its last stored pricing. An
// OpenRouter failure returns a LiteLLM-only snapshot alongside the
// error: Reconcile treats the empty OpenRouter side as delisting
// nothing, so previously stored OpenRouter rows stay priced and tracked
// while the fresh LiteLLM rates are still stored, and the caller
// surfaces the error as a warning.
func FetchCatalog() (Catalog, error) {
	return FetchCatalogContext(context.Background())
}

// FetchCatalogContext is FetchCatalog bound to ctx.
func FetchCatalogContext(ctx context.Context) (Catalog, error) {
	return fetchCatalog(
		ctx, FetchLiteLLMPricingContext, FetchOpenRouterPricingContext,
	)
}

func fetchCatalog(
	ctx context.Context,
	fetchLiteLLM, fetchOpenRouter func(context.Context) ([]ModelPricing, error),
) (Catalog, error) {
	litellm, err := fetchLiteLLM(ctx)
	if err != nil {
		return Catalog{}, err
	}
	openrouter, err := fetchOpenRouter(ctx)
	if err != nil {
		return Catalog{LiteLLM: litellm}, fmt.Errorf(
			"fetching openrouter catalog (storing litellm rates only): %w",
			err,
		)
	}
	return Catalog{LiteLLM: litellm, OpenRouter: openrouter}, nil
}

// Reconcile merges the catalog over a stored table and returns the rows to
// store, the OpenRouter ownership list to record, and the stored patterns
// to delete. stored lists every stored model pattern; previous lists the
// patterns an earlier refresh took from OpenRouter.
//
// An OpenRouter row is dropped when the LiteLLM catalog or a stored row
// from another source (LiteLLM, embedded, supplemental) already lists a
// model with the same canonical name under any spelling or provider
// prefix, so adding OpenRouter never changes a lookup that already
// resolved: two provider-qualified spellings of one model (LiteLLM's
// openrouter/openai/gpt-x against OpenRouter's openai/gpt-x) would
// otherwise tie in the resolver and leave a bare session model name
// unpriced. The cost is that a session naming the OpenRouter id exactly
// stays unpriced in that case, as it did before. Rows are copied whole; a
// zero rate is a valid price for free models, so fields are never
// backfilled across sources.
//
// Of the previously stored OpenRouter rows, those the merged catalog or a
// stored row from another source now covers under a different spelling
// are retired (they would tie with the replacement), those the catalog
// lists exactly follow the catalog's ownership, and rows OpenRouter merely
// delisted stay stored and tracked so past sessions keep their price.
func (c Catalog) Reconcile(
	stored, previous []string,
) (prices []ModelPricing, owned, retired []string) {
	covering := make([]string, 0, len(c.LiteLLM)+len(stored))
	for _, p := range c.LiteLLM {
		covering = append(covering, p.ModelPattern)
	}
	for _, pattern := range stored {
		if !slices.Contains(previous, pattern) {
			covering = append(covering, pattern)
		}
	}
	candidates := make([]string, len(c.OpenRouter))
	for i, p := range c.OpenRouter {
		candidates[i] = p.ModelPattern
	}
	dropped := CoveredPatterns(covering, candidates)
	prices = c.LiteLLM
	for _, p := range c.OpenRouter {
		if slices.Contains(dropped, p.ModelPattern) {
			continue
		}
		prices = append(prices, p)
		owned = append(owned, p.ModelPattern)
	}

	listed := make([]string, 0, len(prices))
	for _, p := range prices {
		listed = append(listed, p.ModelPattern)
	}
	retired = ShadowedPatterns(append(listed, covering...), previous)
	for _, pattern := range previous {
		if !slices.Contains(listed, pattern) &&
			!slices.Contains(retired, pattern) {
			owned = append(owned, pattern)
		}
	}
	slices.Sort(owned)
	return prices, slices.Compact(owned), retired
}

// CoveredPatterns returns the candidates whose canonical model name some
// pattern in covering shares, including exact listings. An OpenRouter row
// covered by a higher-priority source must not be stored beside it.
func CoveredPatterns(covering, candidates []string) []string {
	covered := make(map[string]struct{}, len(covering))
	for _, pattern := range covering {
		covered[canonicalize(pattern)] = struct{}{}
	}
	var out []string
	for _, pattern := range candidates {
		if _, ok := covered[canonicalize(pattern)]; ok {
			out = append(out, pattern)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ShadowedPatterns returns the candidates that catalog covers under a
// different pattern with the same canonical name. Such a stored row would
// tie with the catalog's row in the resolver, so it must be retired.
// Candidates the catalog lists exactly or does not cover at all are not
// returned; a delisted model that nothing replaces keeps its price.
func ShadowedPatterns(catalog, candidates []string) []string {
	var shadowed []string
	for _, pattern := range CoveredPatterns(catalog, candidates) {
		if !slices.Contains(catalog, pattern) {
			shadowed = append(shadowed, pattern)
		}
	}
	return shadowed
}

// EncodeOpenRouterModels renders an ownership list as the sentinel value.
func EncodeOpenRouterModels(patterns []string) string {
	if patterns == nil {
		patterns = []string{}
	}
	encoded, _ := json.Marshal(patterns) // a []string always encodes
	return string(encoded)
}

// DecodeOpenRouterModels parses a sentinel value written by
// EncodeOpenRouterModels. An empty value decodes to no patterns.
func DecodeOpenRouterModels(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	var patterns []string
	if err := json.Unmarshal([]byte(value), &patterns); err != nil {
		return nil, fmt.Errorf(
			"decoding %s (delete that model_pricing row on the "+
				"affected database to reset OpenRouter ownership "+
				"tracking): %w",
			OpenRouterModelsMetaKey, err,
		)
	}
	return patterns, nil
}
