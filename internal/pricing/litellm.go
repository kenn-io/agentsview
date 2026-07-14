package pricing

import (
	"context"

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
