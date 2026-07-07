package vector

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// fakeUnitSource is a slice-backed UnitSource for mirror tests. It records
// the since/includeAutomated values it was called with and filters rows
// whose EndedAt is below since or whose automated flag is out of scope,
// mimicking db.ScanEmbeddableUnits's semantics.
type fakeUnitSource struct {
	rows                []fakeUnit
	gotSince            string
	gotIncludeAutomated bool
}

// fakeUnit pairs an EmbeddableUnit with the ended_at of its session, so the
// fake can compute a maxEnded watermark the way the real scan does.
// automated mimics sessions.is_automated: a false zero value means the row
// is never excluded regardless of includeAutomated.
type fakeUnit struct {
	unit      db.EmbeddableUnit
	endedAt   string
	automated bool
}

func (f *fakeUnitSource) ScanEmbeddableUnits(
	_ context.Context, since string, includeAutomated bool,
	fn func(db.EmbeddableUnit) error,
) (string, error) {
	f.gotSince = since
	f.gotIncludeAutomated = includeAutomated
	var maxEnded string
	for _, r := range f.rows {
		if since != "" && r.endedAt < since {
			continue
		}
		if r.automated && !includeAutomated {
			continue
		}
		if err := fn(r.unit); err != nil {
			return "", err
		}
		if r.endedAt > maxEnded {
			maxEnded = r.endedAt
		}
	}
	return maxEnded, nil
}

// userDoc builds the single-message "user" unit shape most mirror and build
// tests use.
func userDoc(sessionID, sourceUUID string, ordinal int, content string) db.EmbeddableUnit {
	return db.EmbeddableUnit{
		SessionID: sessionID, Kind: "user", SourceUUID: sourceUUID,
		Ordinal: ordinal, OrdinalEnd: ordinal, Content: content,
	}
}

// runDoc builds a "run" unit spanning [start, end] with the given joined
// content and member offsets.
func runDoc(
	sessionID, sourceUUID string, start, end int,
	content string, offsets []db.UnitOffset,
) db.EmbeddableUnit {
	return db.EmbeddableUnit{
		SessionID: sessionID, Kind: "run", SourceUUID: sourceUUID,
		Ordinal: start, OrdinalEnd: end, Content: content, Offsets: offsets,
	}
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
	ordinalEnd  int
	subordinate bool
	offsets     string
	content     string
	contentHash string
}

func readMirrorRow(t *testing.T, ix *Index, docKey string) (vectorMessagesRow, bool) {
	t.Helper()
	var row vectorMessagesRow
	err := ix.db.QueryRow(
		`SELECT session_id, ordinal, ordinal_end, subordinate, offsets, content, content_hash
		 FROM vector_messages WHERE doc_key = ?`, docKey,
	).Scan(&row.sessionID, &row.ordinal, &row.ordinalEnd, &row.subordinate,
		&row.offsets, &row.content, &row.contentHash)
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
	assert.Equal(t, "u:sess-1:uuid-1", DocKey("user", "sess-1", "uuid-1", 5, 1))
	assert.Equal(t, "u:sess-1:uuid-1#2", DocKey("user", "sess-1", "uuid-1", 5, 2))
	assert.Equal(t, "u:sess-1:uuid-1#3", DocKey("user", "sess-1", "uuid-1", 5, 3))
	assert.Equal(t, "o:sess-1:5", DocKey("user", "sess-1", "", 5, 1))
	assert.Equal(t, "o:sess-1:5", DocKey("user", "sess-1", "", 5, 2),
		"occurrence is ignored when source_uuid is empty")

	assert.Equal(t, "r:sess-1:uuid-1", DocKey("run", "sess-1", "uuid-1", 5, 1))
	assert.Equal(t, "r:sess-1:uuid-1#2", DocKey("run", "sess-1", "uuid-1", 5, 2))
	assert.Equal(t, "ro:sess-1:5", DocKey("run", "sess-1", "", 5, 1))
	assert.Equal(t, "ro:sess-1:5", DocKey("run", "sess-1", "", 5, 2),
		"occurrence is ignored when source_uuid is empty")
}

