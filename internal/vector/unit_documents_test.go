package vector

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/embedclient"
	kitvec "go.kenn.io/kit/vector"
)

func TestEncodeUnitDocumentsBatchesSessionChunks(t *testing.T) {
	units := []db.EmbeddableUnit{
		{SessionID: "session", Kind: "user", Ordinal: 0, OrdinalEnd: 0, Content: "first"},
		{SessionID: "session", Kind: "user", Ordinal: 1, OrdinalEnd: 1, Content: "second"},
	}
	var calls [][]string
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		calls = append(calls, append([]string(nil), texts...))
		vectors := make([][]float32, len(texts))
		for i, text := range texts {
			switch text {
			case "first":
				vectors[i] = []float32{1, 0}
			case "second":
				vectors[i] = []float32{0, 1}
			}
		}
		return vectors, nil
	}
	docs, err := EncodeUnitDocuments(t.Context(), units, 128, encode, kitvec.WithBatchSize(2))
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"first", "second"}}, calls)
	require.Len(t, docs, 2)
	require.Len(t, docs[0].Chunks, 1)
	require.Len(t, docs[1].Chunks, 1)
	assert.Equal(t, []float32{1, 0}, docs[0].Chunks[0].Embedding)
	assert.Equal(t, []float32{0, 1}, docs[1].Chunks[0].Embedding)
}

func TestEncodeUnitDocumentsSkipsRejectedInput(t *testing.T) {
	units := []db.EmbeddableUnit{
		{SessionID: "session", Kind: "user", Ordinal: 0, Content: "first"},
		{SessionID: "session", Kind: "user", Ordinal: 1, Content: "poison"},
		{SessionID: "session", Kind: "user", Ordinal: 2, Content: "last"},
	}
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		if slices.Contains(texts, "poison") {
			return nil, &embedclient.APIError{StatusCode: 400, Reason: embedclient.ReasonInputTooLong}
		}
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	docs, err := EncodeUnitDocuments(t.Context(), units, 128, encode, kitvec.WithBatchSize(8))
	require.NoError(t, err)
	require.Len(t, docs, 2)
	assert.Equal(t, "first", docs[0].Content)
	assert.Equal(t, "last", docs[1].Content)
	assert.Equal(t, []float32{1, 0}, docs[0].Chunks[0].Embedding)
	assert.Equal(t, []float32{1, 0}, docs[1].Chunks[0].Embedding)
}
