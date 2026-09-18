package server

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestActivityAutomationFilter locks in the mapping from the activity
// endpoint's automation query value to the AnalyticsFilter class exclusions:
// empty and "all" keep both classes, "interactive" drops automated sessions,
// "automated" drops interactive ones, and any other value is a 400-worthy
// error. The two exclude flags are never both true (that would match nothing),
// and the error case returns the safe no-exclusion default so a typo never
// silently filters the report.
func TestActivityAutomationFilter(t *testing.T) {
	tests := []struct {
		name             string
		automation       string
		wantExcludeAuto  bool
		wantExcludeInter bool
		wantErr          bool
	}{
		{"empty treated as all", "", false, false, false},
		{"all keeps both", "all", false, false, false},
		{"interactive excludes automated", "interactive", true, false, false},
		{"automated excludes interactive", "automated", false, true, false},
		{"unknown value errors", "bogus", false, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			excludeAuto, excludeInter, err := activityAutomationFilter(tc.automation)
			if tc.wantErr {
				require.Error(t, err)
				assert.False(t, excludeAuto, "error must not exclude automated")
				assert.False(t, excludeInter, "error must not exclude interactive")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantExcludeAuto, excludeAuto)
			assert.Equal(t, tc.wantExcludeInter, excludeInter)
			assert.False(t, excludeAuto && excludeInter,
				"the two exclusions are mutually exclusive")
		})
	}
}

func TestResolveActivitySelectionRejectsOversizeTokenInputs(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	_, err := resolveActivitySelection(activitySelectionInput{
		Preset: "day", Date: "2026-08-15", Timezone: "UTC",
		Project: strings.Repeat("p", activityReportFilterValueMaxBytes+1),
	}, now)
	require.Error(t, err)

	_, err = resolveActivitySelection(activitySelectionInput{
		Preset: "day", Date: "2026-08-15", Timezone: "UTC",
		Project: strings.Repeat("p", 800), Agent: strings.Repeat("a", 800),
		Machine: strings.Repeat("m", 800), GitBranch: strings.Repeat("b", 800),
	}, now)
	require.Error(t, err)
}
