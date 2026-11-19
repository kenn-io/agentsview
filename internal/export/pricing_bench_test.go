package export

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
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

// BenchmarkEffectivePricingDigest measures the digest every usage snapshot
// computes, over a catalog the size of a synced pricing table (about 5,000
// rows, many sharing a model pattern so ties fall through to rate keys).
func BenchmarkEffectivePricingDigest(b *testing.B) {
	updated := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]EffectivePricingRow, 0, 5_000)
	for i := range 5_000 {
		row := EffectivePricingRow{ModelPattern: fmt.Sprintf("provider/model-%d", (i*7919)%1_500)}
		row.Rates.Source = PricingRowSourceCustom
		row.Rates.InputPerMTok = money.Money{Microdollars: int64(i%40) * 25_000}
		row.Rates.OutputPerMTok = money.Money{Microdollars: int64(i%13) * 100_000}
		if i%3 == 0 {
			row.Rates.UpdatedAt = &updated
		}
		if i%10 == 0 {
			row.Rates.Bands = []PricingBand{{AboveInputTokens: 200_000, InputPerMTok: row.Rates.InputPerMTok}}
		}
		rows = append(rows, row)
	}
	b.ReportAllocs()
	for b.Loop() {
		_, err := EffectivePricingDigest(rows)
		require.NoError(b, err)
	}
}
