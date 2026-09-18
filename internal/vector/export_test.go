package vector

import (
	"context"
	"errors"
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
	ctx := t.Context()
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
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	ix, gen := newBuiltTestIndex(t)

	exp, ok, err := ix.ActiveExport(ctx)
	require.NoError(err)
	require.True(ok)
	assert.Equal(exp.Fingerprint, gen.Fingerprint())
	assert.Equal(4, exp.Dimension)
	assert.NotEmpty(exp.Model)

	hashes, err := ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal, nil)
	require.NoError(err)
	require.Len(hashes, 2)

	docs, aggHash, err := ix.ExportSessionDocs(ctx, exp.Ordinal, "session-1")
	require.NoError(err)
	require.NotEmpty(docs)
	assert.Equal(hashes["session-1"], aggHash,
		"export hash must equal the session's SessionEmbeddedDocHashes value")
	for _, d := range docs {
		assert.Equal("session-1", d.SessionID)
		assert.NotEmpty(d.ContentHash)
		require.NotEmpty(d.Chunks)
		for _, c := range d.Chunks {
			assert.Len(c.Embedding, 4)
		}
	}

	noDocs, emptyHash, err := ix.ExportSessionDocs(ctx, exp.Ordinal, "absent")
	require.NoError(err)
	assert.Empty(noDocs)
	assert.Empty(emptyHash,
		"an empty export hashes to \"\", matching absence from the hash map")
}

func TestSessionEmbeddedDocHashesScoped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	ix, _ := newBuiltTestIndex(t)

	exp, ok, err := ix.ActiveExport(ctx)
	require.NoError(err)
	require.True(ok)

	all, err := ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal, nil)
	require.NoError(err)
	require.Len(all, 2)

	scoped, err := ix.SessionEmbeddedDocHashes(
		ctx, exp.Ordinal, []string{"session-1"},
	)
	require.NoError(err)
	require.Len(scoped, 1)
	assert.Equal(all["session-1"], scoped["session-1"],
		"a scoped read must produce the same aggregate as the full scan")

	empty, err := ix.SessionEmbeddedDocHashes(
		ctx, exp.Ordinal, []string{},
	)
	require.NoError(err)
	assert.Empty(empty)

	absent, err := ix.SessionEmbeddedDocHashes(
		ctx, exp.Ordinal, []string{"absent"},
	)
	require.NoError(err)
	assert.Empty(absent)
}

func TestExportNoActiveGeneration(t *testing.T) {
	ctx := t.Context()
	ix := newEmptyTestIndex(t)
	_, ok, err := ix.ActiveExport(ctx)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestVectorExportRebuildSnapshotConsistency(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	ix, gen := newBuiltTestIndex(t)
	src := exportTestSource()

	old, ok, err := ix.BeginExport(ctx, nil)
	require.NoError(err)
	require.True(ok)
	defer old.Close()
	oldHashes, err := old.SessionDocHashes(ctx, nil)
	require.NoError(err)
	oldDocs, oldHash, err := old.SessionDocs(ctx, "session-1")
	require.NoError(err)
	require.NotEmpty(oldDocs)

	failingEncoder := func(_ context.Context, _ []string) ([][]float32, error) {
		return nil, errors.New("embeddings endpoint down")
	}
	_, err = ix.Build(ctx, src, failingEncoder, gen,
		BuildOptions{FullRebuild: true})
	require.Error(err, "full rebuild must leave a marker-bearing partial generation")

	_, ok, err = ix.BeginExport(ctx, nil)
	require.Error(err)
	assert.ErrorIs(err, ErrExportNotReady)
	assert.False(ok)

	gotHashes, err := old.SessionDocHashes(ctx, nil)
	require.NoError(err)
	assert.Equal(oldHashes, gotHashes)
	gotDocs, gotHash, err := old.SessionDocs(ctx, "session-1")
	require.NoError(err)
	assert.Equal(oldHash, gotHash)
	assert.Equal(oldDocs, gotDocs)

	_, err = ix.Build(ctx, src, fakeExportEncoder(), gen,
		BuildOptions{FullRebuild: true})
	require.NoError(err)

	newExport, ok, err := ix.BeginExport(ctx, nil)
	require.NoError(err)
	require.True(ok)
	defer newExport.Close()
	newHashes, err := newExport.SessionDocHashes(ctx, nil)
	require.NoError(err)
	assert.Equal(oldHashes, newHashes)
	newDocs, newHash, err := newExport.SessionDocs(ctx, "session-1")
	require.NoError(err)
	assert.Equal(oldHash, newHash)
	assert.Equal(oldDocs, newDocs)
	assert.Equal(gen.Fingerprint(), newExport.Generation().Fingerprint)
}

func TestVectorExportHandleLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	ix, _ := newBuiltTestIndex(t)
	export, ok, err := ix.BeginExport(ctx, nil)
	require.NoError(err)
	require.True(ok)
	require.NoError(export.Close())
	require.NoError(export.Close())
	_, err = export.SessionDocHashes(ctx, nil)
	assert.Error(err)
	_, _, err = export.SessionDocs(ctx, "session-1")
	assert.Error(err)
}

