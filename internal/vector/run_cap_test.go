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

func seedRunCapArchive(t *testing.T) *db.DB {
	t.Helper()
	archive := dbtest.OpenTestDB(t)
	messages := []db.Message{dbtest.UserMsg("session", 0, "work")}
	for ordinal := 1; ordinal <= 40; ordinal++ {
		messages = append(messages, dbtest.AsstMsg("session", ordinal, strings.Repeat("a", 2000)))
	}
	dbtest.SeedSessionWithMessages(t, archive, "session", "project", messages,
		func(s *db.Session) { s.EndedAt = new("2024-01-01T00:00:00Z") })
	return archive
}

func assertRunCapPieces(t *testing.T, ix *Index, want int) {
	t.Helper()
	var documents, overlaps int
	require.NoError(t, ix.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM vector_messages WHERE session_id = 'session'`).Scan(&documents))
	assert.Equal(t, want, documents)
	err := ix.db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM vector_messages a JOIN vector_messages b
		ON a.session_id = b.session_id AND a.ordinal < b.ordinal
		AND a.ordinal_end >= b.ordinal WHERE a.session_id = 'session'`).Scan(&overlaps)
	require.NoError(t, err)
	assert.Zero(t, overlaps, "a cap change must remove obsolete pieces")
}

func TestBuildRunCapBoundsAppend(t *testing.T) {
	archive := seedRunCapArchive(t)
	ix, err := Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"), false, 4000)
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
}

func TestBuildRunCapChangeResplitsIdleSession(t *testing.T) {
	archive := seedRunCapArchive(t)
	dbtest.SeedSessionWithMessages(t, archive, "newer", "project", []db.Message{dbtest.UserMsg("newer", 0, "new work")},
		func(s *db.Session) { s.EndedAt = new("2024-02-01T00:00:00Z") })
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := Open(t.Context(), path, false, 4000)
	require.NoError(t, err)
	gen := fakeGeneration("fake-model")
	gen.Params = map[string]string{"max_input_chars": "4000"}
	_, err = ix.Build(t.Context(), archive, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	assertRunCapPieces(t, ix, 6)
	require.NoError(t, ix.Close())

	smaller, err := Open(t.Context(), path, false, 2500)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, smaller.Close()) })
	gen.Params["max_input_chars"] = "2500"
	_, err = smaller.Build(t.Context(), archive, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	assertRunCapPieces(t, smaller, 9)
}

// legacyRunSource models the uncapped documents emitted by the version-3 reducer.
func legacyRunSource(t *testing.T, archive *db.DB) *fakeUnitSource {
	t.Helper()
	src := &fakeUnitSource{}
	const structuralRunMaxRunes = 1_000_000
	_, err := archive.ScanEmbeddableUnits(t.Context(), "", false, structuralRunMaxRunes, func(u db.EmbeddableUnit) error {
		session, err := archive.GetSession(t.Context(), u.SessionID)
		require.NoError(t, err)
		require.NotNil(t, session)
		src.rows = append(src.rows, fakeUnit{unit: u, endedAt: *session.EndedAt})
		return nil
	})
	require.NoError(t, err)
	return src
}

func TestBuildRunCapMissingResplitsLegacyRun(t *testing.T) {
	archive := seedRunCapArchive(t)
	dbtest.SeedSessionWithMessages(t, archive, "newer", "project", []db.Message{dbtest.UserMsg("newer", 0, "new work")},
		func(s *db.Session) { s.EndedAt = new("2024-02-01T00:00:00Z") })
	ix, err := Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"), false, 4000)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	gen := fakeGeneration("fake-model")
	initial, err := ix.Build(t.Context(), legacyRunSource(t, archive), fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, initial.Fill.Documents)
	assertRunCapPieces(t, ix, 2)
	require.NoError(t, ix.metaDelete(t.Context(), runMaxRunesMetaKey))
	result, err := ix.Build(t.Context(), archive, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, 5, result.Fill.Documents, "the unchanged user document keeps its vectors")
	assertRunCapPieces(t, ix, 6)
}
