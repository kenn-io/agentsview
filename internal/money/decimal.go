package money

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseDollars parses a decimal dollar amount and rounds it to microdollars.
func ParseDollars(value string) (Money, error) {
	microdollars, err := ParseScaledDecimal(value, 6)
	if err != nil {
		return Money{}, err
	}
	return Money{Microdollars: microdollars}, nil
}

// MustParseDollars parses a decimal dollar amount or panics. It is intended
// for static constants and fixtures whose validity is established in source.
func MustParseDollars(value string) Money {
	parsed, err := ParseDollars(value)
	if err != nil {
		panic(err)
	}
	return parsed
}

// ParseCents parses a decimal cent amount and rounds it to microdollars.
func ParseCents(value string) (Money, error) {
	microdollars, err := ParseScaledDecimal(value, 4)
	if err != nil {
		return Money{}, err
	}
	return Money{Microdollars: microdollars}, nil
}

// FromFloatDollars converts a dollar value from an upstream boundary that has
// already discarded its original decimal representation.
func FromFloatDollars(value float64) (Money, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return Money{}, ErrInvalidDecimal
	}
	if value < 0 {
		return Money{}, ErrNegative
	}
	return ParseDollars(strconv.FormatFloat(value, 'g', -1, 64))
}

// ParseScaledDecimal parses decimal text into an integer with scale fractional
// decimal places. Discarded digits round to nearest, with exact halves away
// from zero.
func ParseScaledDecimal(value string, scale uint) (int64, error) {
	negative, digits, fractionalDigits, exponent, err := splitDecimal(value)
	if err != nil {
		return 0, err
	}

	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return 0, nil
	}
	if scale > math.MaxInt32 {
		return 0, ErrOverflow
	}
	shift64 := int64(exponent) - int64(fractionalDigits) + int64(scale)
	if shift64 > math.MaxInt32 || shift64 < math.MinInt32 {
		if shift64 < 0 {
			return 0, nil
		}
		return 0, ErrOverflow
	}

	integerDigits, roundUp, err := scaledIntegerDigits(digits, int(shift64))
	if err != nil {
		return 0, err
	}
	magnitude, err := parseMagnitude(integerDigits, negative)
	if err != nil {
		return 0, err
	}
	if roundUp {
		limit := uint64(math.MaxInt64)
		if negative {
			limit++
		}
		if magnitude == limit {
			return 0, ErrOverflow
		}
		magnitude++
	}

	if negative {
		if magnitude == uint64(math.MaxInt64)+1 {
			return math.MinInt64, nil
		}
		return -int64(magnitude), nil
	}
	return int64(magnitude), nil
}

func splitDecimal(value string) (
	negative bool,
	digits string,
	fractionalDigits int,
	exponent int32,
	err error,
) {
	if value == "" || strings.TrimSpace(value) != value {
		err = ErrInvalidDecimal
		return
	}

	if value[0] == '+' || value[0] == '-' {
		negative = value[0] == '-'
		value = value[1:]
		if value == "" {
			err = ErrInvalidDecimal
			return
		}
	}

	mantissa := value
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		mantissa = value[:index]
		exponentText := value[index+1:]
		if exponentText == "" || strings.ContainsAny(exponentText, "eE") {
			err = ErrInvalidDecimal
			return
		}
		parsed, parseErr := strconv.ParseInt(exponentText, 10, 32)
		if parseErr != nil {
			err = fmt.Errorf("%w: exponent", ErrInvalidDecimal)
			return
		}
		exponent = int32(parsed)
	}

	if mantissa == "" {
		err = ErrInvalidDecimal
		return
	}
	dot := strings.IndexByte(mantissa, '.')
	if dot >= 0 {
		if strings.IndexByte(mantissa[dot+1:], '.') >= 0 {
			err = ErrInvalidDecimal
			return
		}
		fractionalDigits = len(mantissa) - dot - 1
		digits = mantissa[:dot] + mantissa[dot+1:]
	} else {
		digits = mantissa
	}
	if digits == "" {
		err = ErrInvalidDecimal
		return
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			err = ErrInvalidDecimal
			return
		}
	}
	return
}

func scaledIntegerDigits(digits string, shift int) (string, bool, error) {
	if shift >= 0 {
		if len(digits)+shift > 19 {
			return "", false, ErrOverflow
		}
		return digits + strings.Repeat("0", shift), false, nil
	}

	cut := len(digits) + shift
	if cut < 0 {
		return "0", false, nil
	}
	if cut == 0 {
		return "0", digits[0] >= '5', nil
	}
	return digits[:cut], digits[cut] >= '5', nil
}

func parseMagnitude(digits string, negative bool) (uint64, error) {
	magnitude, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, ErrOverflow
	}
	limit := uint64(math.MaxInt64)
	if negative {
		limit++
	}
	if magnitude > limit {
		return 0, ErrOverflow
	}
	return magnitude, nil
}
