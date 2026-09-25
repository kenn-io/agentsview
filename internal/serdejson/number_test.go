package serdejson

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNumberKind(t *testing.T) {
	tests := []struct {
		name    string
		literal Number
		want    NumberKind
		i64     int64
		i64OK   bool
	}{
		{"zero", "0", NumberU64, 0, true},
		{"u64 max", "18446744073709551615", NumberU64, 0, false},
		{"u64 overflow is f64", "18446744073709551616", NumberF64, 0, false},
		{"above 2^53 stays integer", "9007199254740993", NumberU64, 9007199254740993, true},
		{"negative", "-5", NumberI64, -5, true},
		{"i64 min", "-9223372036854775808", NumberI64, -9223372036854775808, true},
		{"below i64 min is f64", "-9223372036854775809", NumberF64, 0, false},
		{"negative zero is f64", "-0", NumberF64, 0, false},
		{"fraction", "1.0", NumberF64, 0, false},
		{"exponent", "1e5", NumberF64, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.literal.Kind())
			got, ok := tt.literal.Int64()
			assert.Equal(t, tt.i64OK, ok)
			assert.Equal(t, tt.i64, got)
		})
	}
}

// Expected strings were captured from serde_json 1.0.149 (zmij 1.0.x)
// Value::to_string on the same literals.
func TestFormatFloat(t *testing.T) {
	tests := []struct {
		literal Number
		want    string
	}{
		{"1.0", "1.0"},
		{"1.5", "1.5"},
		{"-0", "-0.0"},
		{"-0.0", "-0.0"},
		{"0.1", "0.1"},
		{"0.3", "0.3"},
		{"1e16", "1e+16"},
		{"1e15", "1000000000000000.0"},
		{"1e-5", "0.00001"},
		{"1e-6", "1e-6"},
		{"1e-7", "1e-7"},
		{"1.0e2", "100.0"},
		{"17.0", "17.0"},
		{"3.14159", "3.14159"},
		{"4.35", "4.35"},
		{"5e-324", "5e-324"},
		{"100000000000000000000.0", "1e+20"},
		{"0.000001234", "1.234e-6"},
		{"123e-20", "1.23e-18"},
		{"1e22", "1e+22"},
		{"1.5e300", "1.5e+300"},
		{"1e100", "1e+100"},
		{"18446744073709551616", "1.8446744073709552e+19"},
		{"-9223372036854775809", "-9.223372036854776e+18"},
		{"123456789012345678901234567890", "1.2345678901234568e+29"},
		{"1.7976931348623157e308", "1.7976931348623157e+308"},
		{"2.2250738585072014e-308", "2.2250738585072014e-308"},
	}
	for _, tt := range tests {
		t.Run(string(tt.literal), func(t *testing.T) {
			got, err := tt.literal.canonical()
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNumberAccessorsRejectInvalidLiterals(t *testing.T) {
	for _, literal := range []Number{"01", "1.", "NaN", "1E400"} {
		t.Run(string(literal), func(t *testing.T) {
			_, ok := literal.Float64()
			assert.False(t, ok)
			_, err := literal.canonical()
			require.Error(t, err)
		})
	}
	for _, tt := range []struct {
		literal Number
		want    uint64
		ok      bool
	}{
		{"0", 0, true},
		{"18446744073709551615", 18446744073709551615, true},
		{"-1", 0, false},
		{"-0", 0, false},
		{"1.0", 0, false},
		{"18446744073709551616", 0, false},
	} {
		t.Run("uint64/"+string(tt.literal), func(t *testing.T) {
			got, ok := tt.literal.Uint64()
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
