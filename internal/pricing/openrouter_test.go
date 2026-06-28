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
	require.Len(t, prices, 1, "only the text->text priced entry should survive")

	got := prices[0]
	assert.Equal(t, "MiniMax/MiniMax-M3", got.ModelPattern)
	assert.InDelta(t, 5.0, got.InputPerMTok, 1e-9, "input")
	assert.InDelta(t, 25.0, got.OutputPerMTok, 1e-9, "output")
	assert.InDelta(t, 0.5, got.CacheReadPerMTok, 1e-9, "cache_read")
	assert.InDelta(t, 6.25, got.CacheCreationPerMTok, 1e-9, "cache_creation")
}

// TestMergePricing_FirstNonZeroWins verifies that when two
// sources both price the same model_pattern, the first source
// in the iteration order wins for every field, and the second
// source only fills in fields the first left at zero. This
// gives LiteLLM priority over OpenRouter for shared models
// while still letting OpenRouter contribute new rows.
func TestMergePricing_FirstNonZeroWins(t *testing.T) {
	sources := map[string][]ModelPricing{
		"a": {
			{ModelPattern: "shared", InputPerMTok: 3, OutputPerMTok: 15},
			{ModelPattern: "only-a", InputPerMTok: 1, OutputPerMTok: 2},
		},
		"b": {
			{ModelPattern: "shared", InputPerMTok: 99, OutputPerMTok: 99,
				CacheCreationPerMTok: 4},
			{ModelPattern: "only-b", InputPerMTok: 7, OutputPerMTok: 8},
		},
	}
	merged := MergePricing(sources)

	require.Len(t, merged, 3, "expected 3 distinct patterns")
	assert.Equal(t, 3.0, merged["shared"].InputPerMTok, "a wins input")
	assert.Equal(t, 15.0, merged["shared"].OutputPerMTok, "a wins output")
	assert.Equal(t, 4.0, merged["shared"].CacheCreationPerMTok,
		"b fills the zero field")
	assert.Equal(t, 1.0, merged["only-a"].InputPerMTok)
	assert.Equal(t, 7.0, merged["only-b"].InputPerMTok)
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