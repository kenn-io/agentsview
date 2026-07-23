package pricing

import (
	"context"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/pricing/catalog"
)

// ModelPricing holds per-model token pricing in cost per
// million tokens. Separate from db.ModelPricing — the CLI
// command converts between the two.
type ModelPricing = catalog.ModelPricing

// FetchLiteLLMPricing downloads the LiteLLM pricing JSON
// and parses it into ModelPricing entries.
func FetchLiteLLMPricing() ([]ModelPricing, error) {
	return catalog.FetchLiteLLMPricing()
}

// FetchLiteLLMPricingContext downloads the LiteLLM pricing JSON and binds the
// request lifetime to ctx.
func FetchLiteLLMPricingContext(
	ctx context.Context,
) ([]ModelPricing, error) {
	return catalog.FetchLiteLLMPricingContext(ctx)
}

// ParseLiteLLMPricing parses the LiteLLM JSON map into
// ModelPricing entries. Per-token costs are converted to
// per-million-token costs. Entries missing both input and
// output cost are skipped.
func ParseLiteLLMPricing(data []byte) ([]ModelPricing, error) {
	return catalog.ParseLiteLLMPricing(data)
}

// FetchOpenRouterPricing downloads the OpenRouter public model
// catalog and converts each text-generation entry into
// ModelPricing. See catalog.FetchOpenRouterPricing for the
// underlying parser and the rationale for dropping non-text
// modalities.
func FetchOpenRouterPricing() ([]ModelPricing, error) {
	return catalog.FetchOpenRouterPricing()
}

// ParseOpenRouterPricing is the byte-level equivalent of
// FetchOpenRouterPricing, exposed for unit tests.
func ParseOpenRouterPricing(data []byte) ([]ModelPricing, error) {
	return catalog.ParseOpenRouterPricing(data)
}

// PricingSource describes one upstream catalog that the
// pricing refresh loop tries to fetch in the background.
// Sources are tried in declaration order, and every successful
// fetch contributes its rows to the merged result. That order
// also defines precedence when catalogs overlap.
type PricingSource struct {
	Name  string
	Fetch func() ([]ModelPricing, error)
}

// OpenRouterAliasesMetaKey identifies the sentinel model_pricing row that
// stores the aliases emitted by the most recent OpenRouter refresh.
const OpenRouterAliasesMetaKey = "_openrouter_aliases"

// DefaultPricingSources returns the built-in pricing sources
// in priority order. LiteLLM covers most public models; the
// OpenRouter catalog frequently lists fork-tuned and private
// model prices that LiteLLM has not yet picked up. The list
// is intentionally short: each entry adds an HTTP request on
// every server start and we want startup latency to stay low.
func DefaultPricingSources() []PricingSource {
	return []PricingSource{
		{Name: "litellm", Fetch: FetchLiteLLMPricing},
		{Name: "openrouter", Fetch: FetchOpenRouterPricing},
	}
}

// OpenRouterAliasPatterns returns the unqualified aliases emitted alongside
// qualified OpenRouter model IDs. A bare pattern is an alias only when the
// same catalog also contains a qualified row with that suffix.
func OpenRouterAliasPatterns(prices []ModelPricing) []string {
	qualifiedSuffixes := make(map[string]struct{})
	for _, price := range prices {
		if i := strings.LastIndex(price.ModelPattern, "/"); i >= 0 &&
			i < len(price.ModelPattern)-1 {
			qualifiedSuffixes[price.ModelPattern[i+1:]] = struct{}{}
		}
	}

	aliases := make([]string, 0)
	for _, price := range prices {
		if strings.Contains(price.ModelPattern, "/") {
			continue
		}
		if _, ok := qualifiedSuffixes[price.ModelPattern]; ok {
			aliases = append(aliases, price.ModelPattern)
		}
	}
	sort.Strings(aliases)
	return aliases
}

// MergePricing combines an ordered slice of per-source ModelPricing slices into a
// single map keyed by ModelPattern. When two sources report
// the same pattern, the first non-zero field wins (so earlier
// sources in the slice take precedence over later ones).
// This gives LiteLLM priority over OpenRouter for models both
// catalogs cover, while still letting OpenRouter fill in
// models that LiteLLM does not list.
func MergePricing(sources [][]ModelPricing) map[string]ModelPricing {
	out := make(map[string]ModelPricing)
	for _, prices := range sources {
		for _, p := range prices {
			existing, ok := out[p.ModelPattern]
			if !ok {
				out[p.ModelPattern] = p
				continue
			}
			merged := existing
			if merged.InputPerMTok == 0 && p.InputPerMTok != 0 {
				merged.InputPerMTok = p.InputPerMTok
			}
			if merged.OutputPerMTok == 0 && p.OutputPerMTok != 0 {
				merged.OutputPerMTok = p.OutputPerMTok
			}
			if merged.CacheCreationPerMTok == 0 && p.CacheCreationPerMTok != 0 {
				merged.CacheCreationPerMTok = p.CacheCreationPerMTok
			}
			if merged.CacheReadPerMTok == 0 && p.CacheReadPerMTok != 0 {
				merged.CacheReadPerMTok = p.CacheReadPerMTok
			}
			out[p.ModelPattern] = merged
		}
	}
	return out
}
