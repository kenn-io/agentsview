package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTruncate(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()
	s, cut := truncate("hello", 10)
	assert.Equal("hello", s)
	assert.False(cut)

	s, cut = truncate("hello world", 5)
	assert.Equal("hello", s)
	assert.True(cut)

	// Rune-boundary safe: multibyte runes are not split.
	s, cut = truncate("héllo", 2)
	assert.True(cut)
	assert.Equal("hé", s)

	s, cut = truncate("anything", 0)
	assert.Equal("anything", s)
	assert.False(cut, "max<=0 means no truncation")
}

func TestClampLimit(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()
	assert.Equal(20, clampLimit(0, 20, 100), "unset -> default")
	assert.Equal(20, clampLimit(-5, 20, 100), "negative -> default")
	assert.Equal(50, clampLimit(50, 20, 100), "in range -> as-is")
	assert.Equal(20, clampLimit(1000, 20, 100), "over max -> default")
}

func TestIsActiveSince(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	assert.True(isActiveSince("2024-06-15T11:55:00Z", now), "5 min ago is active")
	assert.False(isActiveSince("2024-06-15T11:00:00Z", now), "1 h ago is not active")
	assert.False(isActiveSince("", now), "empty is not active")
	assert.False(isActiveSince("garbage", now), "unparseable is not active")
}

func TestRoleAllowed(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()
	assert.True(roleAllowed("user", nil))
	assert.True(roleAllowed("assistant", nil))
	assert.False(roleAllowed("tool", nil), "default excludes tool")
	assert.False(roleAllowed("system", nil), "default excludes system")
	assert.True(roleAllowed("tool", []string{"tool"}), "explicit allows tool")
	assert.False(roleAllowed("user", []string{"tool"}), "explicit filter excludes others")
}

func TestStrval(t *testing.T) {
	t.Parallel()
	assert.Empty(t, strval(nil))
	v := "x"
	assert.Equal(t, "x", strval(&v))
}
