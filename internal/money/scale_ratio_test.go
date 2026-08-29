package money

import (
	"math"
	"testing"
)

func TestScaleRatio(t *testing.T) {
	tests := []struct {
		name                                string
		value, numerator, denominator, want int64
	}{
		{"premium", 1_000_000, 11, 10, 1_100_000},
		{"identity", 123, 1, 1, 123},
		{"positive half", 5, 1, 2, 3},
		{"negative half", -5, 1, 2, -3},
		{"tiny", 1, 11, 10, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ScaleRatio(Money{Microdollars: tt.value}, tt.numerator, tt.denominator)
			if err != nil || got.Microdollars != tt.want {
				t.Fatalf("got %v, %v; want %d", got, err, tt.want)
			}
		})
	}
	t.Log("ratio boundaries: 11/10, 0.000001, halves away from zero")
}

func TestScaleRatioRejectsInvalidAndOverflow(t *testing.T) {
	for _, tt := range []struct {
		numerator, denominator int64
		want                   error
	}{
		{1, 0, ErrInvalidDecimal}, {1, -1, ErrInvalidDecimal}, {-1, 1, ErrNegative},
	} {
		if _, err := ScaleRatio(Money{Microdollars: 1}, tt.numerator, tt.denominator); err != tt.want {
			t.Errorf("got %v, want %v", err, tt.want)
		}
	}
	if _, err := ScaleRatio(Money{Microdollars: math.MaxInt64}, math.MaxInt64, 1); err != ErrOverflow {
		t.Fatalf("got %v, want overflow", err)
	}
}
