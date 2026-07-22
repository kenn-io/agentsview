package pricing

import "slices"

// supplementalVersion identifies the curated supplemental alias set.
// It is folded into SeedVersion so the version-gated pricing seed
// re-runs on existing databases when this list changes.
const supplementalVersion = "1"

// supplementalPricing lists curated pricing aliases for internal model
// names that never appear in the upstream LiteLLM catalog (neither in
// the embedded snapshot nor in the fetched table), so sessions priced
// through them would otherwise report $0.
//
// The rates below are ESTIMATES aliased to the moonshot/kimi-k2.6 list
// pricing in the embedded snapshot (input 0.95, output 4.0, cache
// creation 0, cache read 0.16 per MTok): Kimi Work (the kimi-desktop
// daimon runtime) reports daimon-kimi-code, daimon-kimi-messages, and
// k3-agent, and the Kimi CLI reports kimi-for-coding and k3, none of
// which carry public rate cards. The seed upserts them like any other
// fallback row, so a later LiteLLM refresh still overwrites them if
// upstream ever lists the real models.
var supplementalPricing = []ModelPricing{
	{
		ModelPattern:         "daimon-kimi-code",
		InputPerMTok:         0.95,
		OutputPerMTok:        4.0,
		CacheCreationPerMTok: 0,
		CacheReadPerMTok:     0.16,
	},
	{
		ModelPattern:         "daimon-kimi-messages",
		InputPerMTok:         0.95,
		OutputPerMTok:        4.0,
		CacheCreationPerMTok: 0,
		CacheReadPerMTok:     0.16,
	},
	{
		ModelPattern:         "k3-agent",
		InputPerMTok:         0.95,
		OutputPerMTok:        4.0,
		CacheCreationPerMTok: 0,
		CacheReadPerMTok:     0.16,
	},
	{
		ModelPattern:         "kimi-for-coding",
		InputPerMTok:         0.95,
		OutputPerMTok:        4.0,
		CacheCreationPerMTok: 0,
		CacheReadPerMTok:     0.16,
	},
	{
		ModelPattern:         "k3",
		InputPerMTok:         0.95,
		OutputPerMTok:        4.0,
		CacheCreationPerMTok: 0,
		CacheReadPerMTok:     0.16,
	},
}

// SupplementalPricing returns the curated alias set, copied for caller
// safety. It is already folded into FallbackPricing; this accessor
// exists for tests and diagnostics that need the supplementals alone.
func SupplementalPricing() []ModelPricing {
	return slices.Clone(supplementalPricing)
}
