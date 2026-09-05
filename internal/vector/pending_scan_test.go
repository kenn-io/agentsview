package vector

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	kitvec "go.kenn.io/kit/vector"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// explainVectorPlan returns the flattened EXPLAIN QUERY PLAN detail lines
// for query.
func explainVectorPlan(t *testing.T, ix *Index, query string, args ...any) []string {
	t.Helper()
	rows, err := ix.db.QueryContext(
		context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, details)
	return details
}

// TestMirrorRevisionIndexExists asserts every store's mirror carries the
// (doc_key, content_hash) index the stamp anti-joins depend on. The index is
// additive, so an existing vectors.db picks it up from MirrorDDL on its next
// write-path open rather than through a MirrorSchemaVersion bump.
func TestMirrorRevisionIndexExists(t *testing.T) {
	tests := []struct {
		name  string
		spec  IndexSpec
		index string
	}{
		{"messages", MessageIndexSpec(), "idx_vector_messages_revision"},
		{"recall", RecallIndexSpec(), "idx_vector_recall_entries_revision"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ix, err := OpenSpec(context.Background(),
				filepath.Join(t.TempDir(), "vectors.db"), tt.spec, false, 4000)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, ix.Close()) })

			var sql string
			require.NoError(t, ix.db.QueryRow(
				`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`,
				tt.index).Scan(&sql))
			assert.Contains(t, sql, tt.spec.DocsTable)
			assert.Contains(t, strings.ReplaceAll(sql, " ", ""), "(doc_key,content_hash)")
		})
	}
}

// TestPendingContentQueryPlanSkipsStampedDocumentContent is the regression
// guard for countPending reading every mirror document's content on every
// after-sync refresh. The pending set must resolve through the mirror's
// covering index, and content must only be read per pending doc_key.
func TestPendingContentQueryPlanSkipsStampedDocumentContent(t *testing.T) {
	ix, gen := builtPendingIndex(t)
	ordinal, err := ix.ordinalForFingerprint(context.Background(), gen.Fingerprint())
	require.NoError(t, err)

	plan := explainVectorPlan(t, ix, ix.pendingContentQuery(), ordinal)
	joined := strings.Join(plan, "\n")

	assert.Contains(t, joined,
		"SCAN d USING COVERING INDEX idx_vector_messages_revision",
		"the pending set must come from the covering index:\n%s", joined)
	assert.Contains(t, joined,
		"SEARCH d USING INDEX sqlite_autoindex_vector_messages_1 (doc_key=?)",
		"content must be fetched per pending doc_key:\n%s", joined)
	for _, line := range plan {
		assert.NotEqual(t, "SCAN d", line,
			"no step may read every mirror row's content:\n%s", joined)
	}
}

// TestGenerationCoverageQueryPlanUsesRevisionIndex asserts the coverage
// query's Embedded and Missing anti-joins are answered from the same
// covering index, so `embeddings status` does not walk the whole mirror
// either.
func TestGenerationCoverageQueryPlanUsesRevisionIndex(t *testing.T) {
	ix, _ := builtPendingIndex(t)

	plan := explainVectorPlan(t, ix, ix.generationCoverageQuery())
	joined := strings.Join(plan, "\n")

	assert.Contains(t, joined,
		"SEARCH d EXISTS USING COVERING INDEX idx_vector_messages_revision",
		"the Embedded column must probe the covering index:\n%s", joined)
	assert.Contains(t, joined,
		"SCAN d USING COVERING INDEX idx_vector_messages_revision",
		"the Missing column must scan the covering index, not the table:\n%s", joined)
	for _, line := range plan {
		assert.NotEqual(t, "SCAN d", line,
			"no coverage step may read every mirror row's content:\n%s", joined)
	}
}

// TestCountPendingMatchesCoverageMissing pins countPending's chunk
// denominator against the same stamp anti-join `embeddings status` reports
// as Missing, across the states a refresh can leave a mirror in.
func TestCountPendingMatchesCoverageMissing(t *testing.T) {
	ctx := context.Background()
	longContent := strings.Repeat("word ", 2000)

	tests := []struct {
		name string
		// mutate leaves the built mirror in the state under test.
		mutate func(t *testing.T, ix *Index)
		// wantChunks is the chunk denominator countPending must report.
		wantChunks func(t *testing.T, ix *Index) int64
		// wantMissing is the document count `embeddings status` must
		// report for the same anti-join.
		wantMissing int64
	}{
		{
			name:        "NothingPendingAfterBuild",
			mutate:      func(*testing.T, *Index) {},
			wantChunks:  func(*testing.T, *Index) int64 { return 0 },
			wantMissing: 0,
		},
		{
			name: "StaleRevisionCountsAsPending",
			mutate: func(t *testing.T, ix *Index) {
				_, err := ix.db.ExecContext(ctx,
					`UPDATE vector_messages SET content_hash = 'changed'
					 WHERE doc_key = 'u:s1:u1'`)
				require.NoError(t, err)
			},
			wantChunks:  func(*testing.T, *Index) int64 { return 1 },
			wantMissing: 1,
		},
		{
			name: "UnstampedMultiChunkDocumentCountsEveryChunk",
			mutate: func(t *testing.T, ix *Index) {
				_, err := ix.db.ExecContext(ctx, `
INSERT INTO vector_messages
    (doc_key, session_id, source_uuid, ordinal, ordinal_end, content, content_hash)
VALUES ('u:s1:u3', 's1', 'u3', 9, 9, ?, 'hash-u3')`, longContent)
				require.NoError(t, err)
			},
			wantChunks: func(t *testing.T, ix *Index) int64 {
				chunks := int64(len(kitvec.Split(longContent, ix.split)))
				require.Greater(t, chunks, int64(1),
					"content must split into several chunks for this case to be meaningful")
				return chunks
			},
			wantMissing: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ix, gen := builtPendingIndex(t)
			tt.mutate(t, ix)
			want := tt.wantChunks(t, ix)

			total, err := ix.countPending(ctx, gen.Fingerprint())
			require.NoError(t, err)
			assert.Equal(t, want, total)

			gens, err := ix.Generations(ctx)
			require.NoError(t, err)
			require.Len(t, gens, 1)
			assert.Equal(t, tt.wantMissing, gens[0].Missing,
				"countPending and the coverage query must agree on what is pending")
		})
	}
}

// builtPendingIndex returns an index with the two-document corpus fully
// built and embedded.
func builtPendingIndex(t *testing.T) (*Index, kitvec.Generation) {
	t.Helper()
	ix := openTestIndex(t)
	gen := fakeGeneration("fake-model")
	_, err := ix.Build(
		context.Background(), twoDocSource(), fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	return ix, gen
}