func TestRefreshInitialFullInsertsRowsWithCorrectDocKeys(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "u1", 0, "hello"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "", 1, "world"), endedAt: "2024-01-01T00:00:01Z"},
	}}

	stats, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 2, Deleted: 0, Unchanged: 0}, stats)

	keys := mirrorDocKeys(t, ix)
	assert.Equal(t, []string{"o:s1:1", "u:s1:u1"}, keys)

	row, ok := readMirrorRow(t, ix, "u:s1:u1")
	require.True(t, ok)
	assert.Equal(t, "s1", row.sessionID)
	assert.Equal(t, 0, row.ordinal)
	assert.Equal(t, 0, row.ordinalEnd)
	assert.False(t, row.subordinate)
	assert.Equal(t, "[]", row.offsets, "user docs store empty offsets")
	assert.Equal(t, "hello", row.content)
	assert.NotEmpty(t, row.contentHash)
}

// TestRefreshRunRowPersistsUnitColumns asserts a run unit's mirror row
// round-trips every v2 column: ordinal (start), ordinal_end, subordinate,
// and the offsets JSON, alongside content and content_hash.
func TestRefreshRunRowPersistsUnitColumns(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	offsets := []db.UnitOffset{
		{Ordinal: 1, RuneStart: 0, ByteStart: 0},
		{Ordinal: 2, RuneStart: 4, ByteStart: 5},
	}
	run := runDoc("s1", "a1", 1, 2, "hé\n\nworld", offsets)
	run.Subordinate = true
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "u0", 0, "question"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: run, endedAt: "2024-01-01T00:00:01Z"},
	}}

	stats, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 2, Deleted: 0, Unchanged: 0}, stats)
	assert.Equal(t, []string{"r:s1:a1", "u:s1:u0"}, mirrorDocKeys(t, ix))

	row, ok := readMirrorRow(t, ix, "r:s1:a1")
	require.True(t, ok)
	assert.Equal(t, "s1", row.sessionID)
	assert.Equal(t, 1, row.ordinal)
	assert.Equal(t, 2, row.ordinalEnd)
	assert.True(t, row.subordinate)
	assert.Equal(t, "hé\n\nworld", row.content)

	var gotOffsets []db.UnitOffset
	require.NoError(t, json.Unmarshal([]byte(row.offsets), &gotOffsets))
	assert.Equal(t, offsets, gotOffsets, "offsets JSON must round-trip through the column")

	userRow, ok := readMirrorRow(t, ix, "u:s1:u0")
	require.True(t, ok)
	assert.Equal(t, "[]", userRow.offsets)
	assert.False(t, userRow.subordinate)
}

// TestRefreshRunWithoutSourceUUIDFallsBackToOrdinalKey asserts a run whose
// first message predates source_uuid tracking gets the ro:<session>:<ordinal>
// fallback key.
func TestRefreshRunWithoutSourceUUIDFallsBackToOrdinalKey(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeUnitSource{rows: []fakeUnit{
		{
			unit: runDoc("s1", "", 4, 5, "a4\n\na5",
				[]db.UnitOffset{{Ordinal: 4}, {Ordinal: 5, RuneStart: 4, ByteStart: 4}}),
			endedAt: "2024-01-01T00:00:00Z",
		},
	}}

	_, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"ro:s1:4"}, mirrorDocKeys(t, ix))
}

func TestRefreshContentChangeUpdatesHash(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "u1", 0, "hello"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	_, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	before, ok := readMirrorRow(t, ix, "u:s1:u1")
	require.True(t, ok)

	src.rows[0].unit.Content = "goodbye"
	stats, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 1, Deleted: 0, Unchanged: 0}, stats)

	after, ok := readMirrorRow(t, ix, "u:s1:u1")
	require.True(t, ok)
	assert.Equal(t, "goodbye", after.content)
	assert.NotEqual(t, before.contentHash, after.contentHash)
}

