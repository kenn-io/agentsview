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

	_, ok = Resolve(rates, "unknown-model")
	assert.False(t, ok, "unknown model stays unresolved")
}
