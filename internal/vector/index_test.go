package vector

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

func TestOpenCreatesSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	var name string
	require.NoError(t, ix.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='vector_messages'`).Scan(&name))
	require.Equal(t, "vector_messages", name)

	require.NoError(t, ix.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='message_vectors_generations'`).Scan(&name))
	require.Equal(t, "message_vectors_generations", name)
}

func TestOpenReadOnlyOnMissingFileFails(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "missing-vectors.db")

	_, err := Open(ctx, path, true, 4000)
	require.Error(t, err)
}

func TestOpenSplitOptionsUse15PercentOverlap(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	assert.Equal(t, 4000, ix.split.MaxRunes)
	assert.Equal(t, 600, ix.split.Overlap, "15%% of 4000 is 600")
	assert.Equal(t, ChunkOverlap(4000), ix.split.Overlap,
		"Open must derive Overlap from the shared ChunkOverlap helper")
}

func TestChunkOverlapIs15Percent(t *testing.T) {
	assert.Equal(t, 600, ChunkOverlap(4000))
	assert.Equal(t, 150, ChunkOverlap(1000))
	assert.Equal(t, 0, ChunkOverlap(0))
}

func TestEnsureGenerationLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateBuilding)
	require.NoError(t, err)
	require.NotEmpty(t, fingerprint)

	infos, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.Equal(t, "building", infos[0].State)
	require.Equal(t, "fake-model", infos[0].Model)
	require.Equal(t, 3, infos[0].Dimension)
	require.Equal(t, fingerprint, infos[0].Fingerprint)
	require.NotZero(t, infos[0].ID)

	building, ok, err := ix.BuildingFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, fingerprint, building)

	_, ok, err = ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, ix.SetStateByID(ctx, infos[0].ID, sqlitevec.StateActive))

	active, ok, err := ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, fingerprint, active)

	_, ok, err = ix.BuildingFingerprint(ctx)
	require.NoError(t, err)
	require.False(t, ok)

	info, err := ix.GenerationByID(ctx, infos[0].ID)
	require.NoError(t, err)
	require.Equal(t, "active", info.State)
}

// TestGenerationByIDUnknownIDReturnsSentinel guards the HTTP layer's 404
// mapping: an id with no matching row must return an error matching
// ErrGenerationNotFound via errors.Is, not just any error, so callers can
// distinguish "not found" from other failures.
func TestGenerationByIDUnknownIDReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	_, err = ix.GenerationByID(ctx, 999)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrGenerationNotFound)
	require.Contains(t, err.Error(), "999")
}

func TestGenerationCoverageCounts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateBuilding)
	require.NoError(t, err)

	_, err = ix.db.ExecContext(ctx,
		`INSERT INTO vector_messages (doc_key, session_id, ordinal, ordinal_end, content, content_hash)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d1", "s1", 0, 0, "hello world", "h1")
	require.NoError(t, err)
	_, err = ix.db.ExecContext(ctx,
		`INSERT INTO vector_messages (doc_key, session_id, ordinal, ordinal_end, content, content_hash)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d2", "s1", 1, 1, "goodbye world", "h2")
	require.NoError(t, err)

	err = ix.store.SaveVectors(ctx, fingerprint, "d1", "h1", []kitvec.ChunkVector{
		{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0}},
	})
	require.NoError(t, err)

	infos, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.EqualValues(t, 1, infos[0].Embedded)
	require.EqualValues(t, 1, infos[0].Missing)
}

