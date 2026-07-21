package vector

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	kitvec "go.kenn.io/kit/vector"
)

// fakeExportEncoder returns a deterministic 4-dimensional encoder for export
// tests, mirroring fakeBuildEncoder's shape at a different dimension so the
// round-trip test can assert Dimension and per-chunk vector length.
func fakeExportEncoder() kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0, 0}
		}
		return out, nil
	}
}

// exportTestSource returns a fakeUnitSource with two sessions: session-1
// holds a user doc plus a multi-message run doc, session-2 holds a single
// user doc, the small corpus TestExportRoundTrip and
// TestSessionEmbeddedDocHashesChangesWithContent share.
func exportTestSource() *fakeUnitSource {
	return &fakeUnitSource{rows: []fakeUnit{
		{
			unit:    userDoc("session-1", "u1", 0, "hello"),
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			unit: runDoc("session-1", "a1", 1, 2, "first\n\nsecond", []db.UnitOffset{
				{Ordinal: 1}, {Ordinal: 2, RuneStart: 7, ByteStart: 7},
			}),
			endedAt: "2024-01-01T00:00:01Z",
		},
		{
			unit:    userDoc("session-2", "u2", 0, "world"),
			endedAt: "2024-01-01T00:00:02Z",
		},
	}}
}

// newBuiltTestIndex opens a temp index, refreshes exportTestSource's two
// sessions, and builds+activates a generation with a fake 4-dim encoder.
func newBuiltTestIndex(t *testing.T) (*Index, kitvec.Generation) {
	t.Helper()
	ix := openTestIndex(t)
	ctx := context.Background()
	src := exportTestSource()
	gen := fakeGeneration4Dim()

	result, err := ix.Build(ctx, src, fakeExportEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	require.True(t, result.Activated)
	return ix, gen
}

// newEmptyTestIndex opens a temp index with no build, for the no-active-
// generation case.
func newEmptyTestIndex(t *testing.T) *Index {
	t.Helper()
	return openTestIndex(t)
}

func fakeGeneration4Dim() kitvec.Generation {
	return kitvec.Generation{Model: "fake-export-model", Dimensions: 4}
}

func TestExportRoundTrip(t *testing.T) {
	ctx := context.Background()
	ix, gen := newBuiltTestIndex(t)

	exp, ok, err := ix.ActiveExport(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, gen.Fingerprint(), exp.Fingerprint)
	assert.Equal(t, 4, exp.Dimension)
	assert.NotEmpty(t, exp.Model)

	hashes, err := ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal, nil)
	require.NoError(t, err)
	require.Len(t, hashes, 2)

	docs, aggHash, err := ix.ExportSessionDocs(ctx, exp.Ordinal, "session-1")
	require.NoError(t, err)
	require.NotEmpty(t, docs)
	assert.Equal(t, hashes["session-1"], aggHash,
		"export hash must equal the session's SessionEmbeddedDocHashes value")
	for _, d := range docs {
		assert.Equal(t, "session-1", d.SessionID)
		assert.NotEmpty(t, d.ContentHash)
		require.NotEmpty(t, d.Chunks)
		for _, c := range d.Chunks {
			assert.Len(t, c.Embedding, 4)
		}
	}

	noDocs, emptyHash, err := ix.ExportSessionDocs(ctx, exp.Ordinal, "absent")
	require.NoError(t, err)
	assert.Empty(t, noDocs)
	assert.Equal(t, "", emptyHash,
		"an empty export hashes to \"\", matching absence from the hash map")
}

func TestSessionEmbeddedDocHashesScoped(t *testing.T) {
	ctx := context.Background()
	ix, _ := newBuiltTestIndex(t)

	exp, ok, err := ix.ActiveExport(ctx)
	require.NoError(t, err)
	require.True(t, ok)

	all, err := ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal, nil)
	require.NoError(t, err)
	require.Len(t, all, 2)

	scoped, err := ix.SessionEmbeddedDocHashes(
		ctx, exp.Ordinal, []string{"session-1"},
	)
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	assert.Equal(t, all["session-1"], scoped["session-1"],
		"a scoped read must produce the same aggregate as the full scan")

	empty, err := ix.SessionEmbeddedDocHashes(
		ctx, exp.Ordinal, []string{},
	)
	require.NoError(t, err)
	assert.Empty(t, empty)

	absent, err := ix.SessionEmbeddedDocHashes(
		ctx, exp.Ordinal, []string{"absent"},
	)
	require.NoError(t, err)
	assert.Empty(t, absent)
}

