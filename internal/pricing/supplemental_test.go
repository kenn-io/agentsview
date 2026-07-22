package pricing

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSupplementalPricing_KimiK3StaticAliases pins the curated static
// alias set: the flat-rate internal names reported by the Kimi CLI
// (k3, kimi-k3, moonshot/kimi-k3) and Kimi Work (k3-agent), all at the
// K3 list pricing. The date-ambiguous aliases (kimi-for-coding,
// daimon-kimi-code, daimon-kimi-messages) must NOT appear here — a
// static exact-match row would shadow CanonicalModelForDate.
func TestSupplementalPricing_KimiK3StaticAliases(t *testing.T) {
	supplementals := SupplementalPricing()
	require.Len(t, supplementals, 4,
		"static supplemental set must hold exactly the four flat K3 rows")

	byPattern := make(map[string]ModelPricing)
	for _, p := range supplementals {
		byPattern[p.ModelPattern] = p
	}

	want := ModelPricing{
		InputPerMTok:         3.00,
		OutputPerMTok:        15.00,
		CacheCreationPerMTok: 0,
		CacheReadPerMTok:     0.30,
	}
	for _, model := range []string{
		"k3",
		"k3-agent",
		"kimi-k3",
		"moonshot/kimi-k3",
	} {
		got, ok := byPattern[model]
		require.True(t, ok, "supplemental alias %q missing", model)
		want.ModelPattern = model
		assert.Equal(t, want, got, "supplemental alias %q rates", model)
	}

	for _, model := range DateAliasedModels() {
		_, ok := byPattern[model]
		assert.False(t, ok,
			"date-ambiguous alias %q must not have a static row", model)
	}
}

func TestDateAliasedModels(t *testing.T) {
	assert.Equal(t, []string{
		"daimon-kimi-code",
		"daimon-kimi-messages",
		"kimi-for-coding",
	}, DateAliasedModels())
}

func TestCanonicalModelForDate(t *testing.T) {
	pre := time.Date(2026, 7, 18, 23, 59, 59, 0, time.UTC)
	at := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	post := time.Date(2026, 7, 19, 0, 0, 1, 0, time.UTC)
	// Same instant as the cutoff, written with a non-UTC offset.
	atOffset := time.Date(2026, 7, 18, 20, 0, 0, 0,
		time.FixedZone("UTC-4", -4*60*60))

	tests := []struct {
		name  string
		model string
		t     time.Time
		want  string
	}{
		{"kimi-for-coding before cutoff", "kimi-for-coding", pre, KimiK26Canonical},
		{"kimi-for-coding at cutoff", "kimi-for-coding", at, KimiK3Canonical},
		{"kimi-for-coding after cutoff", "kimi-for-coding", post, KimiK3Canonical},
		{"kimi-for-coding at cutoff with offset", "kimi-for-coding", atOffset, KimiK3Canonical},
		{"kimi-for-coding zero time falls back to K3", "kimi-for-coding", time.Time{}, KimiK3Canonical},
		{"daimon-kimi-code before cutoff", "daimon-kimi-code", pre, KimiK26Canonical},
		{"daimon-kimi-code after cutoff", "daimon-kimi-code", post, KimiK3Canonical},
		{"daimon-kimi-messages before cutoff", "daimon-kimi-messages", pre, KimiK26Canonical},
		{"daimon-kimi-messages after cutoff", "daimon-kimi-messages", post, KimiK3Canonical},
		{"provider-prefixed alias before cutoff", "kimi-code/kimi-for-coding", pre, KimiK26Canonical},
		{"provider-prefixed alias after cutoff", "kimi-code/kimi-for-coding", post, KimiK3Canonical},
		{"flat k3 alias is not date-ambiguous", "k3", pre, ""},
		{"flat k3-agent alias is not date-ambiguous", "k3-agent", pre, ""},
		{"canonical k2.6 model passes through", KimiK26Canonical, pre, ""},
		{"canonical k3 model passes through", KimiK3Canonical, post, ""},
		{"unknown model passes through", "claude-opus-4-8", pre, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				CanonicalModelForDate(tt.model, tt.t))
		})
	}
}

func TestCanonicalModelForTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		model string
		ts    string
		want  string
	}{
		{"before cutoff", "kimi-for-coding", "2026-07-18T12:00:00Z", KimiK26Canonical},
		{"before cutoff with nanos", "kimi-for-coding", "2026-07-18T23:59:59.999Z", KimiK26Canonical},
		{"at cutoff", "kimi-for-coding", "2026-07-19T00:00:00Z", KimiK3Canonical},
		{"after cutoff", "kimi-for-coding", "2026-07-20T12:00:00Z", KimiK3Canonical},
		{"offset timestamp at cutoff instant", "kimi-for-coding", "2026-07-18T20:00:00-04:00", KimiK3Canonical},
		{"empty timestamp falls back to K3", "kimi-for-coding", "", KimiK3Canonical},
		{"garbage timestamp falls back to K3", "kimi-for-coding", "not-a-time", KimiK3Canonical},
		{"non-alias passes through", "k3", "2026-07-18T12:00:00Z", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				CanonicalModelForTimestamp(tt.model, tt.ts))
		})
	}
}

// TestFallbackPricing_IncludesSupplementals proves the supplementals
// ride the same FallbackPricing set every seed path consumes, so the
// aliases reach the server seed, CLI seed, postgres, duckdb, and the
// in-memory fallback rate maps consistently.
func TestFallbackPricing_IncludesSupplementals(t *testing.T) {
	byPattern := make(map[string]ModelPricing)
	for _, p := range requireEmbeddedFallbackPricing(t) {
		byPattern[p.ModelPattern] = p
	}

	for _, p := range SupplementalPricing() {
		got, ok := byPattern[p.ModelPattern]
		require.True(t, ok,
			"supplemental %q missing from FallbackPricing", p.ModelPattern)
		assert.Equal(t, p, got)
	}
}

// TestFallbackPricing_DateAliasTargetsResolvable proves both canonical
// models the date-ambiguous aliases map onto exist in the fallback
// set, so a mapped lookup always resolves: KimiK26Canonical from the
// embedded snapshot, KimiK3Canonical from the supplemental rows.
func TestFallbackPricing_DateAliasTargetsResolvable(t *testing.T) {
	byPattern := make(map[string]ModelPricing)
	for _, p := range requireEmbeddedFallbackPricing(t) {
		byPattern[p.ModelPattern] = p
	}
	for _, model := range []string{KimiK26Canonical, KimiK3Canonical} {
		_, ok := byPattern[model]
		assert.True(t, ok,
			"date-alias target %q missing from FallbackPricing", model)
	}
}

// TestFallbackPricing_SupplementalsDoNotCollideWithSnapshot guards
// against a supplemental alias shadowing (or being shadowed by) a real
// snapshot entry: duplicate patterns would make seeded rates depend on
// sort order.
func TestFallbackPricing_SupplementalsDoNotCollideWithSnapshot(t *testing.T) {
	snapshot := requireEmbeddedFallbackSnapshot(t)
	snapshotPatterns := make(map[string]struct{}, len(snapshot.Models))
	for _, p := range snapshot.Models {
		snapshotPatterns[p.ModelPattern] = struct{}{}
	}
	for _, p := range SupplementalPricing() {
		_, clash := snapshotPatterns[p.ModelPattern]
		assert.False(t, clash,
			"supplemental %q duplicates a snapshot model; drop the alias", p.ModelPattern)
	}
}

// TestSeedVersion_FoldsInSupplementalVersion pins the seed-gate
// contract: SeedVersion must differ from the bare snapshot version so
// databases seeded by an older binary re-seed when supplementals are
// added, and it must embed the supplemental version so future
// supplemental changes also bump it.
func TestSeedVersion_FoldsInSupplementalVersion(t *testing.T) {
	snapshot := requireEmbeddedFallbackSnapshot(t)
	assert.Equal(t, snapshot.Version, FallbackVersion)
	assert.True(t,
		strings.HasPrefix(SeedVersion, FallbackVersion+"+supplemental-"),
		"SeedVersion %q must be FallbackVersion plus a supplemental suffix",
		SeedVersion)
	assert.NotEqual(t, FallbackVersion, SeedVersion,
		"SeedVersion must differ from FallbackVersion")
}

func TestSupplementalPricing_ReturnsCopy(t *testing.T) {
	first := SupplementalPricing()
	require.NotEmpty(t, first)
	first[0].InputPerMTok = -1
	assert.NotEqual(t, -1.0, SupplementalPricing()[0].InputPerMTok,
		"SupplementalPricing must return an independent copy")
}
