package money_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

func TestMoneyJSONUsesOnlyMicrodollars(t *testing.T) {
	encoded, err := json.Marshal(money.Money{Microdollars: 420_000})
	require.NoError(t, err)
	assert.JSONEq(t, `{"microdollars":420000}`, string(encoded))
}

func TestAddAndSubRejectOverflow(t *testing.T) {
	tests := []struct {
		name  string
		left  money.Money
		right money.Money
		op    func(money.Money, money.Money) (money.Money, error)
	}{
		{
			name:  "add above maximum",
			left:  money.Money{Microdollars: math.MaxInt64},
			right: money.Money{Microdollars: 1},
			op:    money.Add,
		},
		{
			name:  "add below minimum",
			left:  money.Money{Microdollars: math.MinInt64},
			right: money.Money{Microdollars: -1},
			op:    money.Add,
		},
		{
			name:  "subtract below minimum",
			left:  money.Money{Microdollars: math.MinInt64},
			right: money.Money{Microdollars: 1},
			op:    money.Sub,
		},
		{
			name:  "subtract above maximum",
			left:  money.Money{Microdollars: math.MaxInt64},
			right: money.Money{Microdollars: -1},
			op:    money.Sub,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.op(tt.left, tt.right)
			assert.ErrorIs(t, err, money.ErrOverflow)
		})
	}
}

func TestSumReturnsExactMicrodollars(t *testing.T) {
	got, err := money.Sum(
		money.Money{Microdollars: 1_250_000},
		money.Money{Microdollars: 750_000},
		money.Money{Microdollars: -500_000},
	)
	require.NoError(t, err)
	assert.Equal(t, money.Money{Microdollars: 1_500_000}, got)
}

func TestCostPerMillionRoundsCombinedRowOnce(t *testing.T) {
	got, err := money.CostPerMillion([]money.RatedTokens{
		{Tokens: 1, Rate: money.Money{Microdollars: 500_000}},
		{Tokens: 1, Rate: money.Money{Microdollars: 500_000}},
	})
	require.NoError(t, err)
	assert.Equal(t, money.Money{Microdollars: 1}, got)
}

func TestCostPerMillionUsesWideIntermediate(t *testing.T) {
	got, err := money.CostPerMillion([]money.RatedTokens{{
		Tokens: 3_100_000_000,
		Rate:   money.Money{Microdollars: 3_000_000_000},
	}})
	require.NoError(t, err)
	assert.Equal(t, money.Money{Microdollars: 9_300_000_000_000}, got)
}

func TestCostPerMillionRejectsInvalidOrOverflowingInputs(t *testing.T) {
	tests := []struct {
		name string
		rows []money.RatedTokens
		err  error
	}{
		{
			name: "negative tokens",
			rows: []money.RatedTokens{{
				Tokens: -1,
				Rate:   money.Money{Microdollars: 1},
			}},
			err: money.ErrNegative,
		},
		{
			name: "negative rate",
			rows: []money.RatedTokens{{
				Tokens: 1,
				Rate:   money.Money{Microdollars: -1},
			}},
			err: money.ErrNegative,
		},
		{
			name: "final result overflow",
			rows: []money.RatedTokens{{
				Tokens: math.MaxInt64,
				Rate:   money.Money{Microdollars: math.MaxInt64},
			}},
			err: money.ErrOverflow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := money.CostPerMillion(tt.rows)
			assert.ErrorIs(t, err, tt.err)
		})
	}
}
