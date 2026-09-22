package export

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

// BenchmarkPricingResolverResolveAtEmbeddedGenAI measures timestamped
// lookups against the bundled GenAI Prices catalog, covering direct,
// mixed-case, provider-qualified, canonical-alias and unmatched models.
func BenchmarkPricingResolverResolveAtEmbeddedGenAI(b *testing.B) {
	embedded := pricingpkg.EmbeddedGenAIDocument()
	resolver := NewPricingResolver([]EffectivePricingRow{{
		GenAI: embedded.Prices, GenAIVersion: embedded.Version,
		GenAISource: PricingRowSourceEmbedded,
	}})
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		reported  string
		canonical string
		ok        bool
	}{
		{"direct", "claude-sonnet-4-5", "", true},
		{"uppercase", "CLAUDE-SONNET-4-5", "", true},
		{"provider-qualified", "anthropic/claude-sonnet-4-5", "", true},
		{"canonical-alias", "sonnet-alias", "claude-sonnet-4-5", true},
		{"unmatched", "no-such-model-xyz", "", false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var lookup PricingLookup
			for b.Loop() {
				_, lookup = resolver.ResolveAt(tc.reported, tc.canonical, at)
			}
			require.Equal(b, tc.ok, lookup.OK)
		})
	}
}
