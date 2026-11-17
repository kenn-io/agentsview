//go:build pgtest

package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestHostedVectorUploadAndEdit(t *testing.T) {
	f := newProjectionFixture(t)
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "hosted-test", Model: "fixture", Dimension: 2, Params: map[string]string{"test": "fixture"}}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, s := range texts {
			if strings.Contains(s, "new words") {
				out[i] = []float32{0, 1}
			} else {
				out[i] = []float32{1, 0}
			}
		}
		return out, nil
	}
	first, receipt := f.accept(t, "device-a", "first", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, first), first, projectionOutcome("old words")))
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	hosted, err := newHostedAdapter(t.Context(), f.runtime, f.tenant)
	require.NoError(t, err)
	hosted.SetVectorSearcher(NewVectorSearcher(f.runtime, id, 2, 128, func(ctx context.Context, text string) ([]float32, error) {
		values, err := encode(ctx, []string{text})
		if err != nil {
			return nil, err
		}
		return values[0], nil
	}))
	page, err := hosted.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "old words", Mode: "semantic", IncludeOneShot: true, Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, page.Matches)
	assert.Equal(t, "codex:portable", page.Matches[0].SessionID)
	second, _ := f.accept(t, "device-a", "second", receipt.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, second), second, projectionOutcome("new words")))
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	page, err = hosted.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "new words", Mode: "semantic", IncludeOneShot: true, Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, page.Matches)
	assert.Equal(t, "codex:portable", page.Matches[0].SessionID)
	var old int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_documents WHERE content='old words'`).Scan(&old))
	assert.Zero(t, old)
}

func TestHostedVectorSearchSnapshotDuringEdit(t *testing.T) {
	f := newProjectionFixture(t)
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "snapshot-fixture", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	first, receipt := f.accept(t, "device-a", "snapshot-first", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, first), first, projectionOutcome("old snapshot words")))
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	hosted, err := newHostedAdapter(t.Context(), f.runtime, f.tenant)
	require.NoError(t, err)
	searcher := NewVectorSearcher(f.runtime, id, 2, 128, func(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil }).(*vectorSearcher)
	selected := make(chan struct{})
	resume := make(chan struct{})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	searcher.afterCandidates = func() { close(selected); <-resume }
	hosted.SetVectorSearcher(searcher)
	type result struct {
		page db.ContentSearchPage
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		page, err := hosted.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "old", Mode: "semantic", IncludeOneShot: true, Limit: 10})
		resultCh <- result{page, err}
	}()
	select {
	case <-selected:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	second, _ := f.accept(t, "device-a", "snapshot-second", receipt.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, second), second, projectionOutcome("new snapshot words")))
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	close(resume)
	res := <-resultCh
	require.NoError(t, res.err)
	var snippets []string
	for _, match := range res.page.Matches {
		snippets = append(snippets, match.Snippet)
	}
	assert.Contains(t, snippets, "old snapshot words")
}