func TestExportNoActiveGeneration(t *testing.T) {
	ctx := context.Background()
	ix := newEmptyTestIndex(t)
	_, ok, err := ix.ActiveExport(ctx)
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestSessionEmbeddedDocHashesChangesWithContent builds, captures each
// session's embedded-doc aggregate hash, mutates session-1's user doc
// content in the fake source and refreshes (without rebuilding), then
// captures again: session-1's stamp no longer matches its new content_hash,
// so it drops out of the aggregate (session-1's run doc, a1, is untouched
// and stays embedded, so the session key survives with a changed hash);
// session-2, untouched, keeps a stable hash.
func TestSessionEmbeddedDocHashesChangesWithContent(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	src := exportTestSource()
	gen := fakeGeneration4Dim()

	_, err := ix.Build(ctx, src, fakeExportEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	exp, ok, err := ix.ActiveExport(ctx)
	require.NoError(t, err)
	require.True(t, ok)

	before, err := ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal, nil)
	require.NoError(t, err)
	require.Contains(t, before, "session-1")
	require.Contains(t, before, "session-2")

	// A later timestamp than the watermark Build's Refresh already advanced
	// to, or the fake source's incremental scan would never resurface it.
	src.rows[0].unit.Content = "changed"
	src.rows[0].endedAt = "2024-01-02T00:00:00Z"
	_, err = ix.Refresh(ctx, src, false, true)
	require.NoError(t, err)

	after, err := ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal, nil)
	require.NoError(t, err)
	require.Contains(t, after, "session-1",
		"session-1's untouched run doc keeps it in the aggregate")
	assert.NotEqual(t, before["session-1"], after["session-1"],
		"the mutated session's aggregate must change")
	assert.Equal(t, before["session-2"], after["session-2"],
		"the untouched session's hash is stable")

	// The export hash follows the shrunken embedded subset, so a pg push whose
	// delta scan read `before` sees the divergence and defers the session
	// instead of exporting the partial view.
	_, exportHash, err := ix.ExportSessionDocs(ctx, exp.Ordinal, "session-1")
	require.NoError(t, err)
	assert.Equal(t, after["session-1"], exportHash,
		"export hash tracks the current embedded subset")
	assert.NotEqual(t, before["session-1"], exportHash,
		"a stale delta-scan hash no longer matches the export")
}

// TestSessionEmbeddedDocHashesChangesWithMetadata guards the compaction/resync
// case: a doc whose content (and content_hash) is unchanged but whose ordinal
// shifts keeps its UUID-derived doc_key and its embedding stamp, so it stays
// embedded — yet the aggregate must still change so pg push re-anchors it. A
// hash over only (doc_key, content_hash) would miss this and leave PG anchors
// stale. session-2, untouched, keeps a stable hash.
func TestSessionEmbeddedDocHashesChangesWithMetadata(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	src := exportTestSource()
	gen := fakeGeneration4Dim()

	_, err := ix.Build(ctx, src, fakeExportEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	exp, ok, err := ix.ActiveExport(ctx)
	require.NoError(t, err)
	require.True(t, ok)

	before, err := ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal, nil)
	require.NoError(t, err)
	require.Contains(t, before, "session-1")
	require.Contains(t, before, "session-2")

	// Shift session-1's user doc to a free ordinal slot without touching its
	// content. The UUID-keyed doc_key ("u:session-1:u1") and content_hash are
	// unchanged, so its Build-time stamp still matches and the doc stays
	// embedded. Bump endedAt past the watermark so the incremental scan
	// resurfaces it.
	src.rows[0].unit.Ordinal = 5
	src.rows[0].unit.OrdinalEnd = 5
	src.rows[0].endedAt = "2024-01-02T00:00:00Z"
	_, err = ix.Refresh(ctx, src, false, true)
	require.NoError(t, err)

	after, err := ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal, nil)
	require.NoError(t, err)
	require.Contains(t, after, "session-1",
		"a pure ordinal shift keeps the doc embedded (content_hash unchanged)")
	assert.NotEqual(t, before["session-1"], after["session-1"],
		"a metadata-only ordinal shift must still move the aggregate")
	assert.Equal(t, before["session-2"], after["session-2"],
		"the untouched session's hash is stable")
}

// TestExportVersionMismatchReturnsSentinel covers the read-path version gate on
// the export API: a read-only Index over a mirror stamped with a different
// MirrorSchemaVersion must refuse ActiveExport, SessionEmbeddedDocHashes, and
// ExportSessionDocs with ErrMirrorVersionMismatch, the same fail-closed
// behavior Search and StaleActive apply, instead of exporting rows shaped by a
// different schema.
func TestExportVersionMismatchReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	seedV2Mirror(t, path)

	ro, err := Open(ctx, path, true, 4000)
	require.NoError(t, err, "read-only Open must succeed even against a v2-stamped mirror")
	defer ro.Close()

	_, _, err = ro.ActiveExport(ctx)
	assert.ErrorIs(t, err, ErrMirrorVersionMismatch)

	_, err = ro.SessionEmbeddedDocHashes(ctx, 1, nil)
	assert.ErrorIs(t, err, ErrMirrorVersionMismatch)

	_, _, err = ro.ExportSessionDocs(ctx, 1, "s1")
	assert.ErrorIs(t, err, ErrMirrorVersionMismatch)
}
