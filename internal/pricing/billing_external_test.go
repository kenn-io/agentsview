package pricing_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/pricing"
)

func TestPositBillingKeyMatchesProviderID(t *testing.T) {
	if pricing.PositAssistantProviderID != "positai" {
		require.Equal(t, "positai", pricing.PositAssistantProviderID)
	}
}
