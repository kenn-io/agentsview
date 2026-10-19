package vector

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestBuildRunCapBoundsAppendAndReconcilesCapChange(t *testing.T) {
	archive := dbtest.OpenTestDB(t)
	messages := []db.Message{dbtest.UserMsg("session", 0, "work")}
	for ordinal := 1; ordinal <= 40; ordinal++ {
		messages = append(messages, dbtest.AsstMsg("session", ordinal, strings.Repeat("a", 2000)))
	}
	dbtest.SeedSessionWithMessages(t, archive, "session", "project", messages,
		func(s *db.Session) { s.EndedAt = new("2024-01-01T00:00:00Z") })
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := Open(t.Context(), path, false, 4000)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	enc, gen := fakeBuildEncoder(), fakeGeneration("fake-model")
	initial, err := ix.Build(t.Context(), archive, enc, gen, BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, 6, initial.Fill.Documents, "one user document and five capped assistant pieces")

	dbtest.SeedMessages(t, archive, dbtest.AsstMsg("session", 41, strings.Repeat("b", 2000)))
	dbtest.SeedSession(t, archive, "session", "project",
		func(s *db.Session) { s.EndedAt = new("2024-01-02T00:00:00Z") })
	result, err := ix.Build(t.Context(), archive, enc, gen, BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Fill.Documents, "only the trailing piece needs embedding")
	assert.LessOrEqual(t, result.Fill.Chunks, 6, "an append must not re-embed the whole run")

	require.NoError(t, ix.Close())
	smaller, err := Open(t.Context(), path, false, 2500)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, smaller.Close()) })
	_, err = smaller.Build(t.Context(), archive, enc, gen, BuildOptions{})
	require.NoError(t, err)
	var documents int
	require.NoError(t, smaller.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM vector_messages WHERE session_id = 'session'`).Scan(&documents))
	assert.Equal(t, 10, documents, "one user document and nine smaller assistant pieces")
	var overlaps int
	err = smaller.db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM vector_messages a JOIN vector_messages b
		ON a.session_id = b.session_id AND a.ordinal < b.ordinal
		AND a.ordinal_end >= b.ordinal WHERE a.session_id = 'session'`).Scan(&overlaps)
	require.NoError(t, err)
	assert.Zero(t, overlaps, "a cap change must remove obsolete pieces")
}
