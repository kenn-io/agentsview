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
	for _, model := range []string{"example-target", "EXAMPLE-TARGET"} {
		b.Run(model, func(b *testing.B) {
			b.ReportAllocs()
			var matched bool
			for range b.N {
				_, matched = prices.Resolve("", model, time.Time{})
			}
			require.True(b, matched)
		})
	}
}
