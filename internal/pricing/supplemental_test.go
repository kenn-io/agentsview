package pricing

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSupplementalPricing_KimiInternalAliases pins the curated alias
// set: internal model names reported by Kimi Work (daimon runtime) and
// the Kimi CLI, aliased to moonshot/kimi-k2.6 list pricing because the
// names never appear in the LiteLLM catalog.
func TestSupplementalPricing_KimiInternalAliases(t *testing.T) {
	byPattern := make(map[string]ModelPricing)
	for _, p := range SupplementalPricing() {
		byPattern[p.ModelPattern] = p
	}

	want := ModelPricing{
		InputPerMTok:         0.95,
		OutputPerMTok:        4.0,
		CacheCreationPerMTok: 0,
		CacheReadPerMTok:     0.16,
	}
	for _, model := range []string{
		"daimon-kimi-code",
		"daimon-kimi-messages",
		"k3-agent",
		"kimi-for-coding",
		"k3",
	} {
		got, ok := byPattern[model]
		require.True(t, ok, "supplemental alias %q missing", model)
		want.ModelPattern = model
		assert.Equal(t, want, got, "supplemental alias %q rates", model)
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