func TestVectorExportTreatsParkedStampedDocsAsNotReady(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	ix, _ := newBuiltTestIndex(t)

	exp, ok, err := ix.ActiveExport(ctx)
	require.NoError(err)
	require.True(ok)

	_, err = ix.db.ExecContext(ctx, `
UPDATE vector_messages
   SET ordinal = -1
 WHERE session_id = ? AND content = ?`, "session-1", "hello")
	require.NoError(err)

	missing, err := ix.MissingEmbeddedDocs(ctx, exp.Ordinal, nil)
	require.NoError(err)
	assert.EqualValues(1, missing)

	scopedMissing, err := ix.MissingEmbeddedDocs(ctx, exp.Ordinal, []string{"session-1"})
	require.NoError(err)
	assert.EqualValues(1, scopedMissing)

	otherMissing, err := ix.MissingEmbeddedDocs(ctx, exp.Ordinal, []string{"session-2"})
	require.NoError(err)
	assert.Zero(otherMissing)

	_, ok, err = ix.BeginExport(ctx, nil)
	require.Error(err)
	assert.ErrorIs(err, ErrExportNotReady)
	assert.False(ok)

	scoped, ok, err := ix.BeginExport(ctx, []string{"session-2"})
	require.NoError(err)
	require.True(ok)
	require.NoError(scoped.Close())
}

// TestSessionEmbeddedDocHashesChangesWithContent builds, captures each
// session's embedded-doc aggregate hash, mutates session-1's user doc
// content in the fake source and refreshes (without rebuilding), then
// captures again: session-1's stamp no longer matches its new content_hash,
// so it drops out of the aggregate (session-1's run doc, a1, is untouched
// and stays embedded, so the session key survives with a changed hash);
// session-2, untouched, keeps a stable hash.
func TestSessionEmbeddedDocHashesChangesWithContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	ix := openTestIndex(t)
	src := exportTestSource()
	gen := fakeGeneration4Dim()

	_, err := ix.Build(ctx, src, fakeExportEncoder(), gen, BuildOptions{})
	require.NoError(err)

	exp, ok, err := ix.ActiveExport(ctx)
	require.NoError(err)
	require.True(ok)

	before, err := ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal, nil)
	require.NoError(err)
	require.Contains(before, "session-1")
	require.Contains(before, "session-2")

	// A later timestamp than the watermark Build's Refresh already advanced
	// to, or the fake source's incremental scan would never resurface it.
	src.rows[0].unit.Content = "changed"
	src.rows[0].endedAt = "2024-01-02T00:00:00Z"
	_, err = ix.Refresh(ctx, src, false, true)
	require.NoError(err)

	after, err := ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal, nil)
	require.NoError(err)
	require.Contains(after, "session-1",
		"session-1's untouched run doc keeps it in the aggregate")
	assert.NotEqual(before["session-1"], after["session-1"],
		"the mutated session's aggregate must change")
	assert.Equal(before["session-2"], after["session-2"],
		"the untouched session's hash is stable")

	// The export hash follows the shrunken embedded subset, so a pg push whose
	// delta scan read `before` sees the divergence and defers the session
	// instead of exporting the partial view.
	_, exportHash, err := ix.ExportSessionDocs(ctx, exp.Ordinal, "session-1")
	require.NoError(err)
	assert.Equal(after["session-1"], exportHash,
		"export hash tracks the current embedded subset")
	assert.NotEqual(before["session-1"], exportHash,
		"a stale delta-scan hash no longer matches the export")
}

