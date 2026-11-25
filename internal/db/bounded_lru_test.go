package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBoundedLRUTrimUsesStoredKey(t *testing.T) {
	var memo boundedLRU[string, string]
	memo.add("a", "different value", 1)
	memo.trim(0)
	assert.Zero(t, memo.Len())
	assert.Zero(t, memo.bytes)
}
