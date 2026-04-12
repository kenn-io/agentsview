package pricing

import "testing"

func TestFallbackPricing_Opus46Rates(t *testing.T) {
	prices := FallbackPricing()
	var got *ModelPricing
	for i := range prices {
		if prices[i].ModelPattern == "claude-opus-4-6" {
			got = &prices[i]
			break
		}
	}
	if got == nil {
		t.Fatal("claude-opus-4-6 entry missing from FallbackPricing")
	}

	// Source: https://www.anthropic.com/pricing — Opus tier.
	want := ModelPricing{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         15.0,
		OutputPerMTok:        75.0,
		CacheCreationPerMTok: 18.75,
		CacheReadPerMTok:     1.50,
	}
	if *got != want {
		t.Errorf("claude-opus-4-6 pricing = %+v, want %+v", *got, want)
	}
}
