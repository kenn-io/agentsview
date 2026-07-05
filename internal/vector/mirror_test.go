package vector

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// fakeMessageSource is a slice-backed MessageSource for mirror tests. It
// records the since value it was called with and filters rows whose
// EndedAt is below since, mimicking db.ScanEmbeddableMessages's semantics.
type fakeMessageSource struct {
	rows     []fakeRow
	gotSince string
}

// fakeRow pairs an EmbeddableMessage with the ended_at of its session, so
// the fake can compute a maxEnded watermark the way the real scan does.
type fakeRow struct {
	msg     db.EmbeddableMessage
	endedAt string
}

func (f *fakeMessageSource) ScanEmbeddableMessages(
	_ context.Context, since string, fn func(db.EmbeddableMessage) error,
) (string, error) {
	f.gotSince = since
	var maxEnded string
	for _, r := range f.rows {
		if since != "" && r.endedAt < since {
			continue
		}
		if err := fn(r.msg); err != nil {
			return "", err
		}
		if r.endedAt > maxEnded {
			maxEnded = r.endedAt
		}
	}
	return maxEnded, nil
}

func openTestIndex(t *testing.T) *Index {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	return ix
}

// vectorMessagesRow reads back one vector_messages row for assertions.
type vectorMessagesRow struct {
	sessionID   string
	ordinal     int
	content     string
	contentHash string
}

func readMirrorRow(t *testing.T, ix *Index, docKey string) (vectorMessagesRow, bool) {
	t.Helper()
	var row vectorMessagesRow
	err := ix.db.QueryRow(
		`SELECT session_id, ordinal, content, content_hash
		 FROM vector_messages WHERE doc_key = ?`, docKey,
	).Scan(&row.sessionID, &row.ordinal, &row.content, &row.contentHash)
	if err != nil {
		return vectorMessagesRow{}, false
	}
	return row, true
}

func mirrorDocKeys(t *testing.T, ix *Index) []string {
	t.Helper()
	rows, err := ix.db.Query(`SELECT doc_key FROM vector_messages ORDER BY doc_key`)
	require.NoError(t, err)
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		keys = append(keys, k)
	}
	require.NoError(t, rows.Err())
	return keys
}

func TestDocKey(t *testing.T) {
	assert.Equal(t, "u:sess-1:uuid-1", DocKey("sess-1", "uuid-1", 5, 1))
	assert.Equal(t, "u:sess-1:uuid-1#2", DocKey("sess-1", "uuid-1", 5, 2))
	assert.Equal(t, "u:sess-1:uuid-1#3", DocKey("sess-1", "uuid-1", 5, 3))
	assert.Equal(t, "o:sess-1:5", DocKey("sess-1", "", 5, 1))
	assert.Equal(t, "o:sess-1:5", DocKey("sess-1", "", 5, 2),
		"occurrence is ignored when source_uuid is empty")
}

func TestRefreshInitialFullInsertsRowsWithCorrectDocKeys(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u1", Ordinal: 0, Content: "hello"},
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", Ordinal: 1, Content: "world"},
			endedAt: "2024-01-01T00:00:01Z",
		},
	}}

	stats, err := ix.Refresh(ctx, src, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 2, Deleted: 0, Unchanged: 0}, stats)

	keys := mirrorDocKeys(t, ix)
	assert.Equal(t, []string{"o:s1:1", "u:s1:u1"}, keys)

	row, ok := readMirrorRow(t, ix, "u:s1:u1")
	require.True(t, ok)
	assert.Equal(t, "s1", row.sessionID)
	assert.Equal(t, 0, row.ordinal)
	assert.Equal(t, "hello", row.content)
	assert.NotEmpty(t, row.contentHash)
}

func TestRefreshContentChangeUpdatesHash(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u1", Ordinal: 0, Content: "hello"},
			endedAt: "2024-01-01T00:00:00Z",
		},
	}}
	_, err := ix.Refresh(ctx, src, true)
	require.NoError(t, err)
	before, ok := readMirrorRow(t, ix, "u:s1:u1")
	require.True(t, ok)

	src.rows[0].msg.Content = "goodbye"
	stats, err := ix.Refresh(ctx, src, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 1, Deleted: 0, Unchanged: 0}, stats)

	after, ok := readMirrorRow(t, ix, "u:s1:u1")
	require.True(t, ok)
	assert.Equal(t, "goodbye", after.content)
	assert.NotEqual(t, before.contentHash, after.contentHash)
}

