package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFrictionSessionURL(t *testing.T) {
	tests := []struct {
		name, base, id string
		ord            *int
		want           string
	}{
		{"no_public_url", "", "claude:s1", new(1), ""},
		{"prefixed_with_ordinal", "https://h.example", "claude:s1", new(7), "https://h.example/sessions/claude/s1?msg=7"},
		{"no_ordinal", "https://h.example", "claude:s1", nil, "https://h.example/sessions/claude/s1"},
		{"unprefixed", "https://h.example", "abc", nil, "https://h.example/sessions/abc"},
		{"escapes_rest", "https://h.example/base", "codex:abc/def ghi", nil, "https://h.example/base/sessions/codex/abc%2Fdef%20ghi"},
		{"only_first_colon_splits", "https://h.example", "remote~m:claude:x", nil, "https://h.example/sessions/remote~m/claude:x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, frictionSessionURL(tt.base, tt.id, tt.ord))
		})
	}
}
