package pricing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBillingPolicyExactMatch(t *testing.T) {
	if p, ok := BillingPolicyFor(PositAssistantProviderID); !ok || p.Numerator != 11 || p.Denominator != 10 || p.Version == "" {
		require.Truef(t, ok, "unexpected policy: %#v", p)
		require.Equal(t, int64(11), p.Numerator)
		require.Equal(t, int64(10), p.Denominator)
		require.NotEmpty(t, p.Version)
	}
	for _, providerID := range []string{"", "POSITAI", "posit_assistant", "positron", "posit-assistant", "posit-assistant-worker", "claude", "copilot"} {
		if _, ok := BillingPolicyFor(providerID); ok {
			assert.Falsef(t, ok, "matched %q", providerID)
		}
	}
	t.Logf("policy boundary: positai=%d/%d=1.1; non-exact providers rejected", 11, 10)
}
