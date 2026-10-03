package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestNativeDialogueGenerationRejectsOldCorpusAndRebuilds(t *testing.T) {
	archive := dbtest.OpenTestDB(t)
	dbtest.SeedNativeSearch(t, archive)
	ix, err := vector.Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"), false, 4000)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	old := kitvec.Generation{Model: "fake-model", Dimensions: 4, Params: map[string]string{
		"max_input_chars": "4000", "doc_unit_scheme": "run_v1", "chunk_overlap_chars": "600",
	}}
	_, err = ix.Build(t.Context(), testPushUnitSource(), fakePushEncoder(), old, vector.BuildOptions{})
	require.NoError(t, err)
	current := vectorGeneration(config.VectorEmbeddingsConfig{Model: "fake-model", Dimension: 4, MaxInputChars: 4000})
	adapter := newSearcherAdapter(ix, fakePushEncoder(), current)
	_, err = adapter.SemanticSearch(t.Context(), "dialogue", 10)
	require.ErrorIs(t, err, db.ErrSemanticUnavailable, "old vectors must not serve normalized message search")
	var encoded []string
	encoder := func(ctx context.Context, texts []string) ([][]float32, error) {
		encoded = append(encoded, texts...)
		return fakePushEncoder()(ctx, texts)
	}
	result, err := ix.Build(t.Context(), archive, encoder, current, vector.BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"questionneedle", "dialogueneedle sharedneedle [Thinking] literal"}, encoded)
	assert.Equal(t, 3, result.Refresh.Deleted, "full reconciliation evicts old corpus documents")
	active, ok, err := ix.ActiveFingerprint(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, current.Fingerprint(), active)
	hits, err := adapter.SemanticSearch(t.Context(), "dialogue", 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	for _, hit := range hits {
		assert.Equal(t, "native-search", hit.SessionID)
		assert.Contains(t, []int{0, 1}, hit.Ordinal)
		assert.NotContains(t, hit.Snippet, "reasonneedle")
		assert.NotContains(t, hit.Snippet, "commandneedle")
		assert.NotContains(t, hit.Snippet, "resultneedle")
	}
}
