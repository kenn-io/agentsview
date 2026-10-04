package db

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"slices"

	"go.kenn.io/agentsview/internal/export"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

const usageSessionPricingIdentityVersion = "usage-session-pricing-v1"

// usagePricingInput's canonical model already applies timestamp-selected aliases.
type usagePricingInput struct {
	ProviderID, ReportedModel, CanonicalModel string
}

func compareUsagePricingInput(left, right usagePricingInput) int {
	return cmp.Or(
		cmp.Compare(left.ProviderID, right.ProviderID),
		cmp.Compare(left.ReportedModel, right.ReportedModel),
		cmp.Compare(left.CanonicalModel, right.CanonicalModel),
	)
}

func encodeUsagePricingInputs(inputs []usagePricingInput) (string, error) {
	sorted := slices.Clone(inputs)
	slices.SortFunc(sorted, compareUsagePricingInput)
	sorted = slices.Compact(sorted)
	rows := make([][3]string, len(sorted))
	for index, input := range sorted {
		rows[index] = [3]string{
			input.ProviderID, input.ReportedModel, input.CanonicalModel,
		}
	}
	encoded, err := json.Marshal(rows)
	return string(encoded), err
}

func decodeUsagePricingInputs(encoded string) ([]usagePricingInput, error) {
	var rows [][3]string
	if err := json.Unmarshal([]byte(encoded), &rows); err != nil {
		return nil, err
	}
	inputs := make([]usagePricingInput, len(rows))
	for index, row := range rows {
		inputs[index] = usagePricingInput{
			ProviderID: row[0], ReportedModel: row[1], CanonicalModel: row[2],
		}
	}
	return inputs, nil
}

// usagePricingIdentities memoizes per Ensure call; most sessions share few input sets.
type usagePricingIdentities struct {
	resolver     *export.PricingResolver
	fingerprints map[usagePricingInput]string
	byEncoded    map[string]string
}

func newUsagePricingIdentities(
	resolver *export.PricingResolver,
) *usagePricingIdentities {
	return &usagePricingIdentities{
		resolver:     resolver,
		fingerprints: make(map[usagePricingInput]string),
		byEncoded:    make(map[string]string),
	}
}

func (p *usagePricingIdentities) forInputs(
	inputs []usagePricingInput,
) (string, string, error) {
	encoded, err := encodeUsagePricingInputs(inputs)
	if err != nil {
		return "", "", fmt.Errorf("encoding usage pricing inputs: %w", err)
	}
	identity, err := p.forEncoded(encoded)
	return encoded, identity, err
}

func (p *usagePricingIdentities) forEncoded(encoded string) (string, error) {
	if identity, ok := p.byEncoded[encoded]; ok {
		return identity, nil
	}
	inputs, err := decodeUsagePricingInputs(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding usage pricing inputs: %w", err)
	}
	digest := sha256.New()
	writeUsageHashString(digest, usageSessionPricingIdentityVersion)
	writeUsageHashString(digest, pricingpkg.BillingPolicyVersion())
	writeUsageHashInt64(digest, int64(len(inputs)))
	for _, input := range inputs {
		fingerprint, ok := p.fingerprints[input]
		if !ok {
			fingerprint, err = p.resolver.DependencyFingerprint(
				input.ProviderID, input.ReportedModel, input.CanonicalModel)
			if err != nil {
				return "", fmt.Errorf(
					"fingerprinting pricing for model %q: %w", input.ReportedModel, err)
			}
			p.fingerprints[input] = fingerprint
		}
		writeUsageHashString(digest, input.ProviderID)
		writeUsageHashString(digest, input.ReportedModel)
		writeUsageHashString(digest, input.CanonicalModel)
		writeUsageHashString(digest, fingerprint)
	}
	identity := hex.EncodeToString(digest.Sum(nil))
	p.byEncoded[encoded] = identity
	return identity, nil
}
