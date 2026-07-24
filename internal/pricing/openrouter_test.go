package pricing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseOpenRouterPricing_TextGenerationOnly verifies the
// parser keeps text->text entries, drops other modalities,
// converts per-token strings to per-million-token floats, and
// maps cache fields correctly.
func TestParseOpenRouterPricing_TextGenerationOnly(t *testing.T) {
	body := []byte(`{
		"data": [
			{
				"id": "MiniMax/MiniMax-M3",
				"architecture": {"modality": "text->text"},
				"pricing": {
					"prompt": "0.000005",
					"completion": "0.000025",
					"input_cache_read": "0.0000005",
					"input_cache_write": "0.00000625"
				}
			},
			{
				"id": "openai/ignored-image",
				"architecture": {"modality": "text->image"},
				"pricing": {"prompt": "0.01", "completion": "0.01"}
			},
			{
				"id": "no-pricing",
				"architecture": {"modality": "text->text"},
				"pricing": {"prompt": "", "completion": ""}
			}
		]
	}`)

	prices, err := ParseOpenRouterPricing(body)
	require.NoError(t, err)
	require.Len(t, prices, 2,
		"one prefixed entry plus its unique bare-suffix alias")

	// Prefixed row keeps the OpenRouter id verbatim.
	got := prices[0]
	assert.Equal(t, "MiniMax/MiniMax-M3", got.ModelPattern)
	assert.InDelta(t, 5.0, got.InputPerMTok, 1e-9, "input")
	assert.InDelta(t, 25.0, got.OutputPerMTok, 1e-9, "output")
	assert.InDelta(t, 0.5, got.CacheReadPerMTok, 1e-9, "cache_read")
	assert.InDelta(t, 6.25, got.CacheCreationPerMTok, 1e-9, "cache_creation")

	// Bare alias so sessions that record just "MiniMax-M3" resolve.
	alias := prices[1]
	assert.Equal(t, "MiniMax-M3", alias.ModelPattern)
	assert.InDelta(t, 5.0, alias.InputPerMTok, 1e-9, "alias input")
	assert.InDelta(t, 25.0, alias.OutputPerMTok, 1e-9, "alias output")
}

// TestParseOpenRouterPricing_AmbiguousBareSuffixSuppressed verifies
// that when two OpenRouter entries share the same bare suffix
// (e.g. two providers publishing "kimi-k2.5"), the unqualified
// alias is NOT emitted for either, so the canonical resolver does
// not see fabricated ambiguity from within OpenRouter itself.
func TestParseOpenRouterPricing_AmbiguousBareSuffixSuppressed(t *testing.T) {
	body := []byte(`{
		"data": [
			{
				"id": "moonshotai/kimi-k2.5",
				"architecture": {"modality": "text->text"},
				"pricing": {"prompt": "0.0000006", "completion": "0.0000025"}
			},
			{
				"id": "baseten/moonshotai/kimi-k2.5",
				"architecture": {"modality": "text->text"},
				"pricing": {"prompt": "0.0000006", "completion": "0.0000025"}
			}
		]
	}`)

	prices, err := ParseOpenRouterPricing(body)
	require.NoError(t, err)
	require.Len(t, prices, 2, "two prefixed rows, no bare alias")
	for _, p := range prices {
		assert.NotEqual(t, "kimi-k2.5", p.ModelPattern,
			"bare alias must be suppressed when suffix is shared")
	}
}

func TestParseOpenRouterPricing_FreeModel(t *testing.T) {
	body := []byte(`{
		"data": [
			{
				"id": "provider/free-model",
				"architecture": {"modality": "text->text"},
				"pricing": {"prompt": "0", "completion": "0"}
			},
			{
				"id": "provider/negative-model",
				"architecture": {"modality": "text->text"},
				"pricing": {"prompt": "-0.1", "completion": "-0.2"}
			},
			{
				"id": "provider/malformed-model",
				"architecture": {"modality": "text->text"},
				"pricing": {"prompt": "1 trailing", "completion": "NaN"}
			}
		]
	}`)

	prices, err := ParseOpenRouterPricing(body)
	require.NoError(t, err)
	require.Len(t, prices, 2,
		"free qualified row plus its bare alias")
	assert.Equal(t, "provider/free-model", prices[0].ModelPattern)
	assert.Zero(t, prices[0].InputPerMTok)
	assert.Zero(t, prices[0].OutputPerMTok)
	assert.Equal(t, "free-model", prices[1].ModelPattern)
	assert.Zero(t, prices[1].InputPerMTok)
	assert.Zero(t, prices[1].OutputPerMTok)
}

