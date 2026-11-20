//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestHostedVectorUploadEditAndSearchSnapshot(t *testing.T) {
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
	upload, receipt := f.accept(t, "device-a", "upload", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, upload), upload, projectionOutcome("old words")))
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	reader, err := OpenHosted(t.Context(), f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reader.Close()) })
	// Search must reuse its snapshot connection through candidate hydration.
	reader.SetMaxOpenConns(1)
	hosted, err := newHostedAdapter(t.Context(), reader, f.tenant)
	require.NoError(t, err)
	searcher := NewVectorSearcher(reader, id, 2, 128, func(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil }).(*vectorSearcher)
	selected := make(chan struct{})
	resume := make(chan struct{})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	hosted.SetVectorSearcher(searcher)
	initial, err := hosted.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "old", Mode: "semantic", IncludeOneShot: true, Limit: 10})
	require.NoError(t, err)
	var initialSnippets []string
	for _, match := range initial.Matches {
		initialSnippets = append(initialSnippets, match.Snippet)
	}
	assert.Contains(t, initialSnippets, "old words")
	first, receipt := f.accept(t, "device-a", "snapshot-first", receipt.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, first), first, projectionOutcome("old snapshot words")))
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET deleted_at=now()`)
	require.NoError(t, err)
	trashed, err := hosted.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "old", Mode: "semantic", IncludeOneShot: true, Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, trashed.Matches)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET deleted_at=NULL`)
	require.NoError(t, err)
	searcher.afterCandidates = func() { close(selected); <-resume }
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
	searcher.afterCandidates = nil
	page, err := hosted.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "new", Mode: "semantic", IncludeOneShot: true, Limit: 10})
	require.NoError(t, err)
	snippets = nil
	for _, match := range page.Matches {
		snippets = append(snippets, match.Snippet)
	}
	assert.Contains(t, snippets, "new snapshot words")
	var old int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_documents WHERE content='old snapshot words'`).Scan(&old))
	assert.Zero(t, old)
}
