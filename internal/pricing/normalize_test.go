package pricing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4.7":   "claude-opus-4-7",
		"claude-sonnet-4.6": "claude-sonnet-4-6",
		"claude-haiku-4.5":  "claude-haiku-4-5",
		"claude-opus-4-8":   "claude-opus-4-8",
		"gpt-5.5":           "gpt-5-5",
	}
	for in, want := range cases {
		assert.Equal(t, want, NormalizeModelName(in), "input %q", in)
	}
}

func TestResolve(t *testing.T) {
	rates := map[string]int{
		"claude-opus-4-7":   5,
		"claude-opus-4.6":   99,
		"gemini-3.5":        10,
		"gemini-3.5-flash":  20,
		"openai/gpt-5.5":    30,
		"google/gemini-2.5": 40,
	}

	got, ok := Resolve(rates, "claude-opus-4.7")
	require.True(t, ok, "dotted model should resolve via normalized key")
	assert.Equal(t, 5, got)

	got, ok = Resolve(rates, "claude-opus-4-7")
	require.True(t, ok, "already-dashed model should resolve exactly")
	assert.Equal(t, 5, got)

	got, ok = Resolve(rates, "claude-opus-4.6")
	require.True(t, ok)
	assert.Equal(t, 99, got, "exact match must win over normalized fallback")

	// 1. Case-insensitivity
	got, ok = Resolve(rates, "CLAUDE-OPUS-4-7")
	require.True(t, ok)
	assert.Equal(t, 5, got)

	// 2. Substring match on canonicalized name
	got, ok = Resolve(rates, "Gemini 3.5 Flash (Medium)")
	require.True(t, ok)
	assert.Equal(t, 20, got, "should match gemini-3.5-flash via substring canonical match")

	got, ok = Resolve(rates, "Gemini 3.5 Flash (Low)")
	require.True(t, ok)
	assert.Equal(t, 20, got, "should match gemini-3.5-flash via substring canonical match")

	// 3. Specificity matching (gemini-3.5-flash (len 13) vs gemini-3.5 (len 8))
	got, ok = Resolve(rates, "Gemini 3.5 Flash")
	require.True(t, ok)
	assert.Equal(t, 20, got, "longer canonical match should win")

	// 4. Provider-prefix handling
	got, ok = Resolve(rates, "gpt-5.5")
	require.True(t, ok, "should resolve without provider prefix if mapped key has it")
	assert.Equal(t, 30, got)

	got, ok = Resolve(rates, "google/gemini-2.5")
	require.True(t, ok)
	assert.Equal(t, 40, got)

	got, ok = Resolve(rates, "gemini-2.5")
	require.True(t, ok, "should resolve without prefix if map has prefix")
	assert.Equal(t, 40, got)

	// 5. Bracketed long-context tag strips to the base model
	got, ok = Resolve(rates, "claude-opus-4.6[1m]")
	require.True(t, ok, "bracketed decoration should strip to base model")
	assert.Equal(t, 99, got)

	// 6. Trailing release date strips to the base model
	got, ok = Resolve(rates, "claude-opus-4-7-20260101")
	require.True(t, ok, "release-date suffix should strip to base model")
	assert.Equal(t, 5, got)

	_, ok = Resolve(rates, "unknown-model")
	assert.False(t, ok, "unknown model stays unresolved")
}

func TestResolveProviderPrefixes(t *testing.T) {
	rates := map[string]int{
		"openrouter/owl-alpha": 7,
		"gpt-5.5":              30,
	}

	_, ok := Resolve(rates, "other/owl-alpha")
	assert.False(t, ok,
		"provider-qualified model must not take another provider's pricing")

	got, ok := Resolve(rates, "owl-alpha")
	require.True(t, ok, "unqualified model may match a qualified key")
	assert.Equal(t, 7, got)

	got, ok = Resolve(rates, "openai/gpt-5.5")
	require.True(t, ok, "qualified model may match an unqualified key")
	assert.Equal(t, 30, got)
}

func TestResolveCanonicalDeterminism(t *testing.T) {
	// Two providers share one canonical name: ambiguous, unpriced.
	rates := map[string]int{
		"openai/foo": 1,
		"other/foo":  2,
	}
	_, ok := Resolve(rates, "Foo")
	assert.False(t, ok,
		"multiple provider keys for one canonical name are ambiguous")

	// A provider-qualified model resolves its own provider's key.
	got, ok := Resolve(rates, "openai/foo[1m]")
	require.True(t, ok, "matching provider key should resolve")
	assert.Equal(t, 1, got)

	// An unqualified key beats provider-qualified keys.
	withBase := map[string]int{
		"openai/bar": 5,
		"other/bar":  6,
		"bar":        7,
	}
	got, ok = Resolve(withBase, "Bar[1m]")
	require.True(t, ok, "unqualified key should disambiguate")
	assert.Equal(t, 7, got)

	// Distinct keys tied within one rank stay ambiguous.
	dupes := map[string]int{
		"fo.o": 1,
		"fo-o": 2,
	}
	_, ok = Resolve(dupes, "Foo")
	assert.False(t, ok,
		"duplicate unqualified canonical keys are ambiguous")
}

