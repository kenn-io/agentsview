package rawparity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