// TestRefreshTrailingAppendKeepsRunDocKeyAndReembedsOnlyIt covers the
// steady-state append shape: new assistant messages land on a session's
// trailing run. The run's doc_key (from its unchanged first message) must
// survive, its content_hash must change so it becomes pending re-embed, and
// an untouched run in another session must keep its embedding stamp.
func TestRefreshTrailingAppendKeepsRunDocKeyAndReembedsOnlyIt(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeUnitSource{rows: []fakeUnit{
		{
			unit: runDoc("s1", "a1", 1, 2, "first\n\nsecond", []db.UnitOffset{
				{Ordinal: 1}, {Ordinal: 2, RuneStart: 7, ByteStart: 7},
			}),
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			unit:    runDoc("s2", "b1", 0, 0, "other", []db.UnitOffset{{Ordinal: 0}}),
			endedAt: "2024-01-01T00:00:01Z",
		},
	}}
	_, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateActive)
	require.NoError(t, err)
	for _, key := range []string{"r:s1:a1", "r:s2:b1"} {
		row, ok := readMirrorRow(t, ix, key)
		require.True(t, ok)
		require.NoError(t, ix.store.SaveVectors(ctx, fingerprint, key, row.contentHash,
			[]kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0}}}))
	}
	before, ok := readMirrorRow(t, ix, "r:s1:a1")
	require.True(t, ok)

	// A third assistant message is appended to s1's trailing run.
	src.rows[0].unit = runDoc("s1", "a1", 1, 3, "first\n\nsecond\n\nthird", []db.UnitOffset{
		{Ordinal: 1}, {Ordinal: 2, RuneStart: 7, ByteStart: 7},
		{Ordinal: 3, RuneStart: 15, ByteStart: 15},
	})
	src.rows[0].endedAt = "2024-01-02T00:00:00Z"

	stats, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 1, Deleted: 0, Unchanged: 1}, stats)
	assert.ElementsMatch(t, []string{"r:s1:a1", "r:s2:b1"}, mirrorDocKeys(t, ix))

	after, ok := readMirrorRow(t, ix, "r:s1:a1")
	require.True(t, ok)
	assert.Equal(t, 3, after.ordinalEnd, "trailing append extends ordinal_end")
	assert.NotEqual(t, before.contentHash, after.contentHash,
		"appended content must invalidate the hash so the run is re-embedded")

	pending, err := ix.store.PendingForGeneration(ctx, fingerprint, 100)
	require.NoError(t, err)
	var pendingDocs []string
	for _, p := range pending {
		pendingDocs = append(pendingDocs, p.Doc)
	}
	assert.Contains(t, pendingDocs, "r:s1:a1",
		"the appended run must be pending re-embed")
	assert.NotContains(t, pendingDocs, "r:s2:b1",
		"an untouched run must keep its embedding stamp")
}

// TestRefreshMidRunUserSplitCreatesSecondHalfUnderNewKey covers a rescan
// where a new embeddable user row lands mid-run: the run splits, the first
// half keeps the old doc_key (same first message) with a shrunken content
// and changed hash, and the second half plus the user row appear under new
// keys. Nothing is genuinely removed, so full-mode reconciliation must not
// delete anything.
func TestRefreshMidRunUserSplitCreatesSecondHalfUnderNewKey(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeUnitSource{rows: []fakeUnit{
		{
			unit: runDoc("s1", "a0", 0, 3, "a0\n\na1\n\na2\n\na3", []db.UnitOffset{
				{Ordinal: 0}, {Ordinal: 1, RuneStart: 4, ByteStart: 4},
				{Ordinal: 2, RuneStart: 8, ByteStart: 8},
				{Ordinal: 3, RuneStart: 12, ByteStart: 12},
			}),
			endedAt: "2024-01-01T00:00:00Z",
		},
	}}
	_, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	before, ok := readMirrorRow(t, ix, "r:s1:a0")
	require.True(t, ok)

	// A user message surfaces at ordinal 2 on rescan, splitting the run.
	src.rows = []fakeUnit{
		{
			unit: runDoc("s1", "a0", 0, 1, "a0\n\na1", []db.UnitOffset{
				{Ordinal: 0}, {Ordinal: 1, RuneStart: 4, ByteStart: 4},
			}),
			endedAt: "2024-01-02T00:00:00Z",
		},
		{unit: userDoc("s1", "u2", 2, "question"), endedAt: "2024-01-02T00:00:00Z"},
		{
			unit:    runDoc("s1", "a3", 3, 3, "a3", []db.UnitOffset{{Ordinal: 3}}),
			endedAt: "2024-01-02T00:00:00Z",
		},
	}

	stats, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Zero(t, stats.Deleted, "a split rewrites and adds rows; nothing vanishes")
	assert.Equal(t, []string{"r:s1:a0", "r:s1:a3", "u:s1:u2"}, mirrorDocKeys(t, ix))

	firstHalf, ok := readMirrorRow(t, ix, "r:s1:a0")
	require.True(t, ok)
	assert.Equal(t, 0, firstHalf.ordinal)
	assert.Equal(t, 1, firstHalf.ordinalEnd, "old key shrinks to the first half")
	assert.Equal(t, "a0\n\na1", firstHalf.content)
	assert.NotEqual(t, before.contentHash, firstHalf.contentHash,
		"the shrunken first half must be re-embedded")

	secondHalf, ok := readMirrorRow(t, ix, "r:s1:a3")
	require.True(t, ok)
	assert.Equal(t, 3, secondHalf.ordinal)
	assert.Equal(t, 3, secondHalf.ordinalEnd)
	assert.Equal(t, "a3", secondHalf.content)
}

