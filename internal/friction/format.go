package friction

import (
	"fmt"
	"strings"
	"unicode"

	"go.kenn.io/agentsview/internal/stringutil"
)

// truncatedMarker is jilog's suffix.
const truncatedMarker = " … [truncated]"

// TruncateRunes keeps at most n runes and adds no ellipsis.
func TruncateRunes(s string, n int) string {
	return stringutil.TruncateRunes(s, n, "")
}

// TruncateWithMarker keeps at most n runes and appends the marker only
// when text was removed.
func TruncateWithMarker(s string, n int) string {
	return stringutil.TruncateRunes(s, n, truncatedMarker)
}

// PythonRepr approximates Python repr() for a string: single quotes;
// backslash, quote, newline, carriage return, and tab escaped; other
// runes below 0x20 as \xHH; everything else verbatim.
func PythonRepr(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\'':
			b.WriteString(`\'`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// SanitizeDisplay replaces Unicode control runes with spaces for one-line display.
func SanitizeDisplay(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return ' '
		default:
			return r
		}
	}, s)
}
