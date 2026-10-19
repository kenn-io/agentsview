package vector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

// runCapMessages builds one user message and forty 2,000-rune assistant
// messages; the first assistant message has firstRunes runes. uuid gives each
// message a source UUID so pieces use UUID keys instead of ordinal keys.
func runCapMessages(firstRunes int, uuid bool) []db.Message {
	messages := []db.Message{dbtest.UserMsg("session", 0, "work")}
	for ordinal := 1; ordinal <= 40; ordinal++ {
		size := 2000
		if ordinal == 1 {
			size = firstRunes
		}
		messages = append(messages, dbtest.AsstMsg("session", ordinal, strings.Repeat("a", size)))
	}
	if uuid {
		for i := range messages {
			messages[i].SourceUUID = fmt.Sprintf("uuid-%d", i)
		}
	}
	return messages
}

func seedRunCapArchive(t *testing.T, uuid bool) *db.DB {
	t.Helper()
	archive := dbtest.OpenTestDB(t)
	dbtest.SeedSessionWithMessages(t, archive, "session", "project", runCapMessages(2000, uuid),
		func(s *db.Session) { s.EndedAt = new("2024-01-01T00:00:00Z") })
	return archive
}

func assertRunCapPieces(t *testing.T, ix *Index, want int) {
	t.Helper()
	var documents int
	require.NoError(t, ix.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM vector_messages WHERE session_id = 'session'`).Scan(&documents))
	assert.Equal(t, want, documents)
	assertRunCapNoOverlap(t, ix)
}

func assertRunCapNoOverlap(t *testing.T, ix *Index) {
	t.Helper()
	var overlaps int
	err := ix.db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM vector_messages a JOIN vector_messages b
		ON a.session_id = b.session_id AND a.ordinal < b.ordinal
		AND a.ordinal_end >= b.ordinal WHERE a.session_id = 'session'`).Scan(&overlaps)
	require.NoError(t, err)
	assert.Zero(t, overlaps, "a cap change must remove obsolete pieces")
}

func TestBuildRunCapAppendAndRewrite(t *testing.T) {
	for _, keys := range []struct {
		name string
		uuid bool
	}{{"ordinal keys", false}, {"uuid keys", true}} {
		t.Run(keys.name, func(t *testing.T) {
			archive := seedRunCapArchive(t, keys.uuid)
			ix, err := Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"), false, 4000)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, ix.Close()) })
			enc, gen := fakeBuildEncoder(), fakeGeneration("fake-model")
			_, err = ix.Build(t.Context(), archive, enc, gen, BuildOptions{})
			require.NoError(t, err)

			appended := dbtest.AsstMsg("session", 41, strings.Repeat("b", 2000))
			if keys.uuid {
				appended.SourceUUID = "uuid-41"
			}
			dbtest.SeedMessages(t, archive, appended)
			dbtest.SeedSession(t, archive, "session", "project",
				func(s *db.Session) { s.EndedAt = new("2024-01-02T00:00:00Z") })
			result, err := ix.Build(t.Context(), archive, enc, gen, BuildOptions{})
			require.NoError(t, err)
			assert.Equal(t, 1, result.Fill.Documents, "only the trailing piece needs embedding")
			assert.LessOrEqual(t, result.Fill.Chunks, 6, "an append must not re-embed the whole run")
			assertRunCapNoOverlap(t, ix)

			// A re-sync that shortens an earlier message moves every later
			// boundary: pieces 1-8, 9-16, ... become 1-9, 10-17, ...
			require.NoError(t, archive.ReplaceSessionMessages(t.Context(), "session",
				append(runCapMessages(1000, keys.uuid), appended)))
			dbtest.SeedSession(t, archive, "session", "project",
				func(s *db.Session) { s.EndedAt = new("2024-01-03T00:00:00Z") })
			_, err = ix.Build(t.Context(), archive, enc, gen, BuildOptions{})
			require.NoError(t, err)
			assertRunCapNoOverlap(t, ix)
		})
	}
}

// interruptedSource fails the scan after a few units, leaving a reshape
// half-written.
type interruptedSource struct {
	*db.DB
}

func (s interruptedSource) ScanEmbeddableUnits(
	ctx context.Context, since string, includeAutomated bool, maxRunRunes int,
	fn func(db.EmbeddableUnit) error,
) (string, error) {
	units := 0
	return s.DB.ScanEmbeddableUnits(ctx, since, includeAutomated, maxRunRunes, func(u db.EmbeddableUnit) error {
		if units++; units > 4 {
			return errors.New("interrupted")
		}
		return fn(u)
	})
}

func TestBuildRunCapChangeResplitsIdleSession(t *testing.T) {
	tests := []struct {
		name        string
		caps        []int
		want        []int
		interrupted int
	}{
		{name: "smaller cap", caps: []int{4000, 2500}, want: []int{6, 9}, interrupted: -1},
		{name: "larger cap", caps: []int{2500, 4000}, want: []int{9, 6}, interrupted: -1},
		{name: "interrupted reshape, previous cap restored", caps: []int{4000, 2500, 4000}, want: []int{6, 6}, interrupted: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive := seedRunCapArchive(t, false)
			dbtest.SeedSessionWithMessages(t, archive, "newer", "project", []db.Message{dbtest.UserMsg("newer", 0, "new work")},
				func(s *db.Session) { s.EndedAt = new("2024-02-01T00:00:00Z") })
			path := filepath.Join(t.TempDir(), "vectors.db")
			gen := fakeGeneration("fake-model")
			completed := 0
			for i, maxInputChars := range tt.caps {
				ix, err := Open(t.Context(), path, false, maxInputChars)
				require.NoError(t, err)
				gen.Params = map[string]string{"max_input_chars": strconv.Itoa(maxInputChars)}
				var src UnitSource = archive
				if i == tt.interrupted {
					src = interruptedSource{archive}
				}
				_, err = ix.Build(t.Context(), src, fakeBuildEncoder(), gen, BuildOptions{})
				if i == tt.interrupted {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					assertRunCapPieces(t, ix, tt.want[completed])
					completed++
				}
				require.NoError(t, ix.Close())
			}
		})
	}
}

// legacyRunSource models the uncapped documents emitted by the version-3 reducer.
func legacyRunSource(t *testing.T, archive *db.DB) *fakeUnitSource {
	t.Helper()
	src := &fakeUnitSource{}
	_, err := archive.ScanEmbeddableUnits(t.Context(), "", false, math.MaxInt, func(u db.EmbeddableUnit) error {
		session, err := archive.GetSession(t.Context(), u.SessionID)
		require.NoError(t, err)
		require.NotNil(t, session)
		src.rows = append(src.rows, fakeUnit{unit: u, endedAt: *session.EndedAt})
		return nil
	})
	require.NoError(t, err)
	return src
}
