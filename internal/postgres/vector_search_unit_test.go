package postgres

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestHalfvecLiteral(t *testing.T) {
	t.Run("finite vector renders pgvector text form", func(t *testing.T) {
		literal, err := halfvecLiteral([]float32{1, -0.5, 0})
		require.NoError(t, err)
		assert.Equal(t, "[1,-0.5,0]", literal)
	})

	t.Run("empty vector", func(t *testing.T) {
		literal, err := halfvecLiteral(nil)
		require.NoError(t, err)
		assert.Equal(t, "[]", literal)
	})

	nonFinite := []struct {
		name string
		vec  []float32
	}{
		{"NaN", []float32{0, float32(math.NaN()), 1}},
		{"+Inf", []float32{float32(math.Inf(1)), 0}},
		{"-Inf", []float32{0, 0, float32(math.Inf(-1))}},
	}
	for _, tt := range nonFinite {
		t.Run(tt.name+" is rejected", func(t *testing.T) {
			_, err := halfvecLiteral(tt.vec)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "non-finite embedding value")
		})
	}
}

func TestHNSWEfSearch(t *testing.T) {
	cases := []struct {
		name string
		k    int
		want int
	}{
		{"below floor clamps up", 1, 40},
		{"just below floor", 39, 40},
		{"at floor", 40, 40},
		{"mid range passes through", 500, 500},
		{"at ceiling", 1000, 1000},
		{"above ceiling clamps down", 5000, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hnswEfSearch(tc.k))
		})
	}
}

func TestRollupChunkHits(t *testing.T) {
	// Best-first order in, one row per doc out (first seen wins), truncated.
	hits := []chunkHit{
		{docKey: "a", chunkIndex: 0, score: 0.9},
		{docKey: "b", chunkIndex: 1, score: 0.8},
		{docKey: "a", chunkIndex: 2, score: 0.7}, // dominated duplicate dropped
		{docKey: "c", chunkIndex: 0, score: 0.6},
	}

	rolled := rollupChunkHits(hits, 10)
	require.Len(t, rolled, 3)
	assert.Equal(t, "a", rolled[0].docKey)
	assert.Equal(t, 0, rolled[0].chunkIndex, "best chunk kept for doc a")
	assert.Equal(t, "b", rolled[1].docKey)
	assert.Equal(t, "c", rolled[2].docKey)

	truncated := rollupChunkHits(hits, 2)
	require.Len(t, truncated, 2)
	assert.Equal(t, "a", truncated[0].docKey)
	assert.Equal(t, "b", truncated[1].docKey)

	assert.Empty(t, rollupChunkHits(nil, 5))
}

func TestSemanticUnavailableError(t *testing.T) {
	s := &Store{}

	// No reason set: bare sentinel.
	err := s.semanticUnavailableError()
	assert.True(t, errors.Is(err, db.ErrSemanticUnavailable))

	s.SetSemanticUnavailableReason("pgvector extension not installed")
	err = s.semanticUnavailableError()
	assert.True(t, errors.Is(err, db.ErrSemanticUnavailable),
		"reasoned error still wraps the sentinel")
	assert.Contains(t, err.Error(), "pgvector extension not installed")
}

func TestStoreVectorSearcherWiring(t *testing.T) {
	s := &Store{}
	assert.False(t, s.HasSemantic(), "no searcher wired")

	s.SetVectorSearcher(NewVectorSearcher(nil, 1, 4, 100, nil))
	assert.True(t, s.HasSemantic(), "searcher wired")

	s.SetVectorSearcher(nil)
	assert.False(t, s.HasSemantic(), "searcher cleared")
}