// TestMergePricing_FirstSourceOwnsRow verifies that when two
// sources both price the same model_pattern, the first source
// in the ordered slice owns the row entirely — later sources
// never modify it, not even zero-valued fields. Zero is valid
// pricing (free models), so backfilling it from a
// lower-priority catalog would misprice free models. Later
// sources still contribute patterns no earlier source declared.
func TestMergePricing_FirstSourceOwnsRow(t *testing.T) {
	sources := [][]ModelPricing{
		{
			{ModelPattern: "shared", InputPerMTok: 3, OutputPerMTok: 15},
			{ModelPattern: "free-model"},
			{ModelPattern: "only-a", InputPerMTok: 1, OutputPerMTok: 2},
		},
		{
			{ModelPattern: "shared", InputPerMTok: 99, OutputPerMTok: 99,
				CacheCreationPerMTok: 4},
			{ModelPattern: "free-model", InputPerMTok: 5, OutputPerMTok: 6},
			{ModelPattern: "only-b", InputPerMTok: 7, OutputPerMTok: 8},
		},
	}
	merged := MergePricing(sources)

	require.Len(t, merged, 4, "expected 4 distinct patterns")
	assert.Equal(t, 3.0, merged["shared"].InputPerMTok, "a wins input")
	assert.Equal(t, 15.0, merged["shared"].OutputPerMTok, "a wins output")
	assert.Zero(t, merged["shared"].CacheCreationPerMTok,
		"b must not backfill a's zero field")
	assert.Zero(t, merged["free-model"].InputPerMTok,
		"explicit $0 from a survives b's nonzero rate")
	assert.Zero(t, merged["free-model"].OutputPerMTok,
		"explicit $0 from a survives b's nonzero rate")
	assert.Equal(t, 1.0, merged["only-a"].InputPerMTok)
	assert.Equal(t, 7.0, merged["only-b"].InputPerMTok)
}

// TestSuppressShadowedOpenRouterRowsAliases verifies alias rows are
// dropped when a higher-priority source already covers the same
// canonical model name — via a provider-qualified row or an exact
// bare row — while unshadowed aliases survive. An unqualified alias
// would otherwise outrank the earlier source's qualified key at
// resolution time, inverting source precedence for bare session model
// names.
func TestSuppressShadowedOpenRouterRowsAliases(t *testing.T) {
	openrouter := []ModelPricing{
		{ModelPattern: "moonshotai/kimi-k2.5", InputPerMTok: 5},
		{ModelPattern: "kimi-k2.5", InputPerMTok: 5},
		{ModelPattern: "acme/bare-owned", InputPerMTok: 7},
		{ModelPattern: "bare-owned", InputPerMTok: 7},
	}
	litellm := []ModelPricing{
		// Exact bare row owning the bare-owned pattern outright.
		{ModelPattern: "bare-owned", InputPerMTok: 3},
	}

	kept, dropped := SuppressShadowedOpenRouterRows(
		[][]ModelPricing{litellm}, openrouter,
	)

	patterns := make([]string, 0, len(kept))
	for _, p := range kept {
		patterns = append(patterns, p.ModelPattern)
	}
	assert.NotContains(t, patterns, "bare-owned",
		"alias owned outright by litellm must be dropped")
	assert.Contains(t, patterns, "kimi-k2.5",
		"unshadowed alias survives")
	assert.Contains(t, patterns, "acme/bare-owned",
		"qualified row under a different provider survives")
	assert.Equal(t, []string{"bare-owned"}, dropped,
		"dropped patterns are reported for retirement")

	aliases := OpenRouterAliasPatterns(kept)
	assert.Equal(t, []string{"kimi-k2.5"}, aliases,
		"alias metadata must reflect the suppressed set")
}

