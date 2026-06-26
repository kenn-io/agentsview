package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBuildSearchQuery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, raw, want string
	}{
		{"single word quoted", "login", `"login"`},
		{"multi-word AND", "fix bug", `"fix" "bug"`},
		{"hyphen token literal", "agentsview-mcp", `"agentsview-mcp"`},
		{"colon token literal", "status:500", `"status:500"`},
		{"embedded quote doubled", `say"hi`, `"say""hi"`},
		{"leading quote passthrough", `"fix bug"`, `"fix bug"`},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, buildSearchQuery(tc.raw))
		})
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	s, cut := truncate("hello", 10)
	assert.Equal(t, "hello", s)
	assert.False(t, cut)

	s, cut = truncate("hello world", 5)
	assert.Equal(t, "hello", s)
	assert.True(t, cut)

	// Rune-boundary safe: multibyte runes are not split.
	s, cut = truncate("héllo", 2)
	assert.True(t, cut)
	assert.Equal(t, "hé", s)

	s, cut = truncate("anything", 0)
	assert.Equal(t, "anything", s)
	assert.False(t, cut, "max<=0 means no truncation")
}

func TestClampLimit(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 20, clampLimit(0, 20, 100), "unset -> default")
	assert.Equal(t, 20, clampLimit(-5, 20, 100), "negative -> default")
	assert.Equal(t, 50, clampLimit(50, 20, 100), "in range -> as-is")
	assert.Equal(t, 20, clampLimit(1000, 20, 100), "over max -> default")
}

func TestIsActiveSince(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	assert.True(t, isActiveSince("2024-06-15T11:55:00Z", now), "5 min ago is active")
	assert.False(t, isActiveSince("2024-06-15T11:00:00Z", now), "1 h ago is not active")
	assert.False(t, isActiveSince("", now), "empty is not active")
	assert.False(t, isActiveSince("garbage", now), "unparseable is not active")
}

func TestRoleAllowed(t *testing.T) {
	t.Parallel()
	assert.True(t, roleAllowed("user", nil))
	assert.True(t, roleAllowed("assistant", nil))
	assert.False(t, roleAllowed("tool", nil), "default excludes tool")
	assert.False(t, roleAllowed("system", nil), "default excludes system")
	assert.True(t, roleAllowed("tool", []string{"tool"}), "explicit allows tool")
	assert.False(t, roleAllowed("user", []string{"tool"}), "explicit filter excludes others")
}

func TestStrval(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", strval(nil))
	v := "x"
	assert.Equal(t, "x", strval(&v))
}

// Guard against accidental whitespace regressions in the multi-term query
// builder.
func TestBuildSearchQuery_NoTrailingSpace(t *testing.T) {
	t.Parallel()
	got := buildSearchQuery("a b c")
	assert.False(t, strings.HasSuffix(got, " "))
	assert.Equal(t, `"a" "b" "c"`, got)
}