func TestRefreshOrdinalShiftOnUUIDRowKeepsHashStampSurvives(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u1", Ordinal: 0, Content: "hello"},
			endedAt: "2024-01-01T00:00:00Z",
		},
	}}
	_, err := ix.Refresh(ctx, src, true)
	require.NoError(t, err)

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateActive)
	require.NoError(t, err)

	row, ok := readMirrorRow(t, ix, "u:s1:u1")
	require.True(t, ok)
	require.NoError(t, ix.store.SaveVectors(ctx, fingerprint, "u:s1:u1", row.contentHash,
		[]kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0}}}))

	// Shift the ordinal without changing content: the hash must survive so
	// the stamp (keyed by doc_key + revision) is not invalidated.
	src.rows[0].msg.Ordinal = 3
	stats, err := ix.Refresh(ctx, src, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 0, Deleted: 0, Unchanged: 1}, stats)

	after, ok := readMirrorRow(t, ix, "u:s1:u1")
	require.True(t, ok)
	assert.Equal(t, 3, after.ordinal)
	assert.Equal(t, row.contentHash, after.contentHash)

	pending, err := ix.store.PendingForGeneration(ctx, fingerprint, 100)
	require.NoError(t, err)
	for _, p := range pending {
		assert.NotEqual(t, "u:s1:u1", p.Doc, "shifted doc should not be pending re-embed")
	}
}

func TestRefreshOrdinalShiftOntoStaleLegacySlotEvictsIt(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	// First refresh: a legacy o:-keyed row occupies (s1, 3).
	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", Ordinal: 3, Content: "legacy"},
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u1", Ordinal: 0, Content: "hello"},
			endedAt: "2024-01-01T00:00:00Z",
		},
	}}
	_, err := ix.Refresh(ctx, src, true)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"o:s1:3", "u:s1:u1"}, mirrorDocKeys(t, ix))

	// Stamp the legacy row's vectors so eviction has something to clean up.
	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateActive)
	require.NoError(t, err)
	legacyRow, ok := readMirrorRow(t, ix, "o:s1:3")
	require.True(t, ok)
	require.NoError(t, ix.store.SaveVectors(ctx, fingerprint, "o:s1:3", legacyRow.contentHash,
		[]kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{0, 1, 0}}}))

	// The u1 message's ordinal now shifts onto the legacy row's slot.
	src.rows[1].msg.Ordinal = 3
	stats, err := ix.Refresh(ctx, src, true)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Deleted, "legacy slot occupant evicted")

	assert.ElementsMatch(t, []string{"u:s1:u1"}, mirrorDocKeys(t, ix))
	row, ok := readMirrorRow(t, ix, "u:s1:u1")
	require.True(t, ok)
	assert.Equal(t, 3, row.ordinal)

	// The evicted doc_key's vectors must be deleted too, or they would
	// permanently occupy KNN LIMIT slots: reconcileDeletions can never see
	// the key, because its mirror row is already gone before full-mode
	// reconciliation runs.
	var stampCount int
	require.NoError(t, ix.db.QueryRow(
		`SELECT COUNT(*) FROM message_vectors_stamps WHERE doc_key = ?`, "o:s1:3",
	).Scan(&stampCount))
	assert.Zero(t, stampCount, "evicted doc_key's stamps should be gone")

	var chunkCount int
	require.NoError(t, ix.db.QueryRow(
		`SELECT COUNT(*) FROM message_vectors_chunks WHERE doc_key = ?`, "o:s1:3",
	).Scan(&chunkCount))
	assert.Zero(t, chunkCount, "evicted doc_key's chunks should be gone")
}

func TestRefreshFullDeletesVanishedIdentitiesAndVectors(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u1", Ordinal: 0, Content: "hello"},
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u2", Ordinal: 1, Content: "world"},
			endedAt: "2024-01-01T00:00:01Z",
		},
	}}
	_, err := ix.Refresh(ctx, src, true)
	require.NoError(t, err)

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateActive)
	require.NoError(t, err)
	row, ok := readMirrorRow(t, ix, "u:s1:u2")
	require.True(t, ok)
	require.NoError(t, ix.store.SaveVectors(ctx, fingerprint, "u:s1:u2", row.contentHash,
		[]kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0}}}))

	var stampCount int
	require.NoError(t, ix.db.QueryRow(
		`SELECT COUNT(*) FROM message_vectors_stamps WHERE doc_key = ?`, "u:s1:u2",
	).Scan(&stampCount))
	require.Equal(t, 1, stampCount)

	// u2's message vanishes from the archive.
	src.rows = src.rows[:1]
	stats, err := ix.Refresh(ctx, src, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 0, Deleted: 1, Unchanged: 1}, stats)

	_, ok = readMirrorRow(t, ix, "u:s1:u2")
	assert.False(t, ok, "mirror row for vanished identity should be gone")

	require.NoError(t, ix.db.QueryRow(
		`SELECT COUNT(*) FROM message_vectors_stamps WHERE doc_key = ?`, "u:s1:u2",
	).Scan(&stampCount))
	assert.Zero(t, stampCount, "stamp for vanished identity should be gone")

	var chunkCount int
	require.NoError(t, ix.db.QueryRow(
		`SELECT COUNT(*) FROM message_vectors_chunks WHERE doc_key = ?`, "u:s1:u2",
	).Scan(&chunkCount))
	assert.Zero(t, chunkCount, "chunks for vanished identity should be gone")
}

