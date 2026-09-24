package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustUSD(t *testing.T, s string) USD {
	t.Helper()
	u, err := ParseUSD(s)
	require.NoError(t, err)
	return u
}

// Ports format_usd_pads_cents_but_keeps_subcent_precision (jilog
// digest.rs:1522-1527) plus rust_decimal 1.42.1 Display cases.
func TestFormatUSD(t *testing.T) {
	tests := []struct {
		name string
		in   string
		str  string
		want string
	}{
		{"jilog/4.2", "4.2", "4.2", "$4.20"},
		{"jilog/7", "7", "7", "$7.00"},
		{"jilog/0.0003", "0.0003", "0.0003", "$0.0003"},
		{"jilog/4.20", "4.20", "4.20", "$4.20"},
		{"archive scale 6", "332.138392", "332.138392", "$332.138392"},
		{"negative", "-0.5", "-0.5", "$-0.50"},
		{"plus sign", "+1.5", "1.5", "$1.50"},
		{"leading dot", ".5", "0.5", "$0.50"},
		{"trailing dot", "5.", "5", "$5.00"},
		{"negative zero", "-0", "0", "$0.00"},
		{"negative zero scaled", "-0.00", "0.00", "$0.00"},
		{"leading zeros", "00.5", "0.5", "$0.50"},
		{"keeps trailing zeros", "0.000", "0.000", "$0.000"},
		{"max scale", "0.1234567890123456789012345678", "0.1234567890123456789012345678", "$0.1234567890123456789012345678"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := mustUSD(t, tt.in)
			assert.Equal(t, tt.str, u.String())
			assert.Equal(t, tt.want, FormatUSD(u))
		})
	}
}

func TestParseUSDRejects(t *testing.T) {
	for _, in := range []string{"", "abc", "1.2.3", "1,5", " 1", "1 ", "1e5", "1_000.5", "-", ".", "79228162514264337593543950336", "0.12345678901234567890123456789"} {
		t.Run(in, func(t *testing.T) {
			_, err := ParseUSD(in)
			assert.Error(t, err)
		})
	}
	u := mustUSD(t, "79228162514264337593543950335")
	assert.Equal(t, "79228162514264337593543950335", u.String())
}

func TestUSDArithmetic(t *testing.T) {
	tests := []struct {
		name string
		a, b USD
		want string
	}{
		{"max scale kept", mustUSD(t, "1.5"), mustUSD(t, "2.25"), "3.75"},
		{"trailing zero scale kept", mustUSD(t, "1.50"), mustUSD(t, "1"), "2.50"},
		{"micros stay scale 6", USDFromMicros(1_200_000), USDFromMicros(3_000_000), "4.200000"},
		{"micros plus coarse", USDFromMicros(1), mustUSD(t, "4.2"), "4.200001"},
		{"zero value is zero", USD{}, mustUSD(t, "1.5"), "1.5"},
		{"negative cancellation keeps scale", mustUSD(t, "1.50"), mustUSD(t, "-1.50"), "0.00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sum := tt.a.Add(tt.b)
			assert.Equal(t, tt.want, sum.String())
		})
	}
	// The returned sum must not share mutable coefficient storage with either operand.
	a := mustUSD(t, "1.5")
	b := mustUSD(t, "1.25")
	sum := a.Add(b)
	assert.Equal(t, "2.75", sum.String())
	assert.Equal(t, "1.5", a.String())
	assert.Equal(t, "1.25", b.String())
	sum.Coeff.SetInt64(0)
	assert.Equal(t, "1.5", a.String())
	assert.Equal(t, "1.25", b.String())
	assert.Equal(t, "$1300.250000", FormatUSD(USDFromMicros(1_300_250_000)))
	assert.Equal(t, "$0.000001", FormatUSD(USDFromMicros(1)))
	assert.Equal(t, "0", USD{}.String())
	assert.Equal(t, "$0.00", FormatUSD(USD{}))
	assert.Equal(t, "0", USD{}.Add(USD{}).String())
}
