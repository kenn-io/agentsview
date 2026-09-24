package segfile

import (
	"fmt"
	"strings"
	"unicode"
)

// rustDebugString renders s the way Rust's `{:?}` renders a str, for the
// ported jilog messages that interpolate `{name:?}`. It escapes the
// characters Rust escapes in practice: quote, backslash, \t \r \n, NUL as
// \0, and other control characters as \u{hex}. Printable Unicode is kept.
func rustDebugString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		case 0:
			b.WriteString(`\0`)
		default:
			if unicode.IsControl(r) {
				fmt.Fprintf(&b, `\u{%x}`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// rustDebugStrings renders a []string like Rust's Debug for Vec<String>:
// ["a", "b"].
func rustDebugStrings(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = rustDebugString(v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
