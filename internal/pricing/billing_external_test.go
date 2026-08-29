package pricing_test

import (
	"testing"

	"go.kenn.io/agentsview/internal/pricing"
)

func TestPositBillingKeyMatchesProviderID(t *testing.T) {
	if pricing.PositAssistantProviderID != "positai" {
		t.Fatalf("provider ID %q is not the Posit AI provider", pricing.PositAssistantProviderID)
	}
}
