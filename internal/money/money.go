// Package money provides AgentsView's exact monetary value and arithmetic.
package money

import (
	"errors"
	"math"
	"math/bits"
)

const microdollarsPerDollar = 1_000_000

var (
	ErrInvalidDecimal = errors.New("invalid decimal money")
	ErrNegative       = errors.New("money value must not be negative")
	ErrOverflow       = errors.New("money overflow")
)

// Money is a signed count of microdollars. It is the sole machine-readable
// representation of a monetary value.
type Money struct {
	Microdollars int64 `json:"microdollars"`
}

// RatedTokens pairs a nonnegative token count with a nonnegative
// microdollars-per-million-token rate.
type RatedTokens struct {
	Tokens int64
	Rate   Money
}

// Add returns the exact sum or ErrOverflow.
func Add(a, b Money) (Money, error) {
	av := a.Microdollars
	bv := b.Microdollars
	if (bv > 0 && av > math.MaxInt64-bv) ||
		(bv < 0 && av < math.MinInt64-bv) {
		return Money{}, ErrOverflow
	}
	return Money{Microdollars: av + bv}, nil
}

// Sub returns the exact difference or ErrOverflow.
func Sub(a, b Money) (Money, error) {
	av := a.Microdollars
	bv := b.Microdollars
	if (bv > 0 && av < math.MinInt64+bv) ||
		(bv < 0 && av > math.MaxInt64+bv) {
		return Money{}, ErrOverflow
	}
	return Money{Microdollars: av - bv}, nil
}

// Sum returns the exact sum of every value or ErrOverflow.
func Sum(values ...Money) (Money, error) {
	var total Money
	for _, value := range values {
		var err error
		total, err = Add(total, value)
		if err != nil {
			return Money{}, err
		}
	}
	return total, nil
}

// CostPerMillion prices one usage row. Products are accumulated without
// rounding, then the combined row is rounded once to the nearest microdollar.
func CostPerMillion(parts []RatedTokens) (Money, error) {
	var high, low uint64
	for _, part := range parts {
		if part.Tokens < 0 || part.Rate.Microdollars < 0 {
			return Money{}, ErrNegative
		}
		productHigh, productLow := bits.Mul64(
			uint64(part.Tokens),
			uint64(part.Rate.Microdollars),
		)
		var carry uint64
		low, carry = bits.Add64(low, productLow, 0)
		var overflow uint64
		high, overflow = bits.Add64(high, productHigh, carry)
		if overflow != 0 {
			return Money{}, ErrOverflow
		}
	}

	if high >= microdollarsPerDollar {
		return Money{}, ErrOverflow
	}
	quotient, remainder := bits.Div64(high, low, microdollarsPerDollar)
	if quotient > math.MaxInt64 ||
		(quotient == math.MaxInt64 && remainder >= microdollarsPerDollar/2) {
		return Money{}, ErrOverflow
	}
	if remainder >= microdollarsPerDollar/2 {
		quotient++
	}
	return Money{Microdollars: int64(quotient)}, nil
}
