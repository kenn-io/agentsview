//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// searchMaxInputChars is the chunk size the searcher re-splits content with to
// resolve anchors/snippets. It is small so the run fixture's short content
// re-splits into multiple chunks (see runContent), exercising member anchoring.
const searchMaxInputChars = 20

// fixedEncoder returns a QueryEncodeFunc that always emits vec, ignoring the
// query text: the fixture's chunk vectors are chosen so a fixed query yields a
// deterministic cosine ranking.
func fixedEncoder(vec []float32) QueryEncodeFunc {
	return func(context.Context, string) ([]float32, error) { return vec, nil }
}

// newVectorSearchTestPG drops and recreates the schema with the base vector
// tables and skips when pgvector is unavailable.
func newVectorSearchTestPG(t *testing.T, pgURL, schema string) *sql.DB {
	t.Helper()
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = pg.Close() })

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	unavailable, err := ensureVectorBaseSchemaPG(ctx, pg)
	require.NoError(t, err, "ensureVectorBaseSchemaPG")
	if unavailable != "" {
		t.Skip(unavailable)
	}
	return pg
}

func insertSearchDoc(
	t *testing.T, pg *sql.DB,
	docKey, sessionID string, ordinal, ordinalEnd int,
	offsets, content string,
) {
	t.Helper()
	_, err := pg.Exec(`
INSERT INTO vector_documents (
    doc_key, session_id, source_uuid, ordinal, ordinal_end,
    subordinate, offsets, content, content_hash)
VALUES ($1, $2, '', $3, $4, FALSE, $5, $6, 'h')`,
		docKey, sessionID, ordinal, ordinalEnd, offsets, content)
	require.NoError(t, err, "insert doc "+docKey)
}

func insertSearchChunk(
	t *testing.T, pg *sql.DB, table, halfvecType, docKey string,
	chunkIndex int, emb []float32,
) {
	t.Helper()
	literal, err := halfvecLiteral(emb)
	require.NoError(t, err, "halfvecLiteral "+docKey)
	_, err = pg.Exec(
		`INSERT INTO `+table+` (doc_key, chunk_index, embedding)
		 VALUES ($1, $2, $3::`+halfvecType+`)`,
		docKey, chunkIndex, literal)
	require.NoError(t, err, "insert chunk "+docKey)
}

// seedVectorSearchFixture builds a deterministic KNN fixture for query
// [1,0,0,0]. Cosine similarity ignores magnitude, so the chunk directions are
// spread across the x/y plane to give distinct, unambiguous scores by doc's
// best chunk:
//
//	u1   [1,0,0,0]           1.000  user doc, ordinal 5
//	r1   [1,0.2,0,0]         0.981  run doc chunk 1 (chunk 0 is [0,0,1,0] -> 0)
//	tomb [0.95,0.3122,0,0]   0.950  parked at negative ordinal (tombstone)
//	gone [0.9,0.4359,0,0]    0.900  doc row deleted, chunk kept
//	u2   [0,1,0,0]           0.000  user doc, ordinal 20
//
// tomb and gone must be dropped during hydration, leaving [u1, r1, u2].
func seedVectorSearchFixture(t *testing.T, pg *sql.DB) (genID int64, table string) {
	t.Helper()
	ctx := context.Background()
	genID, err := ensureVectorGeneration(ctx, pg, "fp-search", "m", 4)
	require.NoError(t, err, "ensureVectorGeneration")
	require.NoError(t, ensureVectorChunkTable(ctx, pg, genID, 4), "ensureVectorChunkTable")
	extSchema, err := vectorExtensionSchema(ctx, pg)
	require.NoError(t, err, "vectorExtensionSchema")
	halfvec := extSchema + ".halfvec"
	table = vectorChunkTable(genID)

	insertSearchDoc(t, pg, "u1", "S", 5, 5, "[]", "alpha user content")
	insertSearchDoc(t, pg, "r1", "S", 10, 12, runOffsets, runContent)
	insertSearchDoc(t, pg, "u2", "S", 20, 20, "[]", "gamma")
	insertSearchDoc(t, pg, "gone", "S", 30, 30, "[]", "vanished")
	insertSearchDoc(t, pg, "tomb", "S", -1, -1, "[]", "parked tombstone")

	insertSearchChunk(t, pg, table, halfvec, "u1", 0, []float32{1, 0, 0, 0})
	insertSearchChunk(t, pg, table, halfvec, "r1", 0, []float32{0, 0, 1, 0})
	insertSearchChunk(t, pg, table, halfvec, "r1", 1, []float32{1, 0.2, 0, 0})
	insertSearchChunk(t, pg, table, halfvec, "u2", 0, []float32{0, 1, 0, 0})
	insertSearchChunk(t, pg, table, halfvec, "gone", 0, []float32{0.9, 0.4359, 0, 0})
	insertSearchChunk(t, pg, table, halfvec, "tomb", 0, []float32{0.95, 0.3122, 0, 0})

	// gone's chunk stays; its document row disappears between KNN and hydrate.
	_, err = pg.Exec(`DELETE FROM vector_documents WHERE doc_key = 'gone'`)
	require.NoError(t, err, "delete gone doc")
	return genID, table
}