// TestRefreshRunReplacingVanishedRunSlotEvictsVectorsBeforeRow asserts the
// two-phase eviction path still works over run documents: a new run landing
// on the (session_id, ordinal) slot of a vanished run evicts the old
// doc_key, and — since it is never reinserted in the scan — its vectors and
// stamps are deleted along with its mirror row.
func TestRefreshRunReplacingVanishedRunSlotEvictsVectorsBeforeRow(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeUnitSource{rows: []fakeUnit{
		{
			unit: runDoc("s1", "a2", 2, 3, "old\n\nrun", []db.UnitOffset{
				{Ordinal: 2}, {Ordinal: 3, RuneStart: 5, ByteStart: 5},
			}),
			endedAt: "2024-01-01T00:00:00Z",
		},
	}}
	_, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateActive)
	require.NoError(t, err)
	oldRow, ok := readMirrorRow(t, ix, "r:s1:a2")
	require.True(t, ok)
	require.NoError(t, ix.store.SaveVectors(ctx, fingerprint, "r:s1:a2", oldRow.contentHash,
		[]kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{0, 1, 0}}}))

	// The old run vanishes; a different run (new first-message uuid) now
	// starts at the same ordinal.
	src.rows = []fakeUnit{
		{
			unit: runDoc("s1", "b2", 2, 4, "new\n\nrun\n\nhere", []db.UnitOffset{
				{Ordinal: 2}, {Ordinal: 3, RuneStart: 5, ByteStart: 5},
				{Ordinal: 4, RuneStart: 10, ByteStart: 10},
			}),
			endedAt: "2024-01-02T00:00:00Z",
		},
	}
	stats, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Deleted, "the vanished run is evicted exactly once")
	assert.Equal(t, []string{"r:s1:b2"}, mirrorDocKeys(t, ix))

	var stampCount int
	require.NoError(t, ix.db.QueryRow(
		`SELECT COUNT(*) FROM message_vectors_stamps WHERE doc_key = ?`, "r:s1:a2",
	).Scan(&stampCount))
	assert.Zero(t, stampCount, "evicted run's stamps must be gone")

	var chunkCount int
	require.NoError(t, ix.db.QueryRow(
		`SELECT COUNT(*) FROM message_vectors_chunks WHERE doc_key = ?`, "r:s1:a2",
	).Scan(&chunkCount))
	assert.Zero(t, chunkCount, "evicted run's chunks must be gone")
}

func TestRefreshOrdinalShiftOnUUIDRowKeepsHashStampSurvives(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "u1", 0, "hello"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	_, err := ix.Refresh(ctx, src, true, true)
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
	src.rows[0].unit.Ordinal = 3
	src.rows[0].unit.OrdinalEnd = 3
	stats, err := ix.Refresh(ctx, src, true, true)
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
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "", 3, "legacy"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "u1", 0, "hello"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	_, err := ix.Refresh(ctx, src, true, true)
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

	// The u1 unit's ordinal now shifts onto the legacy row's slot.
	src.rows[1].unit.Ordinal = 3
	src.rows[1].unit.OrdinalEnd = 3
	stats, err := ix.Refresh(ctx, src, true, true)
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

	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "u1", 0, "hello"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "u2", 1, "world"), endedAt: "2024-01-01T00:00:01Z"},
	}}
	_, err := ix.Refresh(ctx, src, true, true)
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

	// u2's unit vanishes from the archive.
	src.rows = src.rows[:1]
	stats, err := ix.Refresh(ctx, src, true, true)
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

