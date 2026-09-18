package money_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

func TestParseScaledDecimal(t *testing.T) {
	tests := []struct {
		name  string
		input string
		scale uint
		want  int64
	}{
		{name: "zero", input: "0", scale: 6, want: 0},
		{name: "whole dollars", input: "42", scale: 6, want: 42_000_000},
		{name: "one microdollar", input: "0.000001", scale: 6, want: 1},
		{name: "below half rounds down", input: "0.0000004", scale: 6, want: 0},
		{name: "positive half rounds away", input: "0.0000005", scale: 6, want: 1},
		{name: "negative half rounds away", input: "-0.0000005", scale: 6, want: -1},
		{name: "fraction below half", input: "1.2345674", scale: 6, want: 1_234_567},
		{name: "fraction at half", input: "1.2345675", scale: 6, want: 1_234_568},
		{name: "positive exponent", input: "1.25e2", scale: 6, want: 125_000_000},
		{name: "negative exponent", input: "1e-6", scale: 6, want: 1},
		{name: "exponent half", input: "5e-7", scale: 6, want: 1},
		{
			name:  "maximum int64",
			input: "9223372036854.775807",
			scale: 6,
			want:  math.MaxInt64,
		},
		{
			name:  "minimum int64",
			input: "-9223372036854.775808",
			scale: 6,
			want:  math.MinInt64,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := money.ParseScaledDecimal(tt.input, tt.scale)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseScaledDecimalRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		scale uint
		err   error
	}{
		{name: "empty", input: "", scale: 6, err: money.ErrInvalidDecimal},
		{name: "sign only", input: "-", scale: 6, err: money.ErrInvalidDecimal},
		{name: "decimal only", input: ".", scale: 6, err: money.ErrInvalidDecimal},
		{name: "two decimals", input: "1.2.3", scale: 6, err: money.ErrInvalidDecimal},
		{name: "NaN", input: "NaN", scale: 6, err: money.ErrInvalidDecimal},
		{name: "infinity", input: "Inf", scale: 6, err: money.ErrInvalidDecimal},
		{name: "bad exponent", input: "1e", scale: 6, err: money.ErrInvalidDecimal},
		{
			name:  "above maximum",
			input: "9223372036854.775808",
			scale: 6,
			err:   money.ErrOverflow,
		},
		{
			name:  "below minimum",
			input: "-9223372036854.775809",
			scale: 6,
			err:   money.ErrOverflow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := money.ParseScaledDecimal(tt.input, tt.scale)
			assert.ErrorIs(t, err, tt.err)
		})
	}
}

func TestParseDollarsAndCents(t *testing.T) {
	dollars, err := money.ParseDollars("0.0424128")
	require.NoError(t, err)
	assert.Equal(t, money.Money{Microdollars: 42_413}, dollars)

	cents, err := money.ParseCents("15.66")
	require.NoError(t, err)
	assert.Equal(t, money.Money{Microdollars: 156_600}, cents)
}

func TestFromFloatDollarsConvertsUnavoidableUpstreamBoundary(t *testing.T) {
	got, err := money.FromFloatDollars(0.0424128)
	require.NoError(t, err)
	assert.Equal(t, money.Money{Microdollars: 42_413}, got)

	_, err = money.FromFloatDollars(math.NaN())
	assert.ErrorIs(t, err, money.ErrInvalidDecimal)
}

func TestMustParseDollars(t *testing.T) {
	assert.Equal(t, money.Money{Microdollars: 1_250_000}, money.MustParseDollars("1.25"))
	assert.Panics(t, func() { money.MustParseDollars("invalid") })
}

func TestFromFloatDollarsRejectsNegativeSourceCharge(t *testing.T) {
	_, err := money.FromFloatDollars(-0.01)
	assert.ErrorIs(t, err, money.ErrNegative)
}
