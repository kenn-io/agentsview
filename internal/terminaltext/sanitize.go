// Package terminaltext makes untrusted session text safe to render in a terminal.
package terminaltext

import (
	"strings"
	"unicode/utf8"
)

// Sanitize strips terminal control characters while preserving newlines and tabs.
func Sanitize(s string) string {
	if !hasControlBytes(s) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

func hasControlBytes(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\n' || c == '\t':
			continue
		case c < 0x20, c == 0x7f, c >= 0x80 && c <= 0x9f:
			return true
		}
	}
	return false
}