// TestRefreshFullEvictionOfVanishedOccupantCountsDeletedOnce covers the
// double-counting regression where a slot-evicted doc_key never reappears
// anywhere in a full-mode scan: it is absent from seen (nothing in the scan
// produced that key) while also being in evictSlotOccupant's evicted set.
// Before the fix, both finalizeEvictions and full-mode reconcileDeletions
// would independently delete the row and count it, double-counting
// RefreshStats.Deleted even though store.DeleteVectors is idempotent and the
// row is physically removed exactly once.
func TestRefreshFullEvictionOfVanishedOccupantCountsDeletedOnce(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	// First refresh: a legacy o:-keyed row occupies (s1, 3); u1 sits at (s1, 0).
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "", 3, "legacy"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "u1", 0, "hello"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	_, err := ix.Refresh(ctx, src, true, true)
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

	// The legacy unit vanishes from the archive entirely (unlike a mere
	// slot shift, it is dropped from src.rows), and u1 shifts onto its old
	// slot. The legacy doc_key is now both slot-evicted (evictSlotOccupant
	// finds it occupying (s1, 3) via the DB) and never seen in this scan at
	// all (it produced no row), so it is a candidate for both cleanup paths.
	src.rows = []fakeUnit{
		{unit: userDoc("s1", "u1", 3, "hello"), endedAt: "2024-01-01T00:00:00Z"},
	}
	stats, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Deleted,
		"vanished, slot-evicted occupant must be counted exactly once")

	assert.ElementsMatch(t, []string{"u:s1:u1"}, mirrorDocKeys(t, ix))

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

func TestRefreshIncrementalUsesWatermark(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "u1", 0, "hello"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	_, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Equal(t, "", src.gotSince, "full refresh should scan from the beginning")

	src.rows = append(src.rows, fakeUnit{
		unit: userDoc("s2", "u2", 0, "later"), endedAt: "2024-01-02T00:00:00Z",
	})
	stats, err := ix.Refresh(ctx, src, false, true)
	require.NoError(t, err)
	assert.Equal(t, "2024-01-01T00:00:00Z", src.gotSince,
		"incremental refresh should scan from the stored watermark")
	// The fake mimics ScanEmbeddableUnits's inclusive s.ended_at >= since
	// filter: the original s1/u1 row (endedAt == since) is re-scanned as
	// unchanged, alongside the newly-added s2/u2 row.
	assert.Equal(t, RefreshStats{Upserted: 1, Deleted: 0, Unchanged: 1}, stats,
		"incremental refresh upserts only what the source rescans, never reconciles deletions")

	_, ok := readMirrorRow(t, ix, "u:s2:u2")
	assert.True(t, ok)
}

// TestRefreshDuplicateSourceUUIDGetsStableOccurrenceKeys asserts that two
// units in one session sharing a non-empty source_uuid (permitted by the
// messages schema) collapse into two distinct mirror rows rather than one,
// and that a second refresh reproduces the same occurrence-based keys so a
// stamped document is not spuriously evicted and re-embedded.
func TestRefreshDuplicateSourceUUIDGetsStableOccurrenceKeys(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "dup", 0, "first"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "dup", 1, "second"), endedAt: "2024-01-01T00:00:01Z"},
	}}

	stats, err := ix.Refresh(ctx, src, true, true)
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
	stats, err = ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 0, Deleted: 0, Unchanged: 2}, stats,
		"stable keys across refreshes mean no churn")

	pending, err := ix.store.PendingForGeneration(ctx, fingerprint, 100)
	require.NoError(t, err)
	for _, p := range pending {
		assert.NotEqual(t, "u:s1:dup", p.Doc, "stamped doc should not be pending re-embed")
	}
}

// TestRefreshDuplicateRunFirstMessageUUIDGetsOccurrenceSuffixes asserts run
// keys share the user docs' per-session occurrence machinery: a run whose
// first-message uuid collides with any earlier doc's uuid in the same
// session gets a deterministic #n suffix in (session_id, ordinal) scan
// order, stable across refreshes.
func TestRefreshDuplicateRunFirstMessageUUIDGetsOccurrenceSuffixes(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "dup", 0, "user first"), endedAt: "2024-01-01T00:00:00Z"},
		{
			unit: runDoc("s1", "dup", 1, 2, "run one", []db.UnitOffset{
				{Ordinal: 1}, {Ordinal: 2, RuneStart: 4, ByteStart: 4},
			}),
			endedAt: "2024-01-01T00:00:01Z",
		},
		{unit: userDoc("s1", "", 3, "plain"), endedAt: "2024-01-01T00:00:02Z"},
		{
			unit:    runDoc("s1", "dup", 4, 4, "run two", []db.UnitOffset{{Ordinal: 4}}),
			endedAt: "2024-01-01T00:00:03Z",
		},
	}}

	stats, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 4, Deleted: 0, Unchanged: 0}, stats)
	assert.Equal(t,
		[]string{"o:s1:3", "r:s1:dup#2", "r:s1:dup#3", "u:s1:dup"},
		mirrorDocKeys(t, ix),
		"occurrence suffixes must be assigned in (session_id, ordinal) scan order")

	// A second refresh must reproduce the exact same keys.
	stats, err = ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Equal(t, RefreshStats{Upserted: 0, Deleted: 0, Unchanged: 4}, stats,
		"occurrence-suffixed run keys must be stable across refreshes")
}

