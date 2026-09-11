package postgres

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostedEmbeddingSplitBudget(t *testing.T) {
	r := HostedEmbeddingRecipe{MaxInputChars: 3, ChunkOverlapChars: 0}
	chunks, e := boundedEmbeddingChunks("a     b", r, 10, 1024)
	require.NoError(t, e)
	require.Len(t, chunks, 2)
	assert.Equal(t, 0, chunks[0].Index)
	assert.Equal(t, 2, chunks[1].Index)
	assert.Equal(t, "b", chunks[1].Text)
	r.MaxInputChars = 1000
	r.ChunkOverlapChars = 999
	_, e = boundedEmbeddingChunks(strings.Repeat("x", 10000), r, 100, 1<<20)
	assert.ErrorIs(t, e, ErrHostedEmbeddingWorkLimit)
	_, e = boundedEmbeddingChunks("hello", HostedEmbeddingRecipe{MaxInputChars: 5}, 100, 1)
	assert.ErrorIs(t, e, ErrHostedEmbeddingWorkLimit)
}
