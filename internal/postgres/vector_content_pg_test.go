//go:build pgtest

package postgres

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// Populate the push seam from a real local build and export, including the
// production aggregate hash, content hash, DocKey and offsets encoding.
func builtVectorSource(t *testing.T, ix *vector.Index, units vector.UnitSource, model string, includeAutomated ...bool) *fakeVectorSource {
	t.Helper()
	all := len(includeAutomated) > 0 && includeAutomated[0]
	gen := kitvec.Generation{Model: model, Dimensions: 4, Params: map[string]string{"max_input_chars": "8192"}}
	_, err := ix.Build(t.Context(), units, func(_ context.Context, texts []string) ([][]float32, error) {
		result := make([][]float32, len(texts))
		for i := range result {
			result[i] = []float32{1, 0, 0, 0}
		}
		return result, nil
	}, gen, vector.BuildOptions{Backstop: true, IncludeAutomated: all})
	require.NoError(t, err)
	exp, ok, err := ix.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)
	defer exp.Close()
	hashes, err := exp.SessionDocHashes(t.Context(), nil)
	require.NoError(t, err)
	docs, _, err := exp.SessionDocs(t.Context(), "A")
	require.NoError(t, err)
	info := exp.Generation()
	source := &fakeVectorSource{
		hasGen: true, hashes: hashes, docs: map[string][]storage.VectorPushDoc{},
		gen: storage.VectorGenerationInfo{
			Fingerprint: info.Fingerprint, Model: info.Model, Dimension: info.Dimension,
			Params: gen.Params, IncludeAutomated: info.IncludeAutomated,
		},
	}
	for _, d := range docs {
		out := storage.VectorPushDoc{DocKey: d.DocKey, SessionID: d.SessionID, SourceUUID: d.SourceUUID, Ordinal: d.Ordinal, OrdinalEnd: d.OrdinalEnd, Subordinate: d.Subordinate, OffsetsJSON: d.OffsetsJSON, Content: d.Content, ContentHash: d.ContentHash}
		for _, c := range d.Chunks {
			out.Chunks = append(out.Chunks, storage.VectorPushChunk{ChunkIndex: c.ChunkIndex, Embedding: c.Embedding})
		}
		source.docs["A"] = append(source.docs["A"], out)
	}
	return source
}

func TestVectorPushPinnedGenerationContentCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rewrite    []db.Message
		compatible bool
	}{
		{"ordinal and run offsets", []db.Message{
			{SessionID: "A", Ordinal: 1, Role: "assistant", Content: "first", ContentLength: 5, SourceUUID: "uuid"},
			{SessionID: "A", Ordinal: 2, Role: "assistant", Content: "second", ContentLength: 6, SourceUUID: "next"},
		}, true},
		{"content", []db.Message{
			{SessionID: "A", Ordinal: 0, Role: "assistant", Content: "changed", ContentLength: 7, SourceUUID: "uuid"},
			{SessionID: "A", Ordinal: 1, Role: "assistant", Content: "second", ContentLength: 6, SourceUUID: "next"},
		}, false},
		{"key", []db.Message{
			{SessionID: "A", Ordinal: 0, Role: "assistant", Content: "first", ContentLength: 5, SourceUUID: "replacement"},
			{SessionID: "A", Ordinal: 1, Role: "assistant", Content: "second", ContentLength: 6, SourceUUID: "next"},
		}, false},
		{"deleted message", []db.Message{
			{SessionID: "A", Ordinal: 0, Role: "assistant", Content: "first", ContentLength: 5, SourceUUID: "uuid"},
		}, false},
		{"deleted document", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_content_2416_test")
			seedVectorSession(t, local, "A")
			kept := db.Message{SessionID: "A", Ordinal: 4, Role: "user", SourceUUID: "kept", Content: "keep", ContentLength: 4}
			ix, err := vector.Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"), false, 8192)
			require.NoError(t, err)
			defer ix.Close()
			require.NoError(t, local.ReplaceSessionMessages(t.Context(), "A", []db.Message{
				{SessionID: "A", Ordinal: 0, Role: "assistant", Content: "first", ContentLength: 5, SourceUUID: "uuid"},
				{SessionID: "A", Ordinal: 1, Role: "assistant", Content: "second", ContentLength: 6, SourceUUID: "next"},
				kept,
			}))
			first := builtVectorSource(t, ix, local, "first")
			require.Len(t, first.docs["A"], 2)
			sync.vectorSource = first
			pushed, err := sync.Push(t.Context(), false, nil)
			require.NoError(t, err)
			store := &Store{pg: pg}
			setHubTestSearcher(t, store, pushed.Vectors.GenerationID)
			request := vector.HubLookupRequest{Fingerprint: first.gen.Fingerprint, Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "run", SourceUUID: "uuid", Occurrence: 1, ContentHash: first.docs["A"][0].ContentHash}}}
			before, err := store.LookupHubVectors(t.Context(), request)
			require.NoError(t, err)
			require.Len(t, before.Chunks, 1)
			require.NoError(t, local.ReplaceSessionMessages(t.Context(), "A", append(tc.rewrite, kept)))
			second := builtVectorSource(t, ix, local, "second")
			require.NotEqual(t, first.hashes["A"], second.hashes["A"])
			sync.vectorSource = second
			_, err = sync.Push(t.Context(), false, nil)
			require.NoError(t, err)
			after, err := store.LookupHubVectors(t.Context(), request)
			require.NoError(t, err)
			if tc.compatible {
				require.Equal(t, before.Chunks, after.Chunks, "same key and content retain pinned vectors")
				require.Empty(t, after.Absent)
				var ordinal, end int
				var offsets, coverage string
				require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT ordinal,ordinal_end,offsets FROM vector_documents WHERE doc_key=$1`, first.docs["A"][0].DocKey).Scan(&ordinal, &end, &offsets))
				require.Equal(t, 1, ordinal)
				require.Equal(t, 2, end)
				require.JSONEq(t, `[{"o":1,"r":0,"b":0},{"o":2,"r":7,"b":7}]`, offsets)
				require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT doc_agg_hash FROM vector_push_state WHERE generation_id=$1 AND session_id='A'`, pushed.Vectors.GenerationID).Scan(&coverage))
				require.Equal(t, second.hashes["A"], coverage)
			} else {
				require.Empty(t, after.Chunks)
				require.Equal(t, []int{0}, after.Absent)
				require.Zero(t, countRows(t, pg, `SELECT count(*) FROM `+vectorChunkTable(pushed.Vectors.GenerationID)))
			}
		})
	}
}

