package rawparity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionAccountingSummariesDiffer(t *testing.T) {
	for _, field := range []string{"total", "peak", "has_total", "has_peak"} {
		t.Run(field, func(t *testing.T) {
			a, b := sample(), sample()
			switch field {
			case "total":
				b.Session.TotalOutputTokens = 17
			case "peak":
				b.Session.PeakContextTokens = 23
			case "has_total":
				b.Session.HasTotalOutputTokens = true
			case "has_peak":
				b.Session.HasPeakContextTokens = true
			}
			got, err := Compare(a, b)
			require.NoError(t, err)
			assert.Equal(t, []string{"usage"}, got.Differing)
		})
	}
}
