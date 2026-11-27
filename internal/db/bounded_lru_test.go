package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBoundedLRUTrimToByteBudget(t *testing.T) {
	var memo boundedLRU[string, string]
	memo.add("a", "value-a", 1)
	memo.add("b", "value-b", 2)
	memo.trim(2)
	assert.Equal(t, []string{"b"}, memo.keys())
	assert.EqualValues(t, 2, memo.bytes)
	memo.trim(0)
	assert.Empty(t, memo.keys())
	assert.Zero(t, memo.Len())
	assert.Zero(t, memo.bytes)
}
