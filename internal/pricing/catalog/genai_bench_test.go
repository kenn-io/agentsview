package catalog

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func BenchmarkGenAIPricesResolveScalarRules(b *testing.B) {
	data := []byte(`[{"id":"example","model_match":{"starts_with":"example-"},"models":[`)
	for i := range 256 {
		data = fmt.Appendf(data, `{"id":"model-%d","match":{"starts_with":"example-model-%d-"},"prices":{"input_mtok":1}},`, i, i)
	}
	data = append(data, `{"id":"target","match":{"equals":"example-target"},"prices":{"input_mtok":1}}]}]`...)
	prices, err := ParseGenAIPrices(data)
	require.NoError(b, err)
	for _, tc := range []struct{ name, model string }{
		{"lowercase", "example-target"},
		{"uppercase", "EXAMPLE-TARGET"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var matched bool
			for b.Loop() {
				_, matched = prices.Resolve("", tc.model, time.Time{})
			}
			require.True(b, matched)
		})
	}
}