// TestGenerationCoverageStaleRevisionCountsAsMissing asserts that a stamp
// whose revision no longer matches the mirror row's current content_hash
// (the content changed since it was embedded) counts as Missing, not
// Embedded, since kit's store treats such a document as pending re-embed.
func TestGenerationCoverageStaleRevisionCountsAsMissing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateBuilding)
	require.NoError(t, err)

	_, err = ix.db.ExecContext(ctx,
		`INSERT INTO vector_messages (doc_key, session_id, ordinal, ordinal_end, content, content_hash)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"d1", "s1", 0, 0, "hello world", "h1")
	require.NoError(t, err)

	err = ix.store.SaveVectors(ctx, fingerprint, "d1", "h1", []kitvec.ChunkVector{
		{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0}},
	})
	require.NoError(t, err)

	infos, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.EqualValues(t, 1, infos[0].Embedded)
	require.EqualValues(t, 0, infos[0].Missing)

	_, err = ix.db.ExecContext(ctx,
		`UPDATE vector_messages SET content_hash = 'changed' WHERE doc_key = ?`, "d1")
	require.NoError(t, err)

	infos, err = ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.EqualValues(t, 0, infos[0].Embedded, "stale stamp revision no longer counts as embedded")
	require.EqualValues(t, 1, infos[0].Missing, "stale stamp revision counts as missing")
}

// v1MirrorDDL is the pre-versioning mirror schema (no ordinal_end/
// subordinate/offsets columns, no mirror_schema_version key), used to
// simulate a vectors.db left behind by an older agentsview build.
const v1MirrorDDL = `
CREATE TABLE vector_messages (
    doc_key      TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL,
    source_uuid  TEXT NOT NULL DEFAULT '',
    ordinal      INTEGER NOT NULL,
    content      TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    embed_gen    TEXT
);
CREATE UNIQUE INDEX idx_vector_messages_session_ordinal
    ON vector_messages(session_id, ordinal);
CREATE TABLE vector_meta (
    key TEXT PRIMARY KEY, value TEXT NOT NULL
);
`

// seedV1Mirror writes path as a vectors.db with the pre-versioning v1 mirror
// schema plus the state a real build would have left behind: a mirror row, a
// fake kit generations table (sharing generationsTable's name but not its
// real column set, standing in for whatever kit's store last wrote), a
// stray abandoned per-generation table, and meta keys with no
// mirror_schema_version — exactly what a writable Open must detect as a
// mismatch (absent key, mirror state present) and reset.
func seedV1Mirror(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer raw.Close()

	_, err = raw.ExecContext(ctx, v1MirrorDDL)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `
INSERT INTO vector_messages (doc_key, session_id, ordinal, content, content_hash)
VALUES (?, ?, ?, ?, ?)`,
		"d1", "s1", 0, "hello world", "h1")
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `
INSERT INTO vector_meta (key, value) VALUES (?, ?), (?, ?)`,
		refreshWatermarkKey, "2024-01-01T00:00:00Z",
		scopeIncludeAutomatedKey, "true")
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `CREATE TABLE `+generationsTable+` (ordinal INTEGER)`)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `CREATE TABLE message_vectors_gen7 (id INTEGER)`)
	require.NoError(t, err)
}

func TestMirrorSchemaVersionFreshDBStampsVersionNothingDropped(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	var version string
	require.NoError(t, ix.db.QueryRowContext(ctx,
		`SELECT value FROM vector_meta WHERE key = ?`, mirrorSchemaVersionKey,
	).Scan(&version))
	assert.Equal(t, mirrorSchemaVersion, version)

	var metaCount int
	require.NoError(t, ix.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM vector_meta`).Scan(&metaCount))
	assert.Equal(t, 1, metaCount, "a fresh DB has nothing to drop, only the stamped version key")
}

func TestMirrorSchemaVersionCurrentVersionUntouchedOnReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	_, err = ix.db.ExecContext(ctx, `
INSERT INTO vector_messages (doc_key, session_id, ordinal, ordinal_end, content, content_hash)
VALUES (?, ?, ?, ?, ?, ?)`,
		"d1", "s1", 0, 0, "hello world", "h1")
	require.NoError(t, err)
	require.NoError(t, ix.setRefreshWatermark(ctx, "2024-01-01T00:00:00Z"))
	require.NoError(t, ix.Close())

	ix2, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix2.Close()

	var content string
	require.NoError(t, ix2.db.QueryRowContext(ctx,
		`SELECT content FROM vector_messages WHERE doc_key = ?`, "d1").Scan(&content))
	assert.Equal(t, "hello world", content, "current-version data must survive a reopen untouched")

	watermark, err := ix2.refreshWatermark(ctx)
	require.NoError(t, err)
	assert.Equal(t, "2024-01-01T00:00:00Z", watermark)
}

