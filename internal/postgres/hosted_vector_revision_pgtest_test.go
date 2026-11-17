//go:build pgtest

package postgres

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/kit/embedclient"
	kitvec "go.kenn.io/kit/vector"
)

func TestHostedVectorRevisionReusesUnchangedDocuments(t *testing.T) {
	f := newHostedFixture(t, "tenant-reuse")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "reuse", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('session','p','m','codex','raw','rev1'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('session',0,'user','stable'),('session',1,'user','old')`)
	require.NoError(t, err)
	var calls []string
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		calls = append(calls, texts...)
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	calls = nil
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET raw_content_revision='rev2' WHERE id='session'; UPDATE messages SET content='new' WHERE session_id='session' AND ordinal=1`)
	require.NoError(t, err)
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	assert.Equal(t, []string{"new"}, calls)
	var contents []string
	rows, err := f.runtime.QueryContext(t.Context(), `SELECT content FROM vector_documents WHERE session_id='session' ORDER BY ordinal`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var content string
		require.NoError(t, rows.Scan(&content))
		contents = append(contents, content)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"stable", "new"}, contents)
}

func TestHostedVectorPermanentRejectionWaitsForChange(t *testing.T) {
	f := newHostedFixture(t, "tenant-rejected")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "reject", Model: "fixture", Dimension: 2}
	next := storage.VectorGenerationInfo{Fingerprint: "reject-next", Model: "fixture", Dimension: 2}
	var id int64
	for _, g := range []storage.VectorGenerationInfo{gen, next} {
		require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, g))
		generationID, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, g.Fingerprint)
		require.NoError(t, err)
		require.True(t, ok)
		_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(generationID)+` TO "`+f.role+`"`)
		require.NoError(t, err)
		if g.Fingerprint == gen.Fingerprint {
			id = generationID
		}
	}
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision,message_count,user_message_count) VALUES('session','p','m','codex','raw','rev1',2,2),('session-later','p','m','codex','raw','rev1',1,1); INSERT INTO messages(session_id,ordinal,role,content) VALUES('session',0,'user','good'),('session',1,'user','poison'),('session-later',0,'user','later')`)
	require.NoError(t, err)
	var calls []string
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		calls = append(calls, texts...)
		if slices.Contains(texts, "poison") {
			return nil, &embedclient.APIError{StatusCode: 400, Reason: embedclient.ReasonInputTooLong}
		}
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false, kitvec.WithBatchSize(8)))
	store := &Store{pg: f.runtime}
	store.SetVectorSearcher(NewVectorSearcher(f.runtime, id, 2, 128, func(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil }))
	page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "search", Mode: "semantic", IncludeOneShot: true, Limit: 10})
	require.NoError(t, err)
	var snippets []string
	for _, match := range page.Matches {
		snippets = append(snippets, match.Snippet)
	}
	assert.ElementsMatch(t, []string{"good", "later"}, snippets)
	calls = nil
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	assert.Empty(t, calls)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET raw_content_revision='rev2' WHERE id='session'`)
	require.NoError(t, err)
	calls = nil
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	assert.Equal(t, []string{"poison"}, calls)
	calls = nil
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, next, encode, 128, 1, false))
	assert.Contains(t, calls, "poison")
}