// TestResolveOverlappingQualifiedRows pins the resolver behavior that
// MergeCatalog exists to protect: a bare session model name resolves
// against a lone provider-qualified key, but two qualified spellings of
// one model (LiteLLM's minimax/MiniMax-M3 against OpenRouter's
// minimax/minimax-m3) tie and stay unresolved.
func TestResolveOverlappingQualifiedRows(t *testing.T) {
	single := map[string]int{"minimax/MiniMax-M3": 2}
	got, ok := Resolve(single, "MiniMax-M3")
	require.True(t, ok, "bare name resolves against a lone qualified key")
	assert.Equal(t, 2, got)

	colliding := map[string]int{
		"minimax/MiniMax-M3": 2,
		"minimax/minimax-m3": 9,
	}
	_, ok = Resolve(colliding, "MiniMax-M3")
	assert.False(t, ok,
		"same-provider spellings of one model tie and stay ambiguous")

	// Each spelling still resolves when addressed exactly, so dropping
	// the lower-priority row never orphans a real model id.
	got, ok = Resolve(colliding, "minimax/minimax-m3")
	require.True(t, ok, "exact key still resolves")
	assert.Equal(t, 9, got)
}

func TestResolveEffortTierSuffixFallback(t *testing.T) {
	rates := map[string]int{
		"claude-opus-4-6":         1,
		"claude-opus-4-8":         2,
		"claude-opus-5":           3,
		"gpt-5.6-luna":            4,
		"grok-4":                  5,
		"grok-9":                  6,
		"mistral":                 7,
		"mistral-medium":          8, // catalogued size variant, distinct price
		"grok-4-fast":             9, // catalogued speed variant, distinct price
		"openrouter/o3-mini-high": 10,
	}

	// Devin effort/speed tiers strip to the base model's rate.
	for name, want := range map[string]int{
		"claude-opus-4-6-thinking":    1,
		"claude-opus-4-8-high":        2,
		"claude-opus-4-8-medium-fast": 2,
		"claude-opus-4-8-max-fast":    2,
		"claude-opus-5-xhigh":         3,
		"gpt-5-6-luna-medium":         4,
	} {
		got, ok := Resolve(rates, name)
		require.True(t, ok, "effort-tier model %q should resolve to its base", name)
		assert.Equal(t, want, got, "model %q", name)
	}

	// A catalogued name that genuinely ends in an effort/size word is matched
	// exactly first and never reduced to a different base rate.
	got, ok := Resolve(rates, "mistral-medium")
	require.True(t, ok)
	assert.Equal(t, 8, got, "mistral-medium must keep its own rate, not mistral's")

	got, ok = Resolve(rates, "grok-4-fast")
	require.True(t, ok)
	assert.Equal(t, 9, got, "grok-4-fast must keep its own rate, not grok-4's")

	got, ok = Resolve(rates, "o3-mini-high")
	require.True(t, ok)
	assert.Equal(t, 10, got, "catalogued -high model matches before stripping")

	// A bare "-fast" (no effort tier before it) is a distinct SKU and must not
	// be reduced to its base even when the base is priced.
	_, ok = Resolve(rates, "grok-9-fast")
	assert.False(t, ok, "bare -fast must not strip to the base model")

	// An effort tier on an unknown base stays unresolved.
	_, ok = Resolve(rates, "unknown-model-high")
	assert.False(t, ok, "effort tier cannot conjure a price for an unknown base")
}

func TestEffortTierBaseModel(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-6-thinking":    "claude-opus-4-6",
		"claude-opus-4-8-high":        "claude-opus-4-8",
		"claude-opus-4-8-medium-fast": "claude-opus-4-8",
		"claude-opus-4-8-max-fast":    "claude-opus-4-8",
		"claude-opus-5-xhigh":         "claude-opus-5",
		"gpt-5-6-luna-medium":         "gpt-5-6-luna",
		"grok-4-fast":                 "grok-4-fast", // bare speed tier preserved
		"claude-sonnet-4-6":           "claude-sonnet-4-6",
		"swe-1-6":                     "swe-1-6",
		"-high":                       "-high", // leading dash is not a separator
	}
	for in, want := range cases {
		assert.Equal(t, want, EffortTierBaseModel(in), "input %q", in)
	}
}

func TestResolveRejectsArbitrarySubstrings(t *testing.T) {
	rates := map[string]int{
		"openai/gpt-5.5":   30,
		"gemini-3.5-flash": 20,
	}

	_, ok := Resolve(rates, "gpt-5.5-codex")
	assert.False(t, ok,
		"distinct variant must stay unpriced, not take base pricing")

	_, ok = Resolve(rates, "wrapped-gemini-3.5-flash-pro")
	assert.False(t, ok,
		"key inside an unrelated longer name must not match")
}
