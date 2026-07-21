package money_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/money"
)

func TestFormatUSD(t *testing.T) {
	tests := []struct {
		name  string
		money money.Money
		want  string
	}{
		{name: "zero", money: money.Money{}, want: "$0.00"},
		{
			name:  "nonzero under half cent",
			money: money.Money{Microdollars: 1},
			want:  "<$0.01",
		},
		{
			name:  "ordinary cents",
			money: money.Money{Microdollars: 420_000},
			want:  "$0.42",
		},
		{
			name:  "grouped dollars",
			money: money.Money{Microdollars: 1_234_560_000},
			want:  "$1,234.56",
		},
		{
			name:  "negative",
			money: money.Money{Microdollars: -420_000},
			want:  "-$0.42",
		},
		{
			name:  "minimum int64",
			money: money.Money{Microdollars: math.MinInt64},
			want:  "-$9,223,372,036,854.78",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, money.FormatUSD(tt.money, money.DisplayCents))
		})
	}
}
