package vector

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/embedclient"
	kitvec "go.kenn.io/kit/vector"
)

func TestFillUnitDocumentsBatchesSessionChunks(t *testing.T) {
	units := []db.EmbeddableUnit{
		{SessionID: "session", Kind: "user", Ordinal: 0, OrdinalEnd: 0, SourceUUID: "duplicate", Content: "first"},
		{SessionID: "session", Kind: "user", Ordinal: 1, OrdinalEnd: 1, SourceUUID: "duplicate", Content: "second"},
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
	docs, err := UnitDocuments(units)
	require.NoError(t, err)
	spool, err := NewVectorSpool()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.Close()) })
	docs, err = FillUnitDocuments(t.Context(), docs, spool, 128, 1, encode, kitvec.WithBatchSize(2))
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"first", "second"}}, calls)
	require.Len(t, docs, 2)
	docs[0].Chunks, err = spool.Chunks(docs[0].DocKey)
	require.NoError(t, err)
	docs[1].Chunks, err = spool.Chunks(docs[1].DocKey)
	require.NoError(t, err)
	require.Len(t, docs[0].Chunks, 1)
	require.Len(t, docs[1].Chunks, 1)
	assert.Equal(t, []float32{1, 0}, docs[0].Chunks[0].Embedding)
	assert.Equal(t, []float32{0, 1}, docs[1].Chunks[0].Embedding)
}

func TestFillUnitDocumentsSkipsRejectedInput(t *testing.T) {
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
	docs, err := UnitDocuments(units)
	require.NoError(t, err)
	spool, err := NewVectorSpool()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.Close()) })
	docs, err = FillUnitDocuments(t.Context(), docs, spool, 128, 1, encode, kitvec.WithBatchSize(8))
	require.NoError(t, err)
	require.Len(t, docs, 2)
	assert.Equal(t, "first", docs[0].Content)
	assert.Equal(t, "last", docs[1].Content)
	docs[0].Chunks, err = spool.Chunks(docs[0].DocKey)
	require.NoError(t, err)
	docs[1].Chunks, err = spool.Chunks(docs[1].DocKey)
	require.NoError(t, err)
	assert.Equal(t, []float32{1, 0}, docs[0].Chunks[0].Embedding)
	assert.Equal(t, []float32{1, 0}, docs[1].Chunks[0].Embedding)
}

func TestFillUnitDocumentsRejectsDocumentWithTooManyChunks(t *testing.T) {
	content := strings.Repeat("abcdefghij", 10000)
	units := []db.EmbeddableUnit{
		{SessionID: "session", Kind: "user", Ordinal: 0, Content: content},
		{SessionID: "session", Kind: "user", Ordinal: 1, Content: "later"},
	}
	docs, err := UnitDocuments(units)
	require.NoError(t, err)
	spool, err := NewVectorSpool()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.Close()) })
	var encoded []string
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		encoded = append(encoded, texts...)
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = []float32{1, 0}
		}
		return vectors, nil
	}
	accepted, err := FillUnitDocuments(t.Context(), docs, spool, 20, 1, encode, kitvec.WithBatchSize(32))
	require.NoError(t, err)
	require.Len(t, accepted, 1)
	assert.Equal(t, "later", accepted[0].Content)
	assert.Equal(t, []string{"later"}, encoded)
	assert.Equal(t, content, docs[0].Content)
}

func TestFillUnitDocumentsSpoolsLargeDocumentsBeforeNextPage(t *testing.T) {
	docs, err := UnitDocuments([]db.EmbeddableUnit{
		{SessionID: "session", Kind: "user", Ordinal: 0, Content: strings.Repeat("a", 50000)},
		{SessionID: "session", Kind: "user", Ordinal: 1, Content: strings.Repeat("b", 50000)},
	})
	require.NoError(t, err)
	spool, err := NewVectorSpool()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.Close()) })
	var secondBeforeFirstSaved bool
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		vectors := make([][]float32, len(texts))
		for i, text := range texts {
			if strings.HasPrefix(text, "b") {
				info, err := spool.file.Stat()
				secondBeforeFirstSaved = secondBeforeFirstSaved || err != nil || info.Size() == 0
			}
			vectors[i] = []float32{1, 0}
		}
		return vectors, nil
	}
	accepted, err := FillUnitDocuments(t.Context(), docs, spool, 20, 1, encode, kitvec.WithBatchSize(8192))
	require.NoError(t, err)
	assert.Len(t, accepted, 2)
	assert.False(t, secondBeforeFirstSaved, "second large document encoded before the first was spooled")
}

func TestFillUnitDocumentsSpoolsPagesAndChunks(t *testing.T) {
	var units []db.EmbeddableUnit
	for i := range 140 {
		units = append(units, db.EmbeddableUnit{SessionID: "session", Kind: "user", Ordinal: i, Content: strings.Repeat("abcdefghij", 8)})
	}
	docs, err := UnitDocuments(units)
	require.NoError(t, err)
	spool, err := NewVectorSpool()
	require.NoError(t, err)
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	accepted, err := FillUnitDocuments(t.Context(), docs, spool, 20, 1, encode, kitvec.WithBatchSize(32))
	require.NoError(t, err)
	require.Len(t, accepted, 140)
	for _, doc := range accepted {
		chunks, err := spool.Chunks(doc.DocKey)
		require.NoError(t, err)
		require.Greater(t, len(chunks), 1)
		assert.Equal(t, []float32{1, 0}, chunks[0].Embedding)
	}
	require.NoError(t, spool.Close())
}

func TestFillUnitDocumentsSpoolWriteFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	spool, err := NewVectorSpool()
	require.NoError(t, err)
	require.NoError(t, spool.file.Close())
	docs, err := UnitDocuments([]db.EmbeddableUnit{{SessionID: "session", Kind: "user", Content: "content"}})
	require.NoError(t, err)
	_, err = FillUnitDocuments(t.Context(), docs, spool, 128, 1, func(_ context.Context, texts []string) ([][]float32, error) { return [][]float32{{1, 0}}, nil })
	require.ErrorContains(t, err, "vector spool")
}
