package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNoTokenData(t *testing.T) {
	cases := []struct {
		name   string
		totals UsageTotals
		want   bool
	}{
		{"all zero", UsageTotals{}, true},
		{"input tokens", UsageTotals{InputTokens: 1}, false},
		{"output tokens", UsageTotals{OutputTokens: 1}, false},
		{"cache creation", UsageTotals{CacheCreationTokens: 1}, false},
		{"cache read", UsageTotals{CacheReadTokens: 1}, false},
		{"cost", UsageTotals{TotalCost: 0.01}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NoTokenData(tc.totals))
		})
	}
}