// Empty hash maps can mean removal or an intentional build/push scope change.
// Only real removal may invalidate the other generations' retained history.
func TestVectorPushLastDocumentRemoval(t *testing.T) {
	for _, mode := range []string{"removed", "automated scope", "project scope"} {
		t.Run(mode, func(t *testing.T) {
			sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_empty_test")
			seedVectorSession(t, local, "A")
			ix, err := vector.Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"), false, 8192)
			require.NoError(t, err)
			defer ix.Close()
			first := builtVectorSource(t, ix, local, "first", true)
			require.Len(t, first.docs["A"], 1)
			sync.vectorSource = first
			pushed, err := sync.Push(t.Context(), false, nil)
			require.NoError(t, err)
			store := &Store{pg: pg}
			setHubTestSearcher(t, store, pushed.Vectors.GenerationID)
			request := vector.HubLookupRequest{Fingerprint: first.gen.Fingerprint, Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "user", SourceUUID: first.docs["A"][0].SourceUUID, Occurrence: 1, ContentHash: first.docs["A"][0].ContentHash}}}
			second := builtVectorSource(t, ix, local, "second", true)
			sync.vectorSource = second
			current, err := sync.Push(t.Context(), false, nil)
			require.NoError(t, err)
			require.NotEqual(t, pushed.Vectors.GenerationID, current.Vectors.GenerationID)
			before, err := store.LookupHubVectors(t.Context(), request)
			require.NoError(t, err)
			require.Len(t, before.Chunks, 1)
			switch mode {
			case "removed":
				require.NoError(t, local.ReplaceSessionMessages(t.Context(), "A", nil))
			case "automated scope":
				session, err := local.GetSession(t.Context(), "A")
				require.NoError(t, err)
				require.NotNil(t, session)
				session.IsAutomated = true
				require.NoError(t, local.UpsertSession(t.Context(), *session))
			case "project scope":
				require.NoError(t, local.ReplaceSessionMessages(t.Context(), "A", nil))
				sync.excludeProjects = []string{"proj"}
			}
			latest := builtVectorSource(t, ix, local, "second")
			require.Empty(t, latest.hashes)
			sync.vectorSource = latest
			_, err = sync.Push(t.Context(), false, nil)
			require.NoError(t, err)
			after, err := store.LookupHubVectors(t.Context(), request)
			require.NoError(t, err)
			var coverage int
			require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_push_state WHERE generation_id=$1 AND session_id='A'`, pushed.Vectors.GenerationID).Scan(&coverage))
			if mode == "removed" {
				assert.Empty(t, after.Chunks)
				assert.Equal(t, []int{0}, after.Absent)
				assert.Zero(t, coverage)
				assert.Zero(t, countRows(t, pg, `SELECT count(*) FROM `+vectorChunkTable(pushed.Vectors.GenerationID)))
				assert.Zero(t, countRows(t, pg, `SELECT count(*) FROM vector_push_state WHERE session_id='A'`))
				assert.Zero(t, countRows(t, pg, `SELECT count(*) FROM vector_documents WHERE session_id='A'`))
			} else {
				assert.Equal(t, before.Chunks, after.Chunks)
				assert.Empty(t, after.Absent)
				assert.Equal(t, 1, coverage)
			}
		})
	}
}