// runContent joins two run members with "\n\n"; runOffsets records their rune
// starts (ordinals 10 and 12). With searchMaxInputChars=20 the content
// re-splits so chunk index 1's center falls inside member B (ordinal 12).
var (
	runMemberA = strings.Repeat("a", 17)
	runMemberB = strings.Repeat("b", 17)
	runContent = runMemberA + "\n\n" + runMemberB
	runOffsets = `[{"o":10,"r":0,"b":0},{"o":12,"r":19,"b":19}]`
)

func TestPGVectorSearcher(t *testing.T) {
	pgURL := testPGURL(t)
	pg := newVectorSearchTestPG(t, pgURL, "agentsview_vector_search_test")
	ctx := context.Background()

	genID, _ := seedVectorSearchFixture(t, pg)
	searcher := NewVectorSearcher(
		pg, genID, 4, searchMaxInputChars, fixedEncoder([]float32{1, 0, 0, 0}))

	t.Run("ranking rollup hydration tombstone", func(t *testing.T) {
		hits, err := searcher.SemanticSearch(ctx, "query", 10)
		require.NoError(t, err, "SemanticSearch")

		keys := make([]string, len(hits))
		for i, h := range hits {
			keys[i] = h.SessionID
		}
		require.Len(t, hits, 3, "gone and tomb dropped, r1 rolled to one hit")

		// Ranking order: u1 (1.0) > r1 (0.992) > u2 (0.0).
		assert.Equal(t, 5, hits[0].Ordinal, "u1 first")
		assert.Equal(t, "alpha user content", hits[0].Snippet)
		assert.InDelta(t, 1.0, hits[0].Score, 1e-3, "u1 cosine ~1.0")
		assert.Greater(t, hits[0].Score, hits[1].Score, "scores descend")
		assert.Greater(t, hits[1].Score, hits[2].Score, "scores descend")

		// r1 is the run doc: anchor to member B (ordinal 12), member-local snippet.
		r1 := hits[1]
		assert.Equal(t, 12, r1.Ordinal, "run hit anchors to member B")
		assert.Equal(t, 10, r1.OrdinalStart)
		assert.Equal(t, 12, r1.OrdinalEnd)
		assert.Equal(t, runMemberB, r1.Snippet, "member-local snippet")
		assert.False(t, r1.Subordinate)

		// u2 last; tombstone and vanished doc absent.
		assert.Equal(t, 20, hits[2].Ordinal)
		for _, h := range hits {
			assert.GreaterOrEqual(t, h.Ordinal, 0, "no negative-ordinal tombstone hydrated")
		}
	})

	t.Run("limit truncates documents", func(t *testing.T) {
		hits, err := searcher.SemanticSearch(ctx, "query", 1)
		require.NoError(t, err)
		require.Len(t, hits, 1)
		assert.Equal(t, 5, hits[0].Ordinal, "only top-ranked u1")
	})

	t.Run("encoder error is transient", func(t *testing.T) {
		bad := NewVectorSearcher(pg, genID, 4, searchMaxInputChars,
			func(context.Context, string) ([]float32, error) {
				return nil, errors.New("embeddings endpoint down")
			})
		_, err := bad.SemanticSearch(ctx, "query", 5)
		require.Error(t, err)
		assert.True(t, errors.Is(err, db.ErrSemanticTransient),
			"encoder failure surfaces db.ErrSemanticTransient")
	})

	t.Run("dimension mismatch is rejected", func(t *testing.T) {
		wrongDim := NewVectorSearcher(pg, genID, 4, searchMaxInputChars,
			fixedEncoder([]float32{1, 0, 0}))
		_, err := wrongDim.SemanticSearch(ctx, "query", 5)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dimensions")
	})

	t.Run("resolve message units", func(t *testing.T) {
		tests := []struct {
			name      string
			ordinal   int
			wantDoc   string // "" means zero UnitRef
			wantStart int
			wantEnd   int
		}{
			{"inside unit", 11, "r1", 10, 12},
			{"exact ordinal_end boundary", 12, "r1", 10, 12},
			{"gap between units", 15, "", 0, 0},
			{"before first unit", 2, "", 0, 0},
			{"tombstone guard", 3, "", 0, 0}, // without ordinal>=0, tomb(-1) matches
			{"user unit", 20, "u2", 20, 20},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				units, err := searcher.ResolveMessageUnits(ctx,
					[]db.MessageRef{{SessionID: "S", Ordinal: tt.ordinal}})
				require.NoError(t, err)
				require.Len(t, units, 1)
				u := units[0]
				assert.Equal(t, tt.wantDoc, u.DocKey)
				if tt.wantDoc == "" {
					assert.Equal(t, db.UnitRef{}, u, "zero UnitRef")
					return
				}
				assert.Equal(t, "S", u.SessionID)
				assert.Equal(t, tt.wantStart, u.OrdinalStart)
				assert.Equal(t, tt.wantEnd, u.OrdinalEnd)
			})
		}
	})

	t.Run("resolve preserves ref order and handles empty", func(t *testing.T) {
		empty, err := searcher.ResolveMessageUnits(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, empty)

		units, err := searcher.ResolveMessageUnits(ctx, []db.MessageRef{
			{SessionID: "S", Ordinal: 15}, // gap -> zero
			{SessionID: "S", Ordinal: 11}, // r1
		})
		require.NoError(t, err)
		require.Len(t, units, 2)
		assert.Equal(t, db.UnitRef{}, units[0])
		assert.Equal(t, "r1", units[1].DocKey)
	})
}

