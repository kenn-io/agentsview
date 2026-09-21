// Package stringutil provides shared string operations.
package stringutil

import (
	"unicode/utf8"

	"golang.org/x/exp/utf8string"
)

// SafeTruncate cuts a string to maxBytes without breaking a UTF-8 character sequence.
// It assumes valid UTF-8 input and a nonnegative maxBytes. Callers add any suffix.
func SafeTruncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Step backward (at most 3 bytes) until we find the start of a valid UTF-8 character.
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// TruncateRunes keeps at most maxRunes Unicode code points, adding suffix only
// when text is removed. The nonnegative limit excludes the suffix. It does not
// trim whitespace or preserve grapheme clusters. Input is assumed to be valid
// UTF-8; this function does not sanitize malformed input.
func TruncateRunes(s string, maxRunes int, suffix string) string {
	text := utf8string.NewString(s)
	if text.RuneCount() <= maxRunes {
		return s
	}
	return text.Slice(0, maxRunes) + suffix
}
