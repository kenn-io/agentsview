package readbase

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestActivityUsageSumsCovers(t *testing.T) {
	sums := ActivityUsageSums{EndUS: 10, NextUS: 20}
	for _, tc := range []struct {
		end  int64
		want bool
	}{{9, false}, {10, true}, {15, true}, {20, true}, {21, false}} {
		require.Equal(t, tc.want, sums.Covers(tc.end), "end %d", tc.end)
	}
}
