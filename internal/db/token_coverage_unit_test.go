package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeSessionCoverageUpdates(t *testing.T) {
	tests := []struct {
		name        string
		candidates  []SessionCoverageCandidate
		msgCoverage map[string][2]bool
		want        []SessionCoverageUpdate
	}{
		{
			name: "basic: three candidates with mixed updates",
			candidates: []SessionCoverageCandidate{
				// gets both flags from message coverage
				{
					ID: "a", TotalOutputTokens: 0, PeakContextTokens: 0,
					HasTotal: false, HasPeak: false,
				},
				// gets hasTotal from non-zero total tokens
				{
					ID: "b", TotalOutputTokens: 100, PeakContextTokens: 0,
					HasTotal: false, HasPeak: false,
				},
				// already has both flags — no update needed
				{
					ID: "c", TotalOutputTokens: 50, PeakContextTokens: 200,
					HasTotal: true, HasPeak: true,
				},
			},
			msgCoverage: map[string][2]bool{
				"a": {true, true}, // hasContext=true, hasOutput=true
			},
			want: []SessionCoverageUpdate{
				{ID: "a", HasTotal: true, HasPeak: true},
				{ID: "b", HasTotal: true, HasPeak: false},
			},
		},
		{
			name:        "empty: nil candidates and nil coverage returns empty",
			candidates:  nil,
			msgCoverage: nil,
			want:        []SessionCoverageUpdate{},
		},
		{
			name: "no updates needed: all candidates already correct",
			candidates: []SessionCoverageCandidate{
				{
					ID: "x", TotalOutputTokens: 10, PeakContextTokens: 20,
					HasTotal: true, HasPeak: true,
				},
				{
					ID: "y", TotalOutputTokens: 0, PeakContextTokens: 0,
					HasTotal: false, HasPeak: false,
				},
			},
			msgCoverage: map[string][2]bool{},
			want:        []SessionCoverageUpdate{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeSessionCoverageUpdates(tc.candidates, tc.msgCoverage)

			require.Len(t, got, len(tc.want), "len mismatch; got = %v", got)
			for i, w := range tc.want {
				assert.Equal(t, w.ID, got[i].ID, "[%d] ID", i)
				assert.Equal(t, w.HasTotal, got[i].HasTotal, "[%d] HasTotal", i)
				assert.Equal(t, w.HasPeak, got[i].HasPeak, "[%d] HasPeak", i)
			}
		})
	}
}