// TestRefreshCascadingOrdinalShiftReinsertsEvictedKeysWithoutLosingCoverage
// covers the two-phase eviction regression: three UUID-keyed rows all shift
// ordinal upward by one in a single refresh (e.g. a message inserted ahead
// of them during resync). Each row's move onto the next row's old slot
// evicts that row mid-scan, but the evicted row's own doc_key is stable and
// reappears moments later in the same scan at its new ordinal. The evicted
// rows' stamps and vectors must survive: only a slot eviction that is never
// reinserted anywhere in the scan should reach store.DeleteVectors.
func TestRefreshCascadingOrdinalShiftReinsertsEvictedKeysWithoutLosingCoverage(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "u0", 0, "zero"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "u1", 1, "one"), endedAt: "2024-01-01T00:00:01Z"},
		{unit: userDoc("s1", "u2", 2, "two"), endedAt: "2024-01-01T00:00:02Z"},
	}}
	_, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)

	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	fingerprint, err := ix.EnsureGeneration(ctx, gen, sqlitevec.StateActive)
	require.NoError(t, err)
	keys := []string{"u:s1:u0", "u:s1:u1", "u:s1:u2"}
	for _, key := range keys {
		row, ok := readMirrorRow(t, ix, key)
		require.True(t, ok)
		require.NoError(t, ix.store.SaveVectors(ctx, fingerprint, key, row.contentHash,
			[]kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0}}}))
	}

	// Every existing row shifts up by one ordinal, cascading eviction
	// across the whole session in scan order: u0's move onto slot 1
	// evicts u1's row, u1's move onto slot 2 evicts u2's row, then u1 and
	// u2 are each reinserted under their own stable doc_key at their own
	// turn later in this same scan.
	for i := range src.rows {
		src.rows[i].unit.Ordinal++
		src.rows[i].unit.OrdinalEnd++
	}

	stats, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	assert.Zero(t, stats.Deleted, "cascaded rows are reinserted in-scan, not genuinely removed")

	for _, key := range keys {
		row, ok := readMirrorRow(t, ix, key)
		require.True(t, ok, "%s must still be in the mirror", key)

		var stampCount int
		require.NoError(t, ix.db.QueryRow(
			`SELECT COUNT(*) FROM message_vectors_stamps WHERE doc_key = ? AND revision = ?`,
			key, row.contentHash,
		).Scan(&stampCount))
		assert.Equal(t, 1, stampCount, "%s's stamp must survive the cascading shift", key)
	}

	pending, err := ix.store.PendingForGeneration(ctx, fingerprint, 100)
	require.NoError(t, err)
	assert.Empty(t, pending, "no document should need re-embedding after the cascading shift")
}