// TestMirrorSchemaVersionMismatchResetsWritePath covers the full write-path
// reset: a v1-shaped mirror plus stray kit tables (including a fake
// generations table and an abandoned per-generation table) must be dropped
// and recreated with the v2 columns and defaults, and vector_meta must be
// cleared except for the freshly stamped version key.
func TestMirrorSchemaVersionMismatchResetsWritePath(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	seedV1Mirror(t, path)

	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	defer ix.Close()

	var rowCount int
	require.NoError(t, ix.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM vector_messages`).Scan(&rowCount))
	assert.Zero(t, rowCount, "v1 mirror rows must not survive a version reset")

	_, err = ix.db.ExecContext(ctx, `
INSERT INTO vector_messages (doc_key, session_id, ordinal, ordinal_end, content, content_hash)
VALUES (?, ?, ?, ?, ?, ?)`,
		"d1", "s1", 0, 0, "hello", "h1")
	require.NoError(t, err)
	var subordinate int
	var offsets string
	require.NoError(t, ix.db.QueryRowContext(ctx,
		`SELECT subordinate, offsets FROM vector_messages WHERE doc_key = ?`, "d1",
	).Scan(&subordinate, &offsets))
	assert.Zero(t, subordinate, "subordinate defaults to 0")
	assert.Equal(t, "[]", offsets, "offsets defaults to an empty JSON array")

	_, err = ix.db.ExecContext(ctx, `
INSERT INTO vector_messages (doc_key, session_id, ordinal, ordinal_end, content, content_hash)
VALUES (?, ?, ?, ?, ?, ?)`,
		"d2", "s1", 0, 0, "duplicate slot", "h2")
	assert.Error(t, err, "the unique (session_id, ordinal) index must be retained")

	var metaCount int
	require.NoError(t, ix.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM vector_meta`).Scan(&metaCount))
	assert.Equal(t, 1, metaCount, "vector_meta is cleared of the old watermark/scope keys")
	var version string
	require.NoError(t, ix.db.QueryRowContext(ctx,
		`SELECT value FROM vector_meta WHERE key = ?`, mirrorSchemaVersionKey,
	).Scan(&version))
	assert.Equal(t, mirrorSchemaVersion, version)

	err = ix.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE name = 'message_vectors_gen7'`).Scan(new(string))
	assert.ErrorIs(t, err, sql.ErrNoRows, "the stray abandoned per-generation table must be dropped")

	var genCount int
	require.NoError(t, ix.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+generationsTable).Scan(&genCount))
	assert.Zero(t, genCount, "kit must have recreated its generations table fresh, not kept the fake row")
}

// TestMirrorSchemaVersionReadOnlyMismatchSearchReturnsSentinel covers the
// read path: Open against a version-mismatched vectors.db must still
// succeed (a read-only CLI process cannot reset the file), but Search must
// fail closed with ErrMirrorVersionMismatch before touching any table,
// rather than misreading v1-shaped rows or falling through to
// ErrNoActiveGeneration.
func TestMirrorSchemaVersionReadOnlyMismatchSearchReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	seedV1Mirror(t, path)

	ro, err := Open(ctx, path, true, 4000)
	require.NoError(t, err, "read-only Open must succeed even against a mismatched mirror")
	defer ro.Close()

	_, err = ro.Search(ctx, fakeSearchEncoder(), "alpha", 10)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMirrorVersionMismatch)
	assert.NotErrorIs(t, err, ErrNoActiveGeneration,
		"a version mismatch must not be reported as an empty index")
}

// TestMirrorSchemaVersionReadOnlyCurrentVersionSearchUnaffected is a
// regression guard: a read-only Open against an up-to-date mirror must not
// be flagged as a mismatch, so Search proceeds to its normal
// ErrNoActiveGeneration/BuildingError/hit-returning behavior.
func TestMirrorSchemaVersionReadOnlyCurrentVersionSearchUnaffected(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")

	rw, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	require.NoError(t, rw.Close())

	ro, err := Open(ctx, path, true, 4000)
	require.NoError(t, err)
	defer ro.Close()

	_, err = ro.Search(ctx, fakeSearchEncoder(), "alpha", 10)
	assert.ErrorIs(t, err, ErrNoActiveGeneration,
		"a current-version empty mirror must fall through to the normal empty-index error")
}
