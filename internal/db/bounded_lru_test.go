package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBoundedLRUTrimUsesStoredKey(t *testing.T) {
	var memo boundedLRU[string, string]
	memo.add("a", "different value", 1)
	memo.trim(0, func(string) int64 { return 1 })
	assert.Empty(t, memo.entries)
	assert.Zero(t, memo.bytes)
	assert.Zero(t, memo.order.Len())
}