// TestDocKeyInjectiveWithDelimiterCharacters covers pairs of inputs that
// would collide under DocKey's raw (unescaped) delimiters — a source_uuid
// containing a literal "#<n>" suffix colliding with an occurrence suffix,
// and a ":" inside a session_id or source_uuid colliding across the
// session/uuid boundary — asserting escapeDocKeyComponent keeps the
// encoding injective. Kind is part of the identity too: the same components
// under "user" and "run" must produce distinct keys.
func TestDocKeyInjectiveWithDelimiterCharacters(t *testing.T) {
	tests := []struct {
		name                  string
		aKind                 string
		aSession, aUUID       string
		aOrdinal, aOccurrence int
		bKind                 string
		bSession, bUUID       string
		bOrdinal, bOccurrence int
	}{
		{
			name:        "occurrence suffix vs literal hash-number in uuid",
			aKind:       "user",
			aSession:    "s1",
			aUUID:       "dup#2",
			aOrdinal:    0,
			aOccurrence: 1,
			bKind:       "user",
			bSession:    "s1",
			bUUID:       "dup",
			bOrdinal:    1,
			bOccurrence: 2,
		},
		{
			name:        "colon in session vs session/uuid boundary",
			aKind:       "user",
			aSession:    "a:b",
			aUUID:       "c",
			aOrdinal:    0,
			aOccurrence: 1,
			bKind:       "user",
			bSession:    "a",
			bUUID:       "b:c",
			bOrdinal:    0,
			bOccurrence: 1,
		},
		{
			name:        "colon in uuid vs session/uuid boundary",
			aKind:       "user",
			aSession:    "sess",
			aUUID:       "x:y",
			aOrdinal:    0,
			aOccurrence: 1,
			bKind:       "user",
			bSession:    "sess:x",
			bUUID:       "y",
			bOrdinal:    0,
			bOccurrence: 1,
		},
		{
			name:        "literal percent-escape sequence vs raw delimiter",
			aKind:       "user",
			aSession:    "s1",
			aUUID:       "%3A",
			aOrdinal:    0,
			aOccurrence: 1,
			bKind:       "user",
			bSession:    "s1",
			bUUID:       ":",
			bOrdinal:    0,
			bOccurrence: 1,
		},
		{
			name:        "same components under user vs run kinds",
			aKind:       "user",
			aSession:    "s1",
			aUUID:       "x",
			aOrdinal:    0,
			aOccurrence: 1,
			bKind:       "run",
			bSession:    "s1",
			bUUID:       "x",
			bOrdinal:    0,
			bOccurrence: 1,
		},
		{
			name:        "run ordinal fallback vs user ordinal fallback",
			aKind:       "run",
			aSession:    "s1",
			aUUID:       "",
			aOrdinal:    3,
			aOccurrence: 1,
			bKind:       "user",
			bSession:    "s1",
			bUUID:       "",
			bOrdinal:    3,
			bOccurrence: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := DocKey(tt.aKind, tt.aSession, tt.aUUID, tt.aOrdinal, tt.aOccurrence)
			b := DocKey(tt.bKind, tt.bSession, tt.bUUID, tt.bOrdinal, tt.bOccurrence)
			assert.NotEqual(t, a, b, "distinct identities must not collide")
		})
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

	_, err = ro.Refresh(ctx, &fakeUnitSource{}, true, true)
	require.Error(t, err)
}

// TestRefreshParkedLeftoverFromInterruptedRunDoesNotCollide simulates a
// Refresh interrupted between evictSlotOccupant and finalizeEvictions: a
// row left parked at ordinal -1 (parking writes are autocommit, so a crash
// or cancellation mid-scan leaves them behind). The next run's parking
// sentinel must start below that leftover — restarting at 0 would park a
// freshly evicted row in the same session at the taken -1 and fail the
// unique (session_id, ordinal) index, deterministically on every retry,
// wedging refreshes until a full rebuild.
func TestRefreshParkedLeftoverFromInterruptedRunDoesNotCollide(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	// A legacy occupant at (s1, 9), plus u1 and u2 elsewhere.
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "", 9, "legacy"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "u1", 0, "hello"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "u2", 5, "world"), endedAt: "2024-01-01T00:00:00Z"},
	}}
	_, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)

	// Simulate the interrupted run's leftover: u2 parked at -1.
	_, err = ix.db.Exec(`UPDATE vector_messages SET ordinal = -1 WHERE doc_key = 'u:s1:u2'`)
	require.NoError(t, err)

	// Next run: u1 shifts onto the legacy occupant's slot, forcing a fresh
	// eviction parking in the same session; u2 is rescanned and self-heals.
	src.rows[1].unit.Ordinal = 9
	src.rows[1].unit.OrdinalEnd = 9
	stats, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err, "a parked leftover must not collide with new parking")
	assert.Equal(t, 1, stats.Deleted, "the evicted legacy occupant is finalized")

	assert.ElementsMatch(t, []string{"u:s1:u1", "u:s1:u2"}, mirrorDocKeys(t, ix))
	row, ok := readMirrorRow(t, ix, "u:s1:u2")
	require.True(t, ok)
	assert.Equal(t, 5, row.ordinal, "the leftover parked row self-heals on rescan")

	var parked int
	require.NoError(t, ix.db.QueryRow(
		`SELECT COUNT(*) FROM vector_messages WHERE ordinal < 0`).Scan(&parked))
	assert.Zero(t, parked, "no parked rows survive a completed full refresh")
}
