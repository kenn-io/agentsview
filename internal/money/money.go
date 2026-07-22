// Package money provides AgentsView's exact monetary value and arithmetic.
package money

import (
	"errors"
	"math"
	"math/big"
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

// SignedCostPerMillion prices a row whose rates may be signed, such as cache
// savings. Products are summed exactly and the combined result is rounded once
// to the nearest microdollar, with halves away from zero.
func SignedCostPerMillion(parts []RatedTokens) (Money, error) {
	total := new(big.Int)
	for _, part := range parts {
		if part.Tokens < 0 {
			return Money{}, ErrNegative
		}
		product := new(big.Int).Mul(
			big.NewInt(part.Tokens),
			big.NewInt(part.Rate.Microdollars),
		)
		total.Add(total, product)
	}

	negative := total.Sign() < 0
	magnitude := new(big.Int).Abs(total)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(magnitude, big.NewInt(microdollarsPerDollar), remainder)
	if remainder.Cmp(big.NewInt(microdollarsPerDollar/2)) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if negative {
		quotient.Neg(quotient)
	}
	if !quotient.IsInt64() {
		return Money{}, ErrOverflow
	}
	return Money{Microdollars: quotient.Int64()}, nil
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

// MustAdd returns the exact sum and panics if the signed 64-bit range is
// exceeded. It is intended for aggregating already validated application
// values, where overflow means the archive is outside the supported domain.
func MustAdd(a, b Money) Money {
	total, err := Add(a, b)
	if err != nil {
		panic(err)
	}
	return total
}

// MustSub returns the exact difference and panics on overflow.
func MustSub(a, b Money) Money {
	difference, err := Sub(a, b)
	if err != nil {
		panic(err)
	}
	return difference
}

// Divide divides Money by a positive integer and rounds to the nearest
// microdollar, with halves away from zero.
func Divide(value Money, divisor int64) (Money, error) {
	if divisor <= 0 {
		return Money{}, ErrInvalidDecimal
	}
	numerator := big.NewInt(value.Microdollars)
	negative := numerator.Sign() < 0
	magnitude := new(big.Int).Abs(numerator)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(magnitude, big.NewInt(divisor), remainder)
	twiceRemainder := new(big.Int).Lsh(remainder, 1)
	if twiceRemainder.Cmp(big.NewInt(divisor)) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if negative {
		quotient.Neg(quotient)
	}
	if !quotient.IsInt64() {
		return Money{}, ErrOverflow
	}
	return Money{Microdollars: quotient.Int64()}, nil
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
