package pricing

import (
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/money"
)

// supplementalVersion identifies the curated supplemental alias set.
// It is folded into SeedVersion so the version-gated pricing seed
// re-runs on existing databases when this list changes. Version 2
// replaced the flat K2.6 rates on the date-ambiguous aliases
// (kimi-for-coding, daimon-kimi-code, daimon-kimi-messages) with
// date-based pricing: those names left the static set (the seed
// deletes their stale rows) and k3/k3-agent moved from K2.6 to K3
// rates. Version 3 removed moonshot/kimi-k3 after LiteLLM added it.
const supplementalVersion = "3"

// Canonical pricing models runtime aliases resolve to.
// KimiK26Canonical exists in the embedded LiteLLM snapshot;
// KimiK3Canonical is seeded by the supplemental set below because the
// LiteLLM catalog lists only the provider-qualified Kimi K3 name.
// GPT56LunaCanonical is the catalog id for Codex Luna Reserve (gpt-reserve).
const (
	KimiK26Canonical    = "moonshot/kimi-k2.6"
	KimiK3Canonical     = "kimi-k3"
	GPT56LunaCanonical  = "gpt-5.6-luna"
	GPTReserveModelName = "gpt-reserve"
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

// FixedPricingAlias maps a producer-reported model name onto an
// existing catalog row at query time. The archived model string is
// left unchanged so usage breakdowns keep the reported identity.
type FixedPricingAlias struct {
	Name      string
	Canonical string
}

// fixedPricingAliases are timestamp-independent reported names that
// never appear as catalog keys. Codex writes gpt-reserve for Luna
// Reserve turns; Kimi Work writes k2d6-agent for the K2.6 era. A
// static supplemental rate row for these names would hide later
// catalog updates for the canonical model, including Pydantic
// time-window rates for GPT-5.6 Luna.
var fixedPricingAliases = []FixedPricingAlias{
	{Name: "k2d6-agent", Canonical: KimiK26Canonical},
	{Name: GPTReserveModelName, Canonical: GPT56LunaCanonical},
}

// DateAliasedModels returns the sorted unqualified date-ambiguous
// alias names, copied for caller safety. Used by the pricing seed
// (deleting stale flat rows) and by SQL backends that must compute the
// canonical price model inside an aggregate query.
func DateAliasedModels() []string {
	return slices.Clone(kimiAmbiguousDateAliases)
}

// FixedPricingAliases returns the timestamp-independent reported-name
// mappings, copied for caller safety. DuckDB renders these as SQL CASE
// arms so aggregate usage prices the same canonical models as SQLite.
func FixedPricingAliases() []FixedPricingAlias {
	return slices.Clone(fixedPricingAliases)
}

// KimiK26Aliases returns the sorted unqualified aliases that always resolve
// to the canonical K2.6 pricing model.
func KimiK26Aliases() []string {
	out := make([]string, 0, 1)
	for _, alias := range fixedPricingAliases {
		if alias.Canonical == KimiK26Canonical {
			out = append(out, alias.Name)
		}
	}
	return out
}

// isDateAliasedModel reports whether model is one of the
// date-ambiguous aliases. A provider prefix is ignored
// ("kimi-code/kimi-for-coding" is the same alias), matching how
// ResolveMatch strips provider prefixes in its canonical fallback.
func isDateAliasedModel(model string) bool {
	return slices.Contains(kimiAmbiguousDateAliases, pricingAliasName(model))
}

func fixedCanonicalModel(model string) string {
	name := pricingAliasName(model)
	for _, alias := range fixedPricingAliases {
		if alias.Name == name {
			return alias.Canonical
		}
	}
	return ""
}

func pricingAliasName(model string) string {
	if idx := strings.LastIndex(model, "/"); idx != -1 {
		return model[idx+1:]
	}
	return model
}

// CanonicalModelForDate maps a producer-reported runtime alias to its
// canonical pricing model. Fixed aliases always map to their catalog
// target. Date-ambiguous Kimi aliases map to KimiK26Canonical before
// KimiModelEraCutoff and KimiK3Canonical at or after it. It returns ""
// for any other model. A zero time falls back to the post-cutoff K3
// model only for date-ambiguous aliases.
func CanonicalModelForDate(model string, t time.Time) string {
	if canonical := fixedCanonicalModel(model); canonical != "" {
		return canonical
	}
	if !isDateAliasedModel(model) {
		return ""
	}
	if t.IsZero() || !t.Before(KimiModelEraCutoff) {
		return KimiK3Canonical
	}
	return KimiK26Canonical
}

// CanonicalModelForTimestamp is CanonicalModelForDate for rows whose timestamp
// is stored as RFC3339/RFC3339Nano. Fixed aliases do not require a timestamp;
// an empty or unparseable timestamp falls back to K3 only for date-ambiguous
// aliases.
func CanonicalModelForTimestamp(model, ts string) string {
	if canonical := fixedCanonicalModel(model); canonical != "" {
		return canonical
	}
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
// the Kimi CLI reports k3 and kimi-k3, and Kimi Work (the kimi-desktop
// daimon runtime) reports k3-agent, none of which carry public rate
// cards. LiteLLM now lists moonshot/kimi-k3, so that qualified name
// comes from the snapshot. CanonicalModelForDate maps date-ambiguous
// aliases onto the unqualified kimi-k3 row below. The seed upserts
// these rows like any other fallback row, so a later LiteLLM refresh
// still overwrites them if upstream lists the real models.
var supplementalPricing = []ModelPricing{
	{
		ModelPattern:         "k3",
		InputPerMTok:         money.MustParseDollars("3.00"),
		OutputPerMTok:        money.MustParseDollars("15.00"),
		CacheCreationPerMTok: money.Money{},
		CacheReadPerMTok:     money.MustParseDollars("0.30"),
	},
	{
		ModelPattern:         "k3-agent",
		InputPerMTok:         money.MustParseDollars("3.00"),
		OutputPerMTok:        money.MustParseDollars("15.00"),
		CacheCreationPerMTok: money.Money{},
		CacheReadPerMTok:     money.MustParseDollars("0.30"),
	},
	{
		ModelPattern:         "kimi-k3",
		InputPerMTok:         money.MustParseDollars("3.00"),
		OutputPerMTok:        money.MustParseDollars("15.00"),
		CacheCreationPerMTok: money.Money{},
		CacheReadPerMTok:     money.MustParseDollars("0.30"),
	},
}

// SupplementalPricing returns the curated alias set, copied for caller
// safety. It is already folded into FallbackPricing; this accessor
// exists for tests and diagnostics that need the supplementals alone.
func SupplementalPricing() []ModelPricing {
	return slices.Clone(supplementalPricing)
}
