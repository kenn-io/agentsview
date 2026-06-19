package signals

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type scoreGoldenCase struct {
	Name              string         `json:"name"`
	Input             ScoreInput     `json:"input"`
	WantBaselineScore *int           `json:"want_baseline_score"`
	WantScore         *int           `json:"want_score"`
	WantDelta         *int           `json:"want_delta"`
	WantGrade         string         `json:"want_grade"`
	WantBasis         []string       `json:"want_basis"`
	WantPenalties     map[string]int `json:"want_penalties"`
}

func TestComputeHealthScore_GoldenDeltas(t *testing.T) {
	raw, err := os.ReadFile("testdata/score_golden.json")
	require.NoError(t, err, "read score golden")

	var cases []scoreGoldenCase
	require.NoError(t, json.Unmarshal(raw, &cases), "parse score golden")

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			got := ComputeHealthScore(tc.Input)
			assertIntPtr(t, "Score", got.Score, tc.WantScore)
			assert.Equal(t, tc.WantGrade, got.Grade, "Grade")
			assert.Equal(t, tc.WantBasis, got.Basis, "Basis")
			assert.Equal(t, tc.WantPenalties, got.Penalties, "Penalties")

			baselineInput := tc.Input
			baselineInput.Heuristics = HeuristicSignals{}
			baseline := ComputeHealthScore(baselineInput)
			assertIntPtr(
				t, "baseline Score",
				baseline.Score, tc.WantBaselineScore,
			)
			assertDelta(t, baseline.Score, got.Score, tc.WantDelta)
		})
	}
}

// assertIntPtr compares two optional integers, treating nil as "absent".
func assertIntPtr(t *testing.T, name string, got, want *int) {
	t.Helper()
	assert.Equal(t, want, got, name)
}

// assertDelta checks that current-baseline equals the wanted delta. When any
// input score is absent the delta is unavailable, so want must also be absent.
func assertDelta(t *testing.T, baseline, current, want *int) {
	t.Helper()
	if baseline == nil || current == nil || want == nil {
		assert.Nil(t, want, "delta unavailable but a delta was expected")
		return
	}
	assert.Equal(t, *want, *current-*baseline, "delta")
}
