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
