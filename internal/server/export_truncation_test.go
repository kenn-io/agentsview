package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMarkdownFallbackUTF8(t *testing.T) {
	prefix := strings.Repeat("a", 199)
	assert.Equal(t, prefix+"\u2026", truncateMarkdownFallback(prefix+"\u65e5", 200))
}
