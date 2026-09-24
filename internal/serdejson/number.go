// Package serdejson reproduces the byte output of Rust's serde_json 1.x
// (as pinned by jilog 9e8e094: serde_json 1.0.149 with zmij float
// formatting, no preserve_order, no arbitrary_precision) so that
// ported digests, fingerprints and ledger checksums match jilog.
package serdejson

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Number is a JSON number literal. It is classified the way serde_json's
// parser classifies it: u64, then i64, otherwise f64.
type Number string

// NumberKind is serde_json's internal number representation.
type NumberKind int

const (
	// NumberU64 is a non-negative integer that fits in uint64.
	NumberU64 NumberKind = iota
	// NumberI64 is a negative integer that fits in int64.
	NumberI64
	// NumberF64 is everything else: fractions, exponents, "-0",
	// and integers outside the u64/i64 ranges.
	NumberF64
)

// Kind classifies n the way serde_json's parse_number/parse_integer do
// (serde_json-1.0.149/src/de.rs). "-0" is F64 because serde_json stores
// a negated zero significand as F64(-0.0).
func (n Number) Kind() NumberKind {
	s := string(n)
	if strings.ContainsAny(s, ".eE") {
		return NumberF64
	}
	if strings.HasPrefix(s, "-") {
		if strings.TrimLeft(s[1:], "0") == "" {
			return NumberF64
		}
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			return NumberI64
		}
		return NumberF64
	}
	if _, err := strconv.ParseUint(s, 10, 64); err == nil {
		return NumberU64
	}
	return NumberF64
}

// Int64 mirrors serde_json's Value::as_i64: Some for an i64, or for a
// u64 that fits in i64; None for f64.
func (n Number) Int64() (int64, bool) {
	switch n.Kind() {
	case NumberU64:
		u, _ := strconv.ParseUint(string(n), 10, 64)
		if u > math.MaxInt64 {
			return 0, false
		}
		return int64(u), true
	case NumberI64:
		i, _ := strconv.ParseInt(string(n), 10, 64)
		return i, true
	default:
		return 0, false
	}
}

// Uint64 mirrors serde_json's Value::as_u64.
func (n Number) Uint64() (uint64, bool) {
	if n.Kind() != NumberU64 {
		return 0, false
	}
	u, _ := strconv.ParseUint(string(n), 10, 64)
	return u, true
}

// Float64 returns the literal as a float64 when it is finite and in range.
func (n Number) Float64() (float64, bool) {
	if !validNumberLiteral(string(n)) {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// canonical renders the number as serde_json would re-serialize it.
func (n Number) canonical() (string, error) {
	if !validNumberLiteral(string(n)) {
		return "", fmt.Errorf("serdejson: invalid number literal %q", string(n))
	}
	switch n.Kind() {
	case NumberU64, NumberI64:
		return string(n), nil
	default:
		f, ok := n.Float64()
		if !ok {
			return "", fmt.Errorf("serdejson: number out of range: %q", string(n))
		}
		return formatFloat(f), nil
	}
}

// validNumberLiteral applies the JSON number grammar (RFC 8259 §6).
func validNumberLiteral(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	default:
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(s)
}

// formatFloat ports zmij 1.0.21 `write` (src/lib.rs:1010-1130), which
// serde_json 1.0.149 uses for f64 (src/ser.rs:1716-1723). Shortest
// round-trip digits; plain notation when the decimal exponent is in
// [-5, 15], otherwise d.ddde±X with no exponent padding. Non-finite
// values serialize as null (serde_json ser.rs serialize_f64).
func formatFloat(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	sign := ""
	if f < 0 {
		sign = "-"
		f = -f
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde±XX
	mant, expStr, _ := strings.Cut(sci, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expStr)
	n := len(digits)
	if exp >= -5 && exp <= 15 {
		switch {
		case n-1 <= exp:
			return sign + digits + strings.Repeat("0", exp-(n-1)) + ".0"
		case exp >= 0:
			return sign + digits[:exp+1] + "." + digits[exp+1:]
		default:
			return sign + "0." + strings.Repeat("0", -exp-1) + digits
		}
	}
	out := sign + digits[:1]
	if n > 1 {
		out += "." + digits[1:]
	}
	if exp >= 0 {
		return out + "e+" + strconv.Itoa(exp)
	}
	return out + "e-" + strconv.Itoa(-exp)
}
