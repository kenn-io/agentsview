package friction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"unicode/no truncation", "日本語", 3, "日本語"},
		{"unicode/two", "日本語", 2, "日本"},
		{"empty", "", 5, ""},
		{"zero", "abc", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TruncateRunes(tt.in, tt.n))
		})
	}
}

func TestTruncateWithMarker(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"truncated", strings.Repeat("x", 10), 5, "xxxxx … [truncated]"},
		{"no truncation", "short", 100, "short"},
		{"exact limit", strings.Repeat("x", MaxErrorMessageLength), MaxErrorMessageLength, strings.Repeat("x", MaxErrorMessageLength)},
		{"rune count", strings.Repeat("日", 501), MaxErrorMessageLength, strings.Repeat("日", 500) + " … [truncated]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TruncateWithMarker(tt.in, tt.n))
		})
	}
}

func TestPythonRepr(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", `'hello'`},
		{"quote", "it's", `'it\'s'`},
		{"newline", "a\nb", `'a\nb'`},
		{"correction", "don't do that", `'don\'t do that'`},
		{"backslash", `a\b`, `'a\\b'`},
		{"cr and tab", "a\r\tb", `'a\r\tb'`},
		{"other controls", "a\x01\x1fb", `'a\x01\x1fb'`},
		{"del and non-ASCII", "a\x7fé日", "'a\x7fé日'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, PythonRepr(tt.in))
		})
	}
}

func TestSanitizeDisplay(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"backtick and newline", "general`\ninjected", "general' injected"},
		{"backtick tab newline", "seat`\t\n01", "seat'  01"},
		{"machine label", "mac`1\n", "mac'1 "},
		{"c1 control", "a\u0085b", "a b"},
		{"nbsp", "a\u00a0b", "a\u00a0b"},
		{"plain", "helper", "helper"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SanitizeDisplay(tt.in))
		})
	}
}
