package pricing

import "testing"

func TestBillingPolicyExactMatch(t *testing.T) {
	if p, ok := BillingPolicyFor(PositAssistantProviderID); !ok || p.Numerator != 11 || p.Denominator != 10 || p.Version == "" {
		t.Fatalf("unexpected policy: %#v, %v", p, ok)
	}
	for _, providerID := range []string{"", "POSITAI", "posit_assistant", "positron", "posit-assistant", "posit-assistant-worker", "claude", "copilot"} {
		if _, ok := BillingPolicyFor(providerID); ok {
			t.Errorf("matched %q", providerID)
		}
	}
	t.Logf("policy boundary: positai=%d/%d=1.1; non-exact providers rejected", 11, 10)
}
