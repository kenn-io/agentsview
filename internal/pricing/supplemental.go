package pricing

import (
	"slices"
	"strings"
	"time"
)

// supplementalVersion identifies the curated supplemental alias set.
// It is folded into SeedVersion so the version-gated pricing seed
// re-runs on existing databases when this list changes. Version 2
// replaced the flat K2.6 rates on the date-ambiguous aliases
// (kimi-for-coding, daimon-kimi-code, daimon-kimi-messages) with
// date-based pricing: those names left the static set (the seed
// deletes their stale rows) and k3/k3-agent moved from K2.6 to K3
// rates.
const supplementalVersion = "2"

// Canonical pricing models the date-ambiguous aliases resolve to.
// KimiK26Canonical exists in the embedded LiteLLM snapshot;
// KimiK3Canonical is seeded by the supplemental set below because the
// LiteLLM catalog does not list Kimi K3.
const (
	KimiK26Canonical = "moonshot/kimi-k2.6"
	KimiK3Canonical  = "kimi-k3"
)

// KimiModelEraCutoff is the UTC instant at which the date-ambiguous
// Kimi aliases switched from K2.6 to K3. Rows strictly before the
// cutoff price at K2.6 rates; rows at or after it price at K3 rates.
// The cutoff is compared as an absolute instant, so offset-bearing
// timestamps (e.g. 2026-07-18T20:00:00-04:00) land on the correct
// side.
var KimiModelEraCutoff = time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)

// kimiAmbiguousDateAliases are internal model names that carried K2.6
// before KimiModelEraCutoff and K3 after it: kimi-for-coding (Kimi
// CLI) and, by intent, daimon-kimi-code / daimon-kimi-messages (Kimi
// Work desktop daimon runtime), which ride the same backend model
// rollouts. No single flat rate can describe them, so they are priced
// per row by timestamp via CanonicalModelForDate and deliberately do
// NOT appear in the static supplemental set (a static row would shadow
// the date-based path on exact match).
var kimiAmbiguousDateAliases = []string{
	"daimon-kimi-code",
	"daimon-kimi-messages",
	"kimi-for-coding",
}

// DateAliasedModels returns the sorted unqualified date-ambiguous
// alias names, copied for caller safety. Used by the pricing seed
// (deleting stale flat rows) and by SQL backends that must compute the
// canonical price model inside an aggregate query.
func DateAliasedModels() []string {
	return slices.Clone(kimiAmbiguousDateAliases)
}

// isDateAliasedModel reports whether model is one of the
// date-ambiguous aliases. A provider prefix is ignored
// ("kimi-code/kimi-for-coding" is the same alias), matching how
// ResolveMatch strips provider prefixes in its canonical fallback.
func isDateAliasedModel(model string) bool {
	name := model
	if idx := strings.LastIndex(name, "/"); idx != -1 {
		name = name[idx+1:]
	}
	return slices.Contains(kimiAmbiguousDateAliases, name)
}

// CanonicalModelForDate maps a date-ambiguous alias to the canonical
// pricing model for timestamp t: KimiK26Canonical before
// KimiModelEraCutoff, KimiK3Canonical at or after it. It returns ""
// for any other model. A zero time (missing or unparseable row
// timestamp) falls back to the post-cutoff K3 model so aggregated
// paths without a usable timestamp price at current rates rather than
// silently using the retired K2.6 era.
func CanonicalModelForDate(model string, t time.Time) string {
	if !isDateAliasedModel(model) {
		return ""
	}
	if t.IsZero() || !t.Before(KimiModelEraCutoff) {
		return KimiK3Canonical
	}
	return KimiK26Canonical
}

// CanonicalModelForTimestamp is CanonicalModelForDate for rows whose
// timestamp is stored as an RFC3339/RFC3339Nano string. An empty or
// unparseable timestamp falls back to the post-cutoff K3 model, same
// as a zero time.
func CanonicalModelForTimestamp(model, ts string) string {
	if !isDateAliasedModel(model) {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return KimiK3Canonical
	}
	return CanonicalModelForDate(model, t)
}

// supplementalPricing lists curated pricing aliases for internal model
// names that never appear in the upstream LiteLLM catalog (neither in
// the embedded snapshot nor in the fetched table), so sessions priced
// through them would otherwise report $0.
//
// The rates below are ESTIMATES at the Kimi K3 list pricing (input
// 3.00, output 15.00, cache creation 0, cache read 0.30 per MTok):
// the Kimi CLI reports k3 and kimi-k3 (also seen as moonshot/kimi-k3),
// and Kimi Work (the kimi-desktop daimon runtime) reports k3-agent,
// none of which carry public rate cards. K3 itself is absent from the
// LiteLLM catalog, so kimi-k3 and moonshot/kimi-k3 are seeded here too
// — CanonicalModelForDate maps the date-ambiguous aliases onto these
// canonical rows. The seed upserts them like any other fallback row,
// so a later LiteLLM refresh still overwrites them if upstream ever
// lists the real models.
var supplementalPricing = []ModelPricing{
	{
		ModelPattern:         "k3",
		InputPerMTok:         3.00,
		OutputPerMTok:        15.00,
		CacheCreationPerMTok: 0,
		CacheReadPerMTok:     0.30,
	},
	{
		ModelPattern:         "k3-agent",
		InputPerMTok:         3.00,
		OutputPerMTok:        15.00,
		CacheCreationPerMTok: 0,
		CacheReadPerMTok:     0.30,
	},
	{
		ModelPattern:         "kimi-k3",
		InputPerMTok:         3.00,
		OutputPerMTok:        15.00,
		CacheCreationPerMTok: 0,
		CacheReadPerMTok:     0.30,
	},
	{
		ModelPattern:         "moonshot/kimi-k3",
		InputPerMTok:         3.00,
		OutputPerMTok:        15.00,
		CacheCreationPerMTok: 0,
		CacheReadPerMTok:     0.30,
	},
}

// SupplementalPricing returns the curated alias set, copied for caller
// safety. It is already folded into FallbackPricing; this accessor
// exists for tests and diagnostics that need the supplementals alone.
func SupplementalPricing() []ModelPricing {
	return slices.Clone(supplementalPricing)
}
