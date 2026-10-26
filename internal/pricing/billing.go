package pricing

import (
	"slices"
	"strings"
)

// BillingPolicy describes a published adjustment applied to model-derived
// rates for one exact billing service identifier.
type BillingPolicy struct {
	Service     string
	Version     string
	SourceURL   string
	Numerator   int64
	Denominator int64

	// ModelRatios limits a policy to these models when set. Bump Version when it changes.
	ModelRatios map[string]BillingRatio
}

// BillingRatio gives the published multiplier for one priced model.
type BillingRatio struct {
	Numerator   int64
	Denominator int64
}

const (
	PositAssistantProviderID = "positai"
	PositAssistantService    = "posit-assistant"
	PositAssistantVersion    = "posit-assistant-v1"
	PositAssistantSourceURL  = "https://docs.posit.co/posit-ai/user/faq/"
)

const (
	OpenAIPriorityProviderID  = "openai-priority"
	OpenAIUltrafastProviderID = "openai-ultrafast"
)

var billingPolicies = map[string]BillingPolicy{
	PositAssistantProviderID: {
		Service: PositAssistantService, Version: PositAssistantVersion,
		SourceURL: PositAssistantSourceURL, Numerator: 11, Denominator: 10,
	},
	OpenAIPriorityProviderID: {
		Service: OpenAIPriorityProviderID, Version: "openai-priority-v1",
		SourceURL: "https://developers.openai.com/api/docs/pricing",
		ModelRatios: map[string]BillingRatio{
			"gpt-6-sol":     {Numerator: 2, Denominator: 1},
			"gpt-6.1-sol":   {Numerator: 2, Denominator: 1},
			"gpt-6-astra":   {Numerator: 2, Denominator: 1},
			"gpt-6-luna":    {Numerator: 2, Denominator: 1},
			"gpt-6.1-terra": {Numerator: 2, Denominator: 1},
			"gpt-5.6":       {Numerator: 2, Denominator: 1},
			"gpt-5.6-sol":   {Numerator: 2, Denominator: 1},
			"gpt-5.6-terra": {Numerator: 2, Denominator: 1},
			"gpt-5.6-luna":  {Numerator: 2, Denominator: 1},
			"gpt-5.4":       {Numerator: 2, Denominator: 1},
			"gpt-5.4-mini":  {Numerator: 2, Denominator: 1},
			"gpt-5.2":       {Numerator: 2, Denominator: 1},
			"gpt-5.1":       {Numerator: 2, Denominator: 1},
			"gpt-5":         {Numerator: 2, Denominator: 1},
			"gpt-5.3-codex": {Numerator: 2, Denominator: 1},
			"gpt-5.5":       {Numerator: 5, Denominator: 2},
			"gpt-5-mini":    {Numerator: 9, Denominator: 5},
		},
	},
	OpenAIUltrafastProviderID: {
		Service: OpenAIUltrafastProviderID, Version: "openai-ultrafast-v1",
		SourceURL: "https://developers.openai.com/api/docs/pricing",
		ModelRatios: map[string]BillingRatio{
			"gpt-6-astra": {Numerator: 6, Denominator: 1},
			"gpt-6.1-sol": {Numerator: 6, Denominator: 1},
		},
	},
}

// BillingPolicyFor returns a policy only for an exact provider match.
func BillingPolicyFor(providerID string) (BillingPolicy, bool) {
	policy, ok := billingPolicies[providerID]
	return policy, ok
}

// BillingPolicyVersion identifies all policies in internal cache identities.
func BillingPolicyVersion() string {
	versions := make([]string, 0, len(billingPolicies))
	for _, policy := range billingPolicies {
		versions = append(versions, policy.Version)
	}
	slices.Sort(versions)
	return strings.Join(versions, ",")
}
