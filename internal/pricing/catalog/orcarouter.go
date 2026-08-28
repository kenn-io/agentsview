package catalog

import (
	"context"
	"net/http"
	"time"
)

// orcarouterURL is OrcaRouter's public model list. Like OpenRouter's catalog
// it needs no auth and returns every proxied model with its per-token
// prompt/completion price in the same envelope.
const orcarouterURL = "https://api.orcarouter.ai/v1/models"

// FetchOrcaRouterPricingContext downloads the OrcaRouter model catalog and
// binds the request lifetime to ctx.
func FetchOrcaRouterPricingContext(
	ctx context.Context,
) ([]ModelPricing, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return fetchPricingCatalog(ctx, client, orcarouterURL)
}
