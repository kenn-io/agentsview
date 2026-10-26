package pricing

// BillingPolicy describes a published adjustment applied to model-derived
// rates for one exact billing service identifier.
type BillingPolicy struct {
	Service     string
	Version     string
	SourceURL   string
	Numerator   int64
	Denominator int64
}

const (
	PositAssistantProviderID = "positai"
	PositAssistantService    = "posit-assistant"
	PositAssistantVersion    = "posit-assistant-v1"
	PositAssistantSourceURL  = "https://docs.posit.co/posit-ai/user/faq/"
)

var positAssistantPolicy = BillingPolicy{
	Service: PositAssistantService, Version: PositAssistantVersion,
	SourceURL: PositAssistantSourceURL, Numerator: 11, Denominator: 10,
}

// BillingPolicyFor returns a policy only for an exact provider match.
func BillingPolicyFor(providerID string) (BillingPolicy, bool) {
	if providerID != PositAssistantProviderID {
		return BillingPolicy{}, false
	}
	return positAssistantPolicy, true
}

// BillingPolicyVersion identifies the policy in internal cache identities.
func BillingPolicyVersion() string { return positAssistantPolicy.Version }

const (
	OpenAIPriorityProviderID  = "openai-priority"
	OpenAIUltrafastProviderID = "openai-ultrafast"
)

// ServiceTierFor returns the catalog tier for an exact billing identity.
func ServiceTierFor(providerID string) (string, bool) {
	switch providerID {
	case OpenAIPriorityProviderID:
		return "priority", true
	case OpenAIUltrafastProviderID:
		return "ultrafast", true
	default:
		return "", false
	}
}
