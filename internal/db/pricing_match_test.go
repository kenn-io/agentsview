package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupModelRates_DotDashFallback(t *testing.T) {
	pricing := map[string]modelRates{
		"claude-opus-4-7": {input: 5, output: 25},
		"claude-opus-4.6": {input: 99, output: 99},
	}

	rates, ok := lookupModelRates(pricing, "claude-opus-4.7")
	require.True(t, ok, "dotted model should resolve via normalized key")
	assert.Equal(t, 5.0, rates.input)
	assert.Equal(t, 25.0, rates.output)

	dashed, ok := lookupModelRates(pricing, "claude-opus-4-7")
	require.True(t, ok, "already-dashed model should resolve exactly")
	assert.Equal(t, 5.0, dashed.input)

	exact, ok := lookupModelRates(pricing, "claude-opus-4.6")
	require.True(t, ok)
	assert.Equal(t, 99.0, exact.input,
		"exact match must win over normalized fallback")

	_, ok = lookupModelRates(pricing, "gpt-5.5")
	assert.False(t, ok, "unknown model stays unpriced")
}
