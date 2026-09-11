//go:build pgtest

package postgres

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestHostedEmbeddingGenerationSearchFiltersStaleSources(t *testing.T) {
	f, store, generation, snapshot := embeddingOne(t)
	require.NoError(t, store.Publish(t.Context(), snapshot, embeddingOneVector()))
	activated, err := store.Activate(t.Context(), generation.ID)
	require.NoError(t, err)
	require.True(t, activated)

	hits, err := store.SearchGeneration(t.Context(), generation, []float32{1, 0, 0}, 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, db.VectorHit{SessionID: "s", Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 0, Score: 1, Snippet: "hello"}, hits[0])

	units, err := store.ResolveGenerationUnits(t.Context(), generation, []db.MessageRef{{SessionID: "s", Ordinal: 0}})
	require.NoError(t, err)
	assert.Equal(t, []db.UnitRef{{DocKey: "o:s:0", SessionID: "s", OrdinalStart: 0, OrdinalEnd: 0}}, units)

	_, err = f.runtime.Exec(`UPDATE messages SET content='changed' WHERE session_id='s' AND ordinal=0`)
	require.NoError(t, err)
	hits, err = store.SearchGeneration(t.Context(), generation, []float32{1, 0, 0}, 5)
	require.NoError(t, err)
	assert.Empty(t, hits)
	units, err = store.ResolveGenerationUnits(t.Context(), generation, []db.MessageRef{{SessionID: "s", Ordinal: 0}})
	require.NoError(t, err)
	assert.Equal(t, []db.UnitRef{{}}, units)
}

func TestHostedEmbeddingSearchNormalizesQueryBeforeHalfvec(t *testing.T) {
	f, store, generation := embeddingFixture(t)
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('s','p','m','codex'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('s',0,'user','parallel'),('s',1,'user','opposite')`)
	require.NoError(t, err)
	reconcileEmbedding(t, store)
	leases, err := store.Claim(t.Context(), "worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	snapshot, err := store.ReadSession(t.Context(), leases[0])
	require.NoError(t, err)
	require.NoError(t, store.Publish(t.Context(), snapshot, []HostedEmbeddingVector{
		{DocumentKey: "o:s:0", Values: []float32{1, 0, 0}},
		{DocumentKey: "o:s:1", Values: []float32{-1, 0, 0}},
	}))
	for _, tc := range []struct {
		name  string
		query []float32
	}{
		{"unit", []float32{1, 0, 0}}, {"tiny", []float32{1e-8, 0, 0}}, {"large", []float32{100000, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := store.SearchGeneration(t.Context(), generation, tc.query, 5)
			require.NoError(t, err)
			assert.Equal(t, []db.VectorHit{
				{SessionID: "s", Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 0, Score: 1, Snippet: "parallel"},
				{SessionID: "s", Ordinal: 1, OrdinalStart: 1, OrdinalEnd: 1, Score: -1, Snippet: "opposite"},
			}, hits)
		})
	}
	for _, query := range [][]float32{{1, 0}, {0, 0, 0}, {float32(math.NaN()), 0, 0}, {float32(math.Inf(1)), 0, 0}} {
		_, err := store.SearchGeneration(t.Context(), generation, query, 5)
		assert.ErrorIs(t, err, ErrHostedEmbeddingInvalidResults)
	}
}