// TestPGVectorSearcherExactK pins the exact-k chunk fetch: a single doc whose
// chunks occupy all top-k chunk ranks rolls up to one hit, and a next-best doc
// whose only chunk sits at rank k+1 never enters the result. Over-fetching more
// than k chunks would surface the outside-k doc, so this guards the LIMIT k
// candidate pool that keeps PG parity with the local sqlite-vec searcher.
func TestPGVectorSearcherExactK(t *testing.T) {
	pgURL := testPGURL(t)
	pg := newVectorSearchTestPG(t, pgURL, "agentsview_vector_search_exactk_test")
	ctx := context.Background()

	genID, err := ensureVectorGeneration(ctx, pg, "fp-exactk", "m", 4)
	require.NoError(t, err, "ensureVectorGeneration")
	require.NoError(t, ensureVectorChunkTable(ctx, pg, genID, 4), "ensureVectorChunkTable")
	extSchema, err := vectorExtensionSchema(ctx, pg)
	require.NoError(t, err, "vectorExtensionSchema")
	halfvec := extSchema + ".halfvec"
	table := vectorChunkTable(genID)

	// "multi" contributes the two nearest chunks (ranks 1 and 2 for query
	// [1,0,0,0]); "outside" is next-best at rank 3, beyond k=2.
	insertSearchDoc(t, pg, "multi", "S", 1, 1, "[]", "multi doc content")
	insertSearchDoc(t, pg, "outside", "S", 2, 2, "[]", "outside doc content")
	insertSearchChunk(t, pg, table, halfvec, "multi", 0, []float32{1, 0, 0, 0})
	insertSearchChunk(t, pg, table, halfvec, "multi", 1, []float32{1, 0.1, 0, 0})
	insertSearchChunk(t, pg, table, halfvec, "outside", 0, []float32{0, 1, 0, 0})

	searcher := NewVectorSearcher(
		pg, genID, 4, searchMaxInputChars, fixedEncoder([]float32{1, 0, 0, 0}))
	hits, err := searcher.SemanticSearch(ctx, "query", 2)
	require.NoError(t, err, "SemanticSearch")
	require.Len(t, hits, 1, "both top-2 chunks belong to multi -> one rolled-up doc")
	assert.Equal(t, 1, hits[0].Ordinal, "only multi survives; outside sits beyond k")
}

// TestPGVectorSearcherEfSearchRecall pins the HNSW recall contract: with a
// candidate pool larger than pgvector's default hnsw.ef_search (40), a search
// for k neighbors must return k docs, not ~40. The searcher raises ef_search to
// k inside the query transaction; without that the KNN silently caps the pool
// below k, diverging from the local backend's exact scan.
func TestPGVectorSearcherEfSearchRecall(t *testing.T) {
	pgURL := testPGURL(t)
	pg := newVectorSearchTestPG(t, pgURL, "agentsview_vector_search_efsearch_test")
	ctx := context.Background()

	genID, err := ensureVectorGeneration(ctx, pg, "fp-efsearch", "m", 4)
	require.NoError(t, err, "ensureVectorGeneration")
	require.NoError(t, ensureVectorChunkTable(ctx, pg, genID, 4), "ensureVectorChunkTable")
	extSchema, err := vectorExtensionSchema(ctx, pg)
	require.NoError(t, err, "vectorExtensionSchema")
	halfvec := extSchema + ".halfvec"
	table := vectorChunkTable(genID)

	// 100 distinct docs (> the ef_search default of 40), one chunk each. The
	// vectors spread across the plane so all are neighbors of the query.
	const docCount = 100
	for i := 0; i < docCount; i++ {
		key := fmt.Sprintf("d%d", i)
		insertSearchDoc(t, pg, key, "S", i, i, "[]", fmt.Sprintf("doc %d", i))
		emb := []float32{1, float32(i) / float32(docCount), 0, 0}
		insertSearchChunk(t, pg, table, halfvec, key, 0, emb)
	}

	searcher := NewVectorSearcher(
		pg, genID, 4, searchMaxInputChars, fixedEncoder([]float32{1, 0, 0, 0}))
	hits, err := searcher.SemanticSearch(ctx, "query", docCount)
	require.NoError(t, err, "SemanticSearch")
	assert.Len(t, hits, docCount,
		"ef_search raised to k returns all %d docs, not the ~40 default cap", docCount)
}
