package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

func TestParseUsageTokenTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want UsageTokenTypes
	}{
		{name: "default all", want: UsageTokenTypesAll},
		{
			name: "output only",
			raw:  "output",
			want: UsageTokenTypeOutput,
		},
		{
			name: "canonical combination",
			raw:  "output,input,output",
			want: UsageTokenTypeInput | UsageTokenTypeOutput,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseUsageTokenTypes(tt.raw)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	for _, raw := range []string{"unknown", "input,unknown", ","} {
		t.Run("reject "+raw, func(t *testing.T) {
			_, err := ParseUsageTokenTypes(raw)
			require.Error(t, err)
		})
	}
}

func TestUsageTokenTypesTotal(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 25, UsageTokenTypeOutput.Total(100, 25, 40, 800))
	assert.Equal(t, 825,
		(UsageTokenTypeCacheRead|UsageTokenTypeOutput).
			Total(100, 25, 40, 800))
	assert.Equal(t, 965,
		UsageTokenTypes(0).Total(100, 25, 40, 800),
		"zero-value selection defaults to all token types")
}

func TestSortAndLimitTopSessionsUsesSelectedTokenTypes(t *testing.T) {
	t.Parallel()

	in := []TopSessionEntry{
		{
			SessionID: "input-heavy", InputTokens: 1000,
			OutputTokens: 1, TotalTokens: 1001,
			Cost: money.MustParseDollars("1"),
		},
		{
			SessionID: "output-heavy", InputTokens: 10,
			OutputTokens: 50, TotalTokens: 60,
			Cost: money.MustParseDollars("1"),
		},
	}

	got := SortAndLimitTopSessions(
		in, 1, TopSessionsSortTokens, UsageTokenTypeOutput,
	)
	require.Len(t, got, 1)
	assert.Equal(t, "output-heavy", got[0].SessionID)
}
