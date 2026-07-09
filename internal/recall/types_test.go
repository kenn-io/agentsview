package recall

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeReviewState(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantState string
		wantOK    bool
	}{
		{
			name:      "empty defaults to unreviewed automatic",
			value:     "",
			wantState: ReviewStateUnreviewedAuto,
			wantOK:    true,
		},
		{
			name:      "whitespace defaults to unreviewed automatic",
			value:     "  ",
			wantState: ReviewStateUnreviewedAuto,
			wantOK:    true,
		},
		{
			name:      "human reviewed remains explicit",
			value:     ReviewStateHumanReviewed,
			wantState: ReviewStateHumanReviewed,
			wantOK:    true,
		},
		{
			name:      "unknown state is rejected",
			value:     "self_approved",
			wantState: "",
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotState, gotOK := NormalizeReviewState(tt.value)

			assert.Equal(t, tt.wantState, gotState)
			assert.Equal(t, tt.wantOK, gotOK)
		})
	}
}
