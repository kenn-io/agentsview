package friction

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// maxUSDScale and maxUSDCoeff mirror rust_decimal's limits: 28 fractional
// digits and a 96-bit coefficient.
const maxUSDScale = 28

var maxUSDCoeff = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 96), big.NewInt(1))

// USD is a scale-preserving decimal for jilog's spend calculations. Addition
// keeps the larger scale, and formatting never rounds.
type USD struct {
	Coeff *big.Int
	Scale uint8
}

// ParseUSD parses a plain signed decimal with an optional fractional part.
// Exponents, underscores, and whitespace are not accepted.
func ParseUSD(s string) (USD, error) {
	body := s
	neg := false
	if strings.HasPrefix(body, "-") || strings.HasPrefix(body, "+") {
		neg = body[0] == '-'
		body = body[1:]
	}
	intPart, frac, hasDot := strings.Cut(body, ".")
	if intPart == "" && frac == "" {
		return USD{}, fmt.Errorf("friction: invalid decimal %q", s)
	}
	if hasDot && strings.Contains(frac, ".") {
		return USD{}, fmt.Errorf("friction: invalid decimal %q", s)
	}
	digits := intPart + frac
	for _, r := range digits {
		if r < '0' || r > '9' {
			return USD{}, fmt.Errorf("friction: invalid decimal %q", s)
		}
	}
	if len(frac) > maxUSDScale {
		return USD{}, fmt.Errorf("friction: decimal %q has more than %d fractional digits", s, maxUSDScale)
	}
	coeff, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return USD{}, fmt.Errorf("friction: invalid decimal %q", s)
	}
	if coeff.Cmp(maxUSDCoeff) > 0 {
		return USD{}, errors.New("friction: decimal overflow")
	}
	if neg {
		coeff.Neg(coeff)
	}
	return USD{Coeff: coeff, Scale: uint8(len(frac))}, nil
}

// USDFromMicros converts archive microdollars to a scale-6 decimal.
func USDFromMicros(micros int64) USD {
	return USD{Coeff: big.NewInt(micros), Scale: 6}
}

func (u USD) coeff() *big.Int {
	if u.Coeff == nil {
		return new(big.Int)
	}
	return u.Coeff
}

func (u USD) rescale(scale uint8) USD {
	if scale <= u.Scale {
		return USD{Coeff: new(big.Int).Set(u.coeff()), Scale: u.Scale}
	}
	mul := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale-u.Scale)), nil)
	return USD{Coeff: new(big.Int).Mul(u.coeff(), mul), Scale: scale}
}

// Add returns u+v at the greater input scale without changing either input.
func (u USD) Add(v USD) USD {
	scale := max(u.Scale, v.Scale)
	a, b := u.rescale(scale), v.rescale(scale)
	return USD{Coeff: a.Coeff.Add(a.Coeff, b.Coeff), Scale: scale}
}

// String renders the raw decimal, retaining its scale and omitting a zero sign.
func (u USD) String() string {
	c := u.coeff()
	digits := new(big.Int).Abs(c).String()
	if u.Scale > 0 {
		if pad := int(u.Scale) + 1 - len(digits); pad > 0 {
			digits = strings.Repeat("0", pad) + digits
		}
		cut := len(digits) - int(u.Scale)
		digits = digits[:cut] + "." + digits[cut:]
	}
	if c.Sign() < 0 {
		return "-" + digits
	}
	return digits
}

// FormatUSD pads to at least cents, preserves subcent precision, and adds "$".
func FormatUSD(u USD) string {
	if u.Scale < 2 {
		u = u.rescale(2)
	}
	return "$" + u.String()
}
