package export

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

// Prices asserted against Anthropic's published web search fee of
// $10 per 1,000 requests:
// https://docs.claude.com/en/docs/agents-and-tools/tool-use/web-search-tool
func TestWebSearchFee(t *testing.T) {
	tests := []struct {
		name     string
		requests int
		want     money.Money
	}{
		{name: "no requests", requests: 0, want: money.Money{}},
		{name: "negative count is free", requests: -3, want: money.Money{}},
		{
			name:     "one request is one cent",
			requests: 1,
			want:     money.MustParseDollars("0.01"),
		},
		{
			name:     "one thousand requests is ten dollars",
			requests: 1000,
			want:     money.MustParseDollars("10"),
		},
		{
			name:     "two thousand five hundred requests",
			requests: 2500,
			want:     money.MustParseDollars("25"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := WebSearchFee(tt.requests)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestWebSearchFeeIsExactMicrodollars(t *testing.T) {
	fee, err := WebSearchFee(1)
	require.NoError(t, err)
	assert.Equal(t, WebSearchRequestMicrodollars, fee.Microdollars)
	assert.Equal(t, int64(10_000), fee.Microdollars)
}

func TestAddWebSearchFee(t *testing.T) {
	base := money.MustParseDollars("1.25")

	unchanged, err := AddWebSearchFee(base, 0)
	require.NoError(t, err)
	assert.Equal(t, base, unchanged)

	withFee, err := AddWebSearchFee(base, 3)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("1.28"), withFee)

	fromZero, err := AddWebSearchFee(money.Money{}, 2)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.02"), fromZero)
}

func TestAddWebSearchFeeReportsOverflow(t *testing.T) {
	_, err := AddWebSearchFee(
		money.Money{Microdollars: math.MaxInt64}, 1)
	require.Error(t, err)
}