// TestSessionEmbeddedDocHashesChangesWithMetadata guards the compaction/resync
// case: a doc whose content (and content_hash) is unchanged but whose ordinal
// shifts keeps its UUID-derived doc_key and its embedding stamp, so it stays
// embedded — yet the aggregate must still change so pg push re-anchors it. A
// hash over only (doc_key, content_hash) would miss this and leave PG anchors
// stale. session-2, untouched, keeps a stable hash.
func TestSessionEmbeddedDocHashesChangesWithMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	ix := openTestIndex(t)
	src := exportTestSource()
	gen := fakeGeneration4Dim()

	_, err := ix.Build(ctx, src, fakeExportEncoder(), gen, BuildOptions{})
	require.NoError(err)

	exp, ok, err := ix.ActiveExport(ctx)
	require.NoError(err)
	require.True(ok)

	before, err := ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal, nil)
	require.NoError(err)
	require.Contains(before, "session-1")
	require.Contains(before, "session-2")

	// Shift session-1's user doc to a free ordinal slot without touching its
	// content. The UUID-keyed doc_key ("u:session-1:u1") and content_hash are
	// unchanged, so its Build-time stamp still matches and the doc stays
	// embedded. Bump endedAt past the watermark so the incremental scan
	// resurfaces it.
	src.rows[0].unit.Ordinal = 5
	src.rows[0].unit.OrdinalEnd = 5
	src.rows[0].endedAt = "2024-01-02T00:00:00Z"
	_, err = ix.Refresh(ctx, src, false, true)
	require.NoError(err)

	after, err := ix.SessionEmbeddedDocHashes(ctx, exp.Ordinal, nil)
	require.NoError(err)
	require.Contains(after, "session-1",
		"a pure ordinal shift keeps the doc embedded (content_hash unchanged)")
	assert.NotEqual(before["session-1"], after["session-1"],
		"a metadata-only ordinal shift must still move the aggregate")
	assert.Equal(before["session-2"], after["session-2"],
		"the untouched session's hash is stable")
}

// TestExportVersionMismatchReturnsSentinel covers the read-path version gate on
// the export API: a read-only Index over a mirror stamped with a different
// MirrorSchemaVersion must refuse ActiveExport, SessionEmbeddedDocHashes, and
// ExportSessionDocs with ErrMirrorVersionMismatch, the same fail-closed
// behavior Search and StaleActive apply, instead of exporting rows shaped by a
// different schema.
func TestExportVersionMismatchReturnsSentinel(t *testing.T) {
	assert := assert.New(t)

	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "vectors.db")
	seedV2Mirror(t, path)

	ro, err := Open(ctx, path, true, 4000)
	require.NoError(t, err, "read-only Open must succeed even against a v2-stamped mirror")
	defer ro.Close()

	_, _, err = ro.ActiveExport(ctx)
	assert.ErrorIs(err, ErrMirrorVersionMismatch)

	_, err = ro.SessionEmbeddedDocHashes(ctx, 1, nil)
	assert.ErrorIs(err, ErrMirrorVersionMismatch)

	_, _, err = ro.ExportSessionDocs(ctx, 1, "s1")
	assert.ErrorIs(err, ErrMirrorVersionMismatch)
}
