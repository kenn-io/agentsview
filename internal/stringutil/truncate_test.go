package stringutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSafeTruncate(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		max         int
		want        string
	}{
		{"empty", "", 0, ""},
		{"zero", "abc", 0, ""},
		{"fits", "abc", 4, "abc"},
		{"exact", "abc", 3, "abc"},
		{"ascii", "abc", 2, "ab"},
		{"two-byte", "a\u00e9z", 2, "a"},
		{"three-byte", "a\u65e5z", 3, "a"},
		{"four-byte", "a\U0001f642z", 4, "a"},
		{"boundary", "a\u65e5z", 4, "a\u65e5"},
		{"no-room", "\U0001f642", 3, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SafeTruncate(tt.input, tt.max))
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	for _, tt := range []struct {
		name, input  string
		max          int
		suffix, want string
	}{
		{"empty", "", 0, "...", ""},
		{"zero", "abc", 0, "...", "..."},
		{"fits", "hi", 10, "...", "hi"},
		{"exact", "hello", 5, "...", "hello"},
		{"ascii", "hello world", 5, "...", "hello..."},
		{"no suffix", "hello world", 5, "", "hello"},
		{"two-byte", "a\u00e9z", 2, "...", "a\u00e9..."},
		{"three-byte", "\u65e5\u672c\u8a9e", 2, "...", "\u65e5\u672c..."},
		{"four-byte", "a\U0001f642z", 2, "\u2026", "a\U0001f642\u2026"},
		{"multibyte fits", "\u65e5\u672c", 2, "...", "\u65e5\u672c"},
		{"whitespace retained", " abc ", 3, "", " ab"},
		{"combining rune", "e\u0301x", 1, "", "e"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TruncateRunes(tt.input, tt.max, tt.suffix))
		})
	}
}