func TestRefreshIncrementalUsesWatermark(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u1", Ordinal: 0, Content: "hello"},
			endedAt: "2024-01-01T00:00:00Z",
		},
	}}
	_, err := ix.Refresh(ctx, src, true)
	require.NoError(t, err)
	assert.Equal(t, "", src.gotSince, "full refresh should scan from the beginning")

	src.rows = append(src.rows, fakeRow{
		msg:     db.EmbeddableMessage{SessionID: "s2", SourceUUID: "u2", Ordinal: 0, Content: "later"},
		endedAt: "2024-01-02T00:00:00Z",
	})
	stats, err := ix.Refresh(ctx, src, false)
	require.NoError(t, err)
	assert.Equal(t, "2024-01-01T00:00:00Z", src.gotSince,
		"incremental refresh should scan from the stored watermark")
	// The fake mimics ScanEmbeddableMessages's inclusive s.ended_at >= since
	// filter: the original s1/u1 row (endedAt == since) is re-scanned as
	// unchanged, alongside the newly-added s2/u2 row.
	assert.Equal(t, RefreshStats{Upserted: 1, Deleted: 0, Unchanged: 1}, stats,
		"incremental refresh upserts only what the source rescans, never reconciles deletions")

	_, ok := readMirrorRow(t, ix, "u:s2:u2")
	assert.True(t, ok)
}

// TestRefreshDuplicateSourceUUIDGetsStableOccurrenceKeys asserts that two
// messages in one session sharing a non-empty source_uuid (permitted by the
// messages schema) collapse into two distinct mirror rows rather than one,
// and that a second refresh reproduces the same occurrence-based keys so a
// stamped document is not spuriously evicted and re-embedded.
func TestRefreshDuplicateSourceUUIDGetsStableOccurrenceKeys(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "dup", Ordinal: 0, Content: "first"},
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "dup", Ordinal: 1, Content: "second"},
			endedAt: "2024-01-01T00:00:01Z",
		},
	}}

	stats, err := ix.Refresh(ctx, src, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 2, Deleted: 0, Unchanged: 0}, stats)

	keys := mirrorDocKeys(t, ix)
	assert.Equal(t, []string{"u:s1:dup", "u:s1:dup#2"}, keys,
		"duplicate source_uuid rows must collapse into distinct keys, not one")

	first, ok := readMirrorRow(t, ix, "u:s1:dup")
	require.True(t, ok)
	assert.Equal(t, "first", first.content)
	second, ok := readMirrorRow(t, ix, "u:s1:dup#2")
	require.True(t, ok)
	assert.Equal(t, "second", second.content)

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateActive)
	require.NoError(t, err)
	require.NoError(t, ix.store.SaveVectors(ctx, fingerprint, "u:s1:dup", first.contentHash,
		[]kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0}}}))

	// A second refresh must reproduce the same occurrence keys in the same
	// scan order, or the stamped doc would appear to vanish and be
	// re-embedded on every resync.
	stats, err = ix.Refresh(ctx, src, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 0, Deleted: 0, Unchanged: 2}, stats,
		"stable keys across refreshes mean no churn")

	pending, err := ix.store.PendingForGeneration(ctx, fingerprint, 100)
	require.NoError(t, err)
	for _, p := range pending {
		assert.NotEqual(t, "u:s1:dup", p.Doc, "stamped doc should not be pending re-embed")
	}
}

func TestRefreshReadOnlyIndexRejected(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	rw, err := Open(ctx, path, false, 4000)
	require.NoError(t, err)
	require.NoError(t, rw.Close())

	ro, err := Open(ctx, path, true, 4000)
	require.NoError(t, err)
	defer ro.Close()

	_, err = ro.Refresh(ctx, &fakeMessageSource{}, true)
	require.Error(t, err)
}
