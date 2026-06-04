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
		"claude-opus-4-7": 5,
		"claude-opus-4.6": 99,
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

	_, ok = Resolve(rates, "gpt-5.5")
	assert.False(t, ok, "unknown model stays unresolved")
}