// TestSuppressShadowedOpenRouterRowsCanonicalCollisions verifies a
// qualified OpenRouter row is dropped when a higher-priority source
// already lists the same model under the same provider with a
// different spelling. Both rows canonicalize alike and rank equally,
// so keeping both would make a bare lookup ambiguous and price the
// model at zero. Collisions across different providers are kept: they
// are distinct vendor listings, and dropping one would leave its own
// qualified id unpriced.
func TestSuppressShadowedOpenRouterRowsCanonicalCollisions(t *testing.T) {
	openrouter := []ModelPricing{
		{ModelPattern: "minimax/minimax-m3", InputPerMTok: 9},
		{ModelPattern: "minimax-m3", InputPerMTok: 9},
		{ModelPattern: "azure/gpt-x", InputPerMTok: 4},
		{ModelPattern: "bare-dupe", InputPerMTok: 6},
		// Bare row with no qualified counterpart, so not an alias.
		{ModelPattern: "solo-model", InputPerMTok: 8},
	}
	litellm := []ModelPricing{
		// Same provider, same canonical name, different spelling.
		{ModelPattern: "minimax/MiniMax-M3", InputPerMTok: 2},
		// Different provider, same canonical name.
		{ModelPattern: "openai/gpt-x", InputPerMTok: 1},
		// Bare row colliding with a bare non-alias OpenRouter row.
		{ModelPattern: "Bare-Dupe", InputPerMTok: 3},
		// Qualified row a bare OpenRouter row would outrank.
		{ModelPattern: "acme/Solo-Model", InputPerMTok: 5},
	}

	kept, dropped := SuppressShadowedOpenRouterRows(
		[][]ModelPricing{litellm}, openrouter,
	)

	patterns := make([]string, 0, len(kept))
	for _, p := range kept {
		patterns = append(patterns, p.ModelPattern)
	}
	assert.NotContains(t, patterns, "minimax/minimax-m3",
		"qualified row duplicating litellm's spelling must be dropped")
	assert.NotContains(t, patterns, "minimax-m3",
		"its alias is shadowed as well")
	assert.NotContains(t, patterns, "bare-dupe",
		"bare row duplicating a litellm bare row must be dropped")
	assert.NotContains(t, patterns, "solo-model",
		"bare row outranking a litellm qualified row must be dropped")
	assert.Contains(t, patterns, "azure/gpt-x",
		"cross-provider canonical collision survives")
	assert.Equal(t, []string{
		"bare-dupe", "minimax-m3", "minimax/minimax-m3", "solo-model",
	}, dropped, "dropped patterns are reported for retirement")

	merged := MergePricing([][]ModelPricing{litellm, kept})
	price, ok := Resolve(merged, "MiniMax-M3")
	require.True(t, ok,
		"bare lookup stays resolvable after suppression")
	assert.Equal(t, 2.0, price.InputPerMTok,
		"higher-priority litellm rate wins")
	price, ok = Resolve(merged, "minimax/minimax-m3")
	require.True(t, ok,
		"the dropped OpenRouter id still resolves")
	assert.Equal(t, 2.0, price.InputPerMTok,
		"and resolves to the higher-priority rate")
}

// TestSuppressShadowedOpenRouterRows_NoEarlierSources verifies
// the OpenRouter slice passes through untouched when no
// higher-priority source responded.
func TestSuppressShadowedOpenRouterRows_NoEarlierSources(t *testing.T) {
	openrouter := []ModelPricing{
		{ModelPattern: "minimax/minimax-m3", InputPerMTok: 9},
		{ModelPattern: "minimax-m3", InputPerMTok: 9},
	}
	kept, dropped := SuppressShadowedOpenRouterRows(nil, openrouter)
	assert.Equal(t, openrouter, kept)
	assert.Empty(t, dropped)
	kept, dropped = SuppressShadowedOpenRouterRows(
		[][]ModelPricing{}, openrouter,
	)
	assert.Equal(t, openrouter, kept)
	assert.Empty(t, dropped)
}

// TestDropOpenRouterAliases verifies every alias row is removed while
// qualified rows and inherently bare (non-alias) models survive.
func TestDropOpenRouterAliases(t *testing.T) {
	prices := []ModelPricing{
		{ModelPattern: "minimax/minimax-m3", InputPerMTok: 9},
		{ModelPattern: "minimax-m3", InputPerMTok: 9},
		{ModelPattern: "acme/other-model", InputPerMTok: 5},
		// Bare pattern with no qualified counterpart is not an alias.
		{ModelPattern: "standalone-model", InputPerMTok: 3},
	}
	kept := DropOpenRouterAliases(prices)
	patterns := make([]string, 0, len(kept))
	for _, p := range kept {
		patterns = append(patterns, p.ModelPattern)
	}
	assert.ElementsMatch(t, []string{
		"minimax/minimax-m3", "acme/other-model", "standalone-model",
	}, patterns)
}

// TestDefaultPricingSources_OrderIsStable makes sure the
// declared priority (LiteLLM first, OpenRouter second) is
// preserved so upstream rate precedence stays deterministic
// after MergePricing.
func TestDefaultPricingSources_OrderIsStable(t *testing.T) {
	srcs := DefaultPricingSources()
	require.Len(t, srcs, 2, "two default sources")
	assert.Equal(t, "litellm", srcs[0].Name)
	assert.Equal(t, "openrouter", srcs[1].Name)
}
