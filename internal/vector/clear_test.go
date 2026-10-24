package vector

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestClearRemovesIncompletePullContent(t *testing.T) {
	ctx := t.Context()
	archive := dbtest.OpenTestDB(t)
	dbtest.SeedSessionWithMessages(t, archive, "session", "project", []db.Message{
		dbtest.UserMsg("session", 0, "private first prompt"),
		dbtest.UserMsg("session", 1, "private second prompt"),
	})
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	old := fakeGeneration("local")
	_, err = ix.Build(ctx, archive, fakeBuildEncoder(), old, BuildOptions{})
	require.NoError(t, err)

	// A second store shares the file but must survive clearing messages.
	recall, err := OpenSpec(ctx, path, RecallIndexSpec(), false, 4000)
	require.NoError(t, err)
	_, err = recall.Build(ctx, twoDocSource(), fakeBuildEncoder(), fakeGeneration("recall"), BuildOptions{})
	require.NoError(t, err)
	require.NoError(t, recall.Close())

	var stages []IndexSpec
	var vectorTables []string
	for _, model := range []string{"hub-a", "hub-b"} {
		gen := fakeGeneration(model)
		result, err := ix.Pull(ctx, archive, lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
			require.Len(t, r.Identities, 2)
			return HubLookupReply{
				Version: HubVectorVersion, Fingerprint: r.Fingerprint,
				Chunks: []HubChunk{{Index: 0, ChunkCount: 1, Vector: "AACAPwAAAAAAAAAA"}},
				Absent: []int{1},
			}, nil
		}), gen, PullOptions{})
		require.NoError(t, err)
		require.False(t, result.Activated)
		stage, err := ix.pendingPullStage(ctx, gen.Fingerprint())
		require.NoError(t, err)
		require.NotSame(t, ix, stage)
		stages = append(stages, stage.spec)
		ordinal, err := stage.ordinalForFingerprint(ctx, gen.Fingerprint())
		require.NoError(t, err)
		vectorTables = append(vectorTables, fmt.Sprintf("%s_v%d", stage.spec.VectorsPrefix, ordinal))
		var content []string
		rows, err := ix.db.QueryContext(ctx, "SELECT content FROM "+stage.spec.DocsTable+" ORDER BY content")
		require.NoError(t, err)
		for rows.Next() {
			var text string
			require.NoError(t, rows.Scan(&text))
			content = append(content, text)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.Equal(t, []string{"private first prompt", "private second prompt"}, content)
		require.Equal(t, 1, clearTestRowCount(t, ix, vectorTables[len(vectorTables)-1]))
	}
	oldOrdinal, err := ix.ordinalForFingerprint(ctx, old.Fingerprint())
	require.NoError(t, err)
	vectorTables = append(vectorTables, fmt.Sprintf("message_vectors_v%d", oldOrdinal))
	require.Equal(t, 2, clearTestRowCount(t, ix, ix.spec.DocsTable))
	require.Equal(t, 2, clearTestRowCount(t, ix, vectorTables[2]))

	// This is the public path used by usage-only startup. The archive itself
	// stays intact; only the disposable index is cleared.
	require.NoError(t, ix.Clear(ctx))
	for _, reopened := range []bool{false, true} {
		if reopened {
			require.NoError(t, ix.Close())
			ix, err = Open(ctx, path, false, 4000)
			require.NoError(t, err)
		}
		for _, spec := range append(stages, ix.spec) {
			for _, table := range []string{spec.DocsTable, spec.chunksTable(), spec.stampsTable(), spec.repairQueueTable()} {
				assert.Zero(t, clearTestRowCount(t, ix, table), "retained rows in %s after reopen=%v", table, reopened)
			}
		}
		for _, table := range vectorTables {
			assert.Zero(t, clearTestRowCount(t, ix, table), "retained vectors in %s after reopen=%v", table, reopened)
		}
		var pending int
		require.NoError(t, ix.db.QueryRowContext(ctx, `SELECT count(*) FROM vector_meta WHERE substr(key,1,?)=?`, len(pullStageKey), pullStageKey).Scan(&pending))
		assert.Zero(t, pending)
		fp, active, err := ix.ActiveFingerprint(ctx)
		require.NoError(t, err)
		assert.True(t, active, "Clear preserves existing generation bookkeeping")
		assert.Equal(t, old.Fingerprint(), fp)
		export, ok, err := ix.BeginExport(ctx, nil)
		require.NoError(t, err)
		require.True(t, ok)
		docs, _, err := export.SessionDocs(ctx, "session")
		require.NoError(t, err)
		assert.Empty(t, docs)
		require.NoError(t, export.Close())
		assert.Equal(t, 2, clearTestRowCount(t, ix, "vector_recall_entries"))
		assert.Equal(t, 2, clearTestRowCount(t, ix, "recall_vectors_v1"))
	}
	messages, err := archive.GetMessages(ctx, "session", 0, 10, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "private first prompt", messages[0].Content)
	assert.Equal(t, "private second prompt", messages[1].Content)
}

// A dropped staging table and an emptied active table both retain zero rows.
func clearTestRowCount(t *testing.T, ix *Index, table string) int {
	t.Helper()
	var exists, count int
	require.NoError(t, ix.db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&exists))
	if exists != 0 {
		require.NoError(t, ix.db.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&count))
	}
	return count
}
