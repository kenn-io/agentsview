package rawparity

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestPlanUsageSourceOwnedRoot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		targets []string
		missing bool
		matches map[string]string
		equal   bool
	}{
		{"matched", []string{"raw-root"}, false, map[string]string{"raw-root": "root"}, true},
		{"duplicate_proof", []string{"raw-root", "raw-root"}, false, map[string]string{"raw-root": "root"}, true},
		{"different", []string{"raw-other"}, false, map[string]string{"raw-other": "other"}, false},
		{"unmapped", []string{"root"}, false, nil, false},
		{"missing", nil, true, nil, false},
		{"plural", []string{"raw-root", "raw-other"}, false, map[string]string{"raw-root": "root", "raw-other": "other"}, false},
		{"no_recorded_edge", nil, false, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := planUsageSample(), planUsageSample()
			var links []Link
			if tc.name != "no_recorded_edge" {
				links = []Link{{Kind: "usage_root", Ordinal: -1, CallIndex: -1, EventIndex: -1, Targets: tc.targets, Missing: tc.missing}}
			}
			got, err := CompareResolved(a, b, links, tc.matches)
			require.NoError(t, err)
			if tc.equal {
				assert.Empty(t, got.Differing)
			} else {
				assert.Equal(t, []string{"usage"}, got.Differing)
			}
			assert.Equal(t, planUsageSample(), a)
			assert.Equal(t, planUsageSample(), b)
		})
	}
}

// Usage roots belong to the session's plan row, independent of message indexes.
func TestPlanUsageRootCoordinatesProperty(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		index := hegel.Draw(ht, hegel.Integers(math.MinInt32, math.MaxInt32))
		links := []Link{
			{Kind: "usage_root", Targets: []string{"raw-root"}},
			{Kind: "usage_root", Ordinal: index, CallIndex: index, EventIndex: index, Targets: []string{"raw-root"}},
		}
		a, b := planUsageSample(), planUsageSample()
		_, err := CompareResolved(a, b, links, map[string]string{"raw-root": "root"})
		require.ErrorContains(ht, err, "duplicate")
		assert.Equal(ht, planUsageSample(), a)
		assert.Equal(ht, planUsageSample(), b)
	}, hegel.WithTestCases(1000))
}
