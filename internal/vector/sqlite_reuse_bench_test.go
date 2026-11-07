package vector

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func BenchmarkVectorMirrorUpsert(b *testing.B) {
	ix, err := Open(b.Context(), filepath.Join(b.TempDir(), "vectors.db"), false, 4000)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, ix.Close()) })
	units := make([]db.EmbeddableUnit, b.N)
	keys := make([]string, b.N)
	for i := range units {
		units[i] = userDoc("session", fmt.Sprintf("source-%06d", i), i, "Synthetic document text.")
		keys[i] = DocKey(units[i].Kind, units[i].SessionID, units[i].SourceUUID, i, 1)
	}
	sentinel := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i, unit := range units {
		unchanged, evicted, err := ix.upsertMirrorRow(b.Context(), keys[i], unit, &sentinel)
		require.NoError(b, err)
		require.False(b, unchanged)
		require.Empty(b, evicted)
	}
	b.StopTimer()
	var count int
	require.NoError(b, ix.db.QueryRowContext(b.Context(), `SELECT COUNT(*) FROM vector_messages`).Scan(&count))
	require.Equal(b, b.N, count)
	var content string
	require.NoError(b, ix.db.QueryRowContext(b.Context(),
		`SELECT content FROM vector_messages WHERE doc_key = ?`, keys[0]).Scan(&content))
	require.Equal(b, "Synthetic document text.", content)
}
