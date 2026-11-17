//go:build pgtest

package postgres

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/kit/embedclient"
	kitvec "go.kenn.io/kit/vector"
)

func TestHostedVectorUploadAndEdit(t *testing.T) {
	f := newProjectionFixture(t)
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "hosted-test", Model: "fixture", Dimension: 2, Params: map[string]string{"test": "fixture"}}
	require.ErrorContains(t, CheckHostedVectorPrivileges(t.Context(), f.runtime, f.schema, gen.Fingerprint), "hosted-provision")
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	require.ErrorContains(t, CheckHostedVectorPrivileges(t.Context(), f.runtime, f.schema, gen.Fingerprint), vectorChunkTable(id))
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	oldGen := storage.VectorGenerationInfo{Fingerprint: "old-hosted-test", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, oldGen))
	oldID, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, oldGen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT DELETE ON `+vectorChunkTable(oldID)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	require.ErrorContains(t, CheckHostedVectorPrivileges(t.Context(), f.runtime, f.schema, gen.Fingerprint), vectorChunkTable(oldID))
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT ON `+vectorChunkTable(oldID)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO vector_push_state(generation_id,session_id,doc_agg_hash) VALUES($1,'removed-session','old')`, oldID)
	require.NoError(t, err)
	require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	require.NoError(t, CheckHostedVectorPrivileges(t.Context(), f.runtime, f.schema, gen.Fingerprint))
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
	var orphanCount int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_push_state WHERE session_id='removed-session'`).Scan(&orphanCount))
	assert.Zero(t, orphanCount)
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
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	var retained int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_documents WHERE content='new words'`).Scan(&retained))
	assert.Equal(t, 1, retained)
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

func TestHostedVectorPartialSearch(t *testing.T) {
	f := newProjectionFixture(t)
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "partial-search", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	first, _ := f.accept(t, "device-a", "partial-search", "")
	parsed := projectionOutcome("first searchable message")
	parsed.Outcome.Results[0].Result.Messages = []parser.ParsedMessage{
		{Ordinal: 0, Role: parser.RoleUser, Content: "first searchable message"},
		{Ordinal: 1, Role: parser.RoleUser, Content: "rejected message"},
		{Ordinal: 2, Role: parser.RoleUser, Content: "last searchable message"},
	}
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, first), first, parsed))
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET deleted_at=now() WHERE provenance_kind='raw'`)
	require.NoError(t, err)
	reject := true
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		if reject && slices.Contains(texts, "rejected message") {
			return nil, &embedclient.APIError{StatusCode: 400, Reason: embedclient.ReasonInputTooLong}
		}
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	require.Error(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false, kitvec.WithBatchSize(8)))
	search := func() []string {
		h, err := newHostedAdapter(t.Context(), f.runtime, f.tenant)
		require.NoError(t, err)
		h.SetVectorSearcher(NewVectorSearcher(f.runtime, id, 2, 128, func(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil }))
		page, err := h.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "search", Mode: "semantic", IncludeOneShot: true, Limit: 10})
		require.NoError(t, err)
		var snippets []string
		for _, match := range page.Matches {
			snippets = append(snippets, match.Snippet)
		}
		return snippets
	}
	assert.Empty(t, search())
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET deleted_at=NULL WHERE provenance_kind='raw'`)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"first searchable message", "last searchable message"}, search())
	reject = false
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false, kitvec.WithBatchSize(8)))
	assert.ElementsMatch(t, []string{"first searchable message", "rejected message", "last searchable message"}, search())
}
