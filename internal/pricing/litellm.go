package pricing

import (
	"context"
	"slices"
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

// OpenRouterShadowedMetaKey identifies the sentinel model_pricing row that
// stores the patterns the most recent OpenRouter refresh suppressed
// because a higher-priority source already covered them. Unlike the alias
// list, which downstream stores diff against their previous copy to find
// retired aliases, this list is absolute: every pattern in it must not
// exist, so a push target can retire it without tracking history.
const OpenRouterShadowedMetaKey = "_openrouter_shadowed"

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

// SuppressShadowedOpenRouterRows drops OpenRouter rows that a
// higher-priority source already covers and reports the dropped
// patterns, so a caller can also retire copies persisted by an earlier
// refresh. Coverage is compared on canonicalized names — the same
// normalization the resolver uses.
//
// A row is shadowed when an earlier row already occupies the rank the
// row would resolve at, so the two rows are indistinguishable:
//
//   - An unqualified OpenRouter row (an alias emitted alongside a
//     qualified id, or an id that carries no provider prefix at all) is
//     shadowed by any earlier row with the same canonical name.
//     Unqualified keys outrank provider-qualified ones during
//     resolution, so an unsuppressed bare row would let an OpenRouter
//     rate shadow an earlier source's qualified row (LiteLLM's
//     minimax/MiniMax-M3) for bare session model names.
//
//   - A provider-qualified OpenRouter row is shadowed by an earlier row
//     with the same canonical provider and name — LiteLLM's
//     minimax/MiniMax-M3 against OpenRouter's minimax/minimax-m3. Both
//     spellings canonicalize alike and rank equally, so a bare
//     MiniMax-M3 lookup would find two tied keys, be rejected as
//     ambiguous, and price at zero. Keeping one row per canonical model
//     preserves that lookup; the dropped id still resolves
//     case-insensitively to the earlier source's row, which is the rate
//     precedence already demands.
//
// Rows that canonically collide across different providers
// (openai/gpt-x against azure/gpt-x) are kept. They are distinct vendor
// listings with independently valid rates, they never tie with each
// other for a qualified lookup, and dropping one would leave its own
// qualified id unpriced, because canonical resolution never matches a
// key whose provider conflicts with the model's.
func SuppressShadowedOpenRouterRows(
	earlier [][]ModelPricing, openrouter []ModelPricing,
) (kept []ModelPricing, dropped []string) {
	// coveredNames is keyed by canonical model name alone; coveredIDs
	// additionally keys on the canonical provider prefix.
	coveredNames := make(map[string]struct{})
	coveredIDs := make(map[[2]string]struct{})
	for _, prices := range earlier {
		for _, p := range prices {
			c := canonicalize(p.ModelPattern)
			if c == "" {
				continue
			}
			coveredNames[c] = struct{}{}
			coveredIDs[[2]string{canonicalProvider(p.ModelPattern), c}] =
				struct{}{}
		}
	}
	if len(coveredNames) == 0 {
		return openrouter, nil
	}
	kept = make([]ModelPricing, 0, len(openrouter))
	for _, p := range openrouter {
		c := canonicalize(p.ModelPattern)
		if c == "" {
			kept = append(kept, p)
			continue
		}
		var shadowed bool
		if provider := canonicalProvider(p.ModelPattern); provider == "" {
			_, shadowed = coveredNames[c]
		} else {
			_, shadowed = coveredIDs[[2]string{provider, c}]
		}
		if shadowed {
			dropped = append(dropped, p.ModelPattern)
			continue
		}
		kept = append(kept, p)
	}
	sort.Strings(dropped)
	return kept, slices.Compact(dropped)
}

// DropOpenRouterAliases returns the OpenRouter rows with every alias row
// removed, keeping only qualified rows and inherently bare models. Used
// when a higher-priority source failed to fetch: its persisted rows may
// still be authoritative, but they are not visible to
// SuppressShadowedOpenRouterRows, so alias emission cannot be
// validated and is skipped for the whole refresh.
func DropOpenRouterAliases(prices []ModelPricing) []ModelPricing {
	aliases := OpenRouterAliasPatterns(prices)
	if len(aliases) == 0 {
		return prices
	}
	aliasSet := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		aliasSet[alias] = struct{}{}
	}
	kept := make([]ModelPricing, 0, len(prices))
	for _, p := range prices {
		if _, isAlias := aliasSet[p.ModelPattern]; isAlias {
			continue
		}
		kept = append(kept, p)
	}
	return kept
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
// single map keyed by ModelPattern. The first source to report
// a pattern owns that row entirely; later sources only
// contribute patterns no earlier source declared. Field-level
// backfill is deliberately avoided: a zero rate is valid
// pricing (free models), and ModelPricing cannot distinguish
// an explicit zero from an absent field, so filling "missing"
// fields from a lower-priority catalog would silently turn a
// free model into a paid one. This gives LiteLLM priority over
// OpenRouter for models both catalogs cover, while still
// letting OpenRouter contribute models LiteLLM does not list.
func MergePricing(sources [][]ModelPricing) map[string]ModelPricing {
	out := make(map[string]ModelPricing)
	for _, prices := range sources {
		for _, p := range prices {
			if _, ok := out[p.ModelPattern]; !ok {
				out[p.ModelPattern] = p
			}
		}
	}
	return out
}
