package stringutil

import (
	"strings"
	"unicode"
)

// SanitizeUTF8 removes invalid UTF-8 and controls other than display whitespace.
// Apply it before recording byte offsets into message bodies.
func SanitizeUTF8(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ToValidUTF8(s, "")
	strip := func(r rune) bool {
		return r != '\n' && r != '\t' && r != '\r' && unicode.IsControl(r)
	}
	if strings.IndexFunc(s, strip) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if strip(r) {
			return -1
		}
		return r
	}, s)
}
