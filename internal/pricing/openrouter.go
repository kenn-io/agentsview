package pricing

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/agentsview/internal/pricing/catalog"
)

// OpenRouterModelsMetaKey names the model_pricing sentinel row whose
// updated_at holds the JSON list of patterns the online marketplace sources
// (OpenRouter and OrcaRouter) contributed to the stored catalog. Refreshes use
// it to retire those rows once LiteLLM lists the same model, and push targets
// use it to mirror that retirement. The key keeps its historical name even
// though it now tracks OrcaRouter-owned rows alongside OpenRouter-owned rows,
// so databases written before OrcaRouter was added keep their sentinel.
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

// FetchOrcaRouterPricingContext downloads the OrcaRouter model catalog and
// binds the request lifetime to ctx.
func FetchOrcaRouterPricingContext(
	ctx context.Context,
) ([]ModelPricing, error) {
	return catalog.FetchOrcaRouterPricingContext(ctx)
}

// ParseOrcaRouterPricing parses the OrcaRouter /models JSON into
// ModelPricing entries. OrcaRouter publishes the same envelope and price
// fields as OpenRouter, so parsing is shared with that source.
func ParseOrcaRouterPricing(data []byte) ([]ModelPricing, error) {
	return catalog.ParseOpenRouterPricing(data)
}

// Catalog is one refresh result from the GenAI Prices, LiteLLM, OpenRouter,
// and OrcaRouter upstream sources. Reconcile merges the flat fallback rows
// over storage.
type Catalog struct {
	GenAI      *GenAIDocument
	LiteLLM    []ModelPricing
	OpenRouter []ModelPricing
	OrcaRouter []ModelPricing
}

// FetchCatalog fetches GenAI Prices first, followed by LiteLLM, OpenRouter,
// and OrcaRouter. Successful earlier sources remain available when a later
// source fails.
func FetchCatalog() (Catalog, error) {
	return FetchCatalogContext(context.Background())
}

// FetchCatalogContext is FetchCatalog bound to ctx.
func FetchCatalogContext(ctx context.Context) (Catalog, error) {
	return fetchCatalog(
		ctx,
		FetchGenAIPricesContext,
		FetchLiteLLMPricingContext,
		FetchOpenRouterPricingContext,
		FetchOrcaRouterPricingContext,
	)
}

func fetchCatalog(
	ctx context.Context,
	fetchGenAI func(context.Context) (*GenAIPrices, error),
	fetchLiteLLM, fetchOpenRouter,
	fetchOrcaRouter func(context.Context) ([]ModelPricing, error),
) (Catalog, error) {
	var result Catalog
	var warnings []error
	genAI, genAIErr := fetchGenAI(ctx)
	if genAIErr != nil {
		warnings = append(warnings, fmt.Errorf(
			"fetching GenAI Prices (preserving stored document): %w",
			genAIErr,
		))
	} else {
		document, err := NewGenAIDocument(genAI, catalog.GenAIPricesURL)
		if err != nil {
			warnings = append(warnings, err)
		} else {
			result.GenAI = &document
		}
	}
	litellm, liteLLMErr := fetchLiteLLM(ctx)
	if liteLLMErr != nil {
		warnings = append(warnings, fmt.Errorf(
			"fetching LiteLLM catalog (preserving stored rates): %w",
			liteLLMErr,
		))
		return result, errors.Join(warnings...)
	}
	result.LiteLLM = litellm
	openrouter, openRouterErr := fetchOpenRouter(ctx)
	if openRouterErr != nil {
		warnings = append(warnings, fmt.Errorf(
			"fetching openrouter catalog (storing litellm rates only): %w",
			openRouterErr,
		))
	} else {
		result.OpenRouter = openrouter
	}
	orcarouter, orcaRouterErr := fetchOrcaRouter(ctx)
	if orcaRouterErr != nil {
		warnings = append(warnings, fmt.Errorf(
			"fetching orcarouter catalog (storing litellm rates only): %w",
			orcaRouterErr,
		))
	} else {
		result.OrcaRouter = orcarouter
	}
	return result, errors.Join(warnings...)
}

// Reconcile merges the catalog over a stored table and returns the rows to
// store, the online-marketplace ownership list to record, and the stored
// patterns to delete. stored lists every stored model pattern; previous lists
// the patterns an earlier refresh took from the online marketplace sources.
//
// An online-marketplace row (OpenRouter or OrcaRouter) is dropped when the
// LiteLLM catalog or a stored row from another source (LiteLLM, embedded,
// supplemental) already lists a model with the same canonical name under any
// spelling or provider prefix, so adding a marketplace source never changes a
// lookup that already resolved: two provider-qualified spellings of one model
// (LiteLLM's openrouter/openai/gpt-x against OpenRouter's openai/gpt-x) would
// otherwise tie in the resolver and leave a bare session model name unpriced.
// The cost is that a session naming the marketplace id exactly stays unpriced
// in that case, as it did before. Rows are copied whole; a zero rate is a valid
// price for free models, so fields are never backfilled across sources.
//
// OrcaRouter is reconciled after OpenRouter and outranks nothing: an OrcaRouter
// row is dropped when OpenRouter already lists the same canonical model, so the
// two marketplace sources never store duplicate spellings of one model. Both
// sources feed the same ownership list.
//
// Of the previously stored marketplace rows, those the merged catalog or a
// stored row from another source now covers under a different spelling are
// retired (they would tie with the replacement), those the catalog lists
// exactly follow the catalog's ownership, and rows the marketplace merely
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
	prices = c.LiteLLM
	coveringWithOpenRouter := append(slices.Clone(covering),
		reconcileMarketplace(c.OpenRouter, covering, &prices, &owned)...)
	reconcileMarketplace(c.OrcaRouter, coveringWithOpenRouter, &prices, &owned)

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

// reconcileMarketplace appends the source rows that no covering pattern
// shadows to prices and owned, and returns the patterns it kept so a later
// marketplace source can be outranked by them.
func reconcileMarketplace(
	rows []ModelPricing,
	covering []string,
	prices *[]ModelPricing,
	owned *[]string,
) []string {
	candidates := make([]string, len(rows))
	for i, p := range rows {
		candidates[i] = p.ModelPattern
	}
	dropped := CoveredPatterns(covering, candidates)
	var kept []string
	for _, p := range rows {
		if slices.Contains(dropped, p.ModelPattern) {
			continue
		}
		*prices = append(*prices, p)
		*owned = append(*owned, p.ModelPattern)
		kept = append(kept, p.ModelPattern)
	}
	return kept
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
