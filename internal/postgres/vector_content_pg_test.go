//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
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
			Params: gen.Params, IncludeAutomated: info.IncludeAutomated, DataVersion: info.DataVersion,
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
	for _, tc := range []struct {
		name, mode      string
		seedReplacement bool
	}{
		{"removed after G2 push", "removed", true},
		{"automated scope after G2 push", "automated scope", true},
		{"project scope after G2 push", "project scope", true},
		{"removed before first G2 push", "removed", false},
		{"automated scope before first G2 push", "automated scope", false},
		{"project scope before first G2 push", "project scope", false},
		{"foreign owner before first G2 push", "foreign owner", false},
		{"failed session before first G2 push", "failed session", false},
		{"changed scope retains other session", "changed scope", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if tc.seedReplacement {
				second := builtVectorSource(t, ix, local, "second", true)
				sync.vectorSource = second
				current, err := sync.Push(t.Context(), false, nil)
				require.NoError(t, err)
				require.NotEqual(t, pushed.Vectors.GenerationID, current.Vectors.GenerationID)
			}
			before, err := store.LookupHubVectors(t.Context(), request)
			require.NoError(t, err)
			require.Len(t, before.Chunks, 1)
			switch tc.mode {
			case "removed", "failed session", "foreign owner", "changed scope":
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
			if tc.mode == "foreign owner" {
				_, err := pg.ExecContext(t.Context(), "UPDATE sessions SET owner_marker='another-publisher' WHERE id='A'")
				require.NoError(t, err)
			}
			latest := builtVectorSource(t, ix, local, "second", tc.mode == "changed scope")
			require.Empty(t, latest.hashes)
			sync.vectorSource = latest
			switch tc.mode {
			case "failed session":
				_, err = sync.pushVectors(t.Context(), false, nil, 0, map[string]struct{}{"A": {}}, nil)
			case "changed scope":
				_, err = sync.pushVectors(t.Context(), false, []string{"B"}, 0, nil, nil)
			default:
				_, err = sync.Push(t.Context(), false, nil)
			}
			require.NoError(t, err)
			after, err := store.LookupHubVectors(t.Context(), request)
			require.NoError(t, err)
			var coverage int
			require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_push_state WHERE generation_id=$1 AND session_id='A'`, pushed.Vectors.GenerationID).Scan(&coverage))
			if tc.mode == "removed" {
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

// A parser upgrade without an embedding refresh must retain the mirror's actual
// source version. Legacy mirrors keep pushing without acquiring new hub proof.
func TestVectorPushStaleMirrorParserWitness(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_stale_parser_test")
	seedVectorSession(t, local, "A")
	oldVersion := db.CurrentDataVersion() - 1
	setVersion := func(version int) {
		require.NoError(t, local.Update(t.Context(), func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(t.Context(), "UPDATE sessions SET data_version=?", version); err != nil {
				return err
			}
			_, err := tx.ExecContext(t.Context(), "PRAGMA user_version="+strconv.Itoa(version))
			return err
		}))
	}
	setVersion(oldVersion)
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := vector.Open(t.Context(), path, false, 8192)
	require.NoError(t, err)
	first := builtVectorSource(t, ix, local, "first")
	require.Equal(t, oldVersion, first.gen.DataVersion)
	require.NoError(t, ix.Close())
	setVersion(db.CurrentDataVersion())
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "A", []db.Message{{SessionID: "A", Ordinal: 0, Role: "user", Content: "new parser output", ContentLength: 17}}))
	ix, err = vector.Open(t.Context(), path, true, 8192)
	require.NoError(t, err)
	exp, ok, err := ix.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, oldVersion, exp.Generation().DataVersion)
	docs, _, err := exp.SessionDocs(t.Context(), "A")
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.Equal(t, "A", docs[0].Content)
	require.NoError(t, exp.Close())
	require.NoError(t, ix.Close())
	sync.vectorSource = first
	pushed, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	require.False(t, pushed.Vectors.Skipped)
	var stamped sql.NullInt64
	require.NoError(t, pg.QueryRowContext(t.Context(), "SELECT data_version FROM vector_generation_machines WHERE generation_id=$1", pushed.Vectors.GenerationID).Scan(&stamped))
	assert.Equal(t, int64(oldVersion), stamped.Int64)
	assert.True(t, stamped.Valid)
	store := &Store{pg: pg}
	setHubTestSearcher(t, store, pushed.Vectors.GenerationID)
	discovery, err := store.HubVectorGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, oldVersion, discovery.DataVersion, "discovery reports actual old proof, so current-parser importers reject reuse")
	// A source without proof still publishes to existing readers.
	mirror, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = mirror.ExecContext(t.Context(), "DELETE FROM vector_meta WHERE key='source_data_version'")
	require.NoError(t, err)
	require.NoError(t, mirror.Close())
	legacy, err := vector.Open(t.Context(), path, true, 8192)
	require.NoError(t, err)
	legacyExport, ok, err := legacy.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)
	first.gen.DataVersion = legacyExport.Generation().DataVersion
	require.Zero(t, first.gen.DataVersion)
	require.NoError(t, legacyExport.Close())
	require.NoError(t, legacy.Close())
	_, err = sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	require.NoError(t, pg.QueryRowContext(t.Context(), "SELECT data_version FROM vector_generation_machines WHERE generation_id=$1", pushed.Vectors.GenerationID).Scan(&stamped))
	assert.False(t, stamped.Valid)
	discovery, err = store.HubVectorGeneration(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, discovery.Unavailable)
	assert.Equal(t, 1, countRows(t, pg, "SELECT count(*) FROM "+vectorChunkTable(pushed.Vectors.GenerationID)))
	ix, err = vector.Open(t.Context(), path, false, 8192)
	require.NoError(t, err)
	defer ix.Close()
	sync.vectorSource = builtVectorSource(t, ix, local, "first")
	_, err = sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	discovery, err = store.HubVectorGeneration(t.Context())
	require.NoError(t, err)
	assert.Empty(t, discovery.Unavailable)
	assert.Equal(t, db.CurrentDataVersion(), discovery.DataVersion, "explicit full refresh recovers current source proof")
}
