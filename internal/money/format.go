package money

import (
	"strconv"
	"strings"
)

// DisplayPrecision identifies a human-facing dollar precision policy.
type DisplayPrecision uint8

const (
	// DisplayCents renders two decimal places and marks positive nonzero values
	// below half a cent as less than one cent.
	DisplayCents DisplayPrecision = iota
)

// FormatUSD renders Money as an ordinary human-facing dollar amount.
func FormatUSD(value Money, _ DisplayPrecision) string {
	microdollars := value.Microdollars
	if microdollars > 0 && microdollars < 5_000 {
		return "<$0.01"
	}

	negative := microdollars < 0
	magnitude := unsignedMagnitude(microdollars)
	cents := (magnitude + 5_000) / 10_000
	dollars := cents / 100
	fraction := cents % 100

	var formatted strings.Builder
	if negative {
		formatted.WriteByte('-')
	}
	formatted.WriteByte('$')
	formatted.WriteString(groupThousands(dollars))
	formatted.WriteByte('.')
	if fraction < 10 {
		formatted.WriteByte('0')
	}
	formatted.WriteString(strconv.FormatUint(fraction, 10))
	return formatted.String()
}

func unsignedMagnitude(value int64) uint64 {
	if value >= 0 {
		return uint64(value)
	}
	return uint64(-(value + 1)) + 1
}

func groupThousands(value uint64) string {
	digits := strconv.FormatUint(value, 10)
	first := len(digits) % 3
	if first == 0 {
		first = 3
	}

	var grouped strings.Builder
	grouped.Grow(len(digits) + (len(digits)-1)/3)
	grouped.WriteString(digits[:first])
	for index := first; index < len(digits); index += 3 {
		grouped.WriteByte(',')
		grouped.WriteString(digits[index : index+3])
	}
	return grouped.String()
}
