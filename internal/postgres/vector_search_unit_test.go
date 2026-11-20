package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestSemanticUnavailableError(t *testing.T) {
	s := &Store{}

	// No reason set: bare sentinel.
	err := s.semanticUnavailableError()
	require.ErrorIs(t, err, db.ErrSemanticUnavailable)

	s.SetSemanticUnavailableReason("pgvector extension not installed")
	err = s.semanticUnavailableError()
	require.ErrorIs(t, err, db.ErrSemanticUnavailable,
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

func TestStoreSemanticReadinessReportsUnavailableReasonAndReadyWithoutCounts(t *testing.T) {
	s := &Store{}
	s.SetSemanticUnavailableReason("no compatible embeddings generation")

	status, err := s.SemanticReadiness(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "unavailable", status.State)
	assert.Equal(t, "no compatible embeddings generation", status.Reason)

	s.SetVectorSearcher(NewVectorSearcher(nil, 1, 4, 100, nil))
	status, err = s.SemanticReadiness(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "ready", status.State)
	assert.Equal(t, "counts_unavailable", status.Reason)
	assert.Empty(t, status.Generation)
}
