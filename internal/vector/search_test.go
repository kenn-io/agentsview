package vector

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// fakeSearchEncoder maps distinct known texts to orthogonal 3-dimensional
// vectors so a query for one topic scores a perfect match against its own
// document and a clear non-match against the others.
func fakeSearchEncoder() func(_ context.Context, texts []string) ([][]float32, error) {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			switch {
			case strings.Contains(text, "alpha"):
				out[i] = []float32{1, 0, 0}
			case strings.Contains(text, "beta"):
				out[i] = []float32{0, 1, 0}
			default:
				out[i] = []float32{0, 0, 1}
			}
		}
		return out, nil
	}
}

// threeDocSearchSource returns three single-topic documents in one session,
// one each for "alpha", "beta", and a third ("gamma") topic.
func threeDocSearchSource() *fakeUnitSource {
	return &fakeUnitSource{rows: []fakeUnit{
		{
			unit:    userDoc("s1", "u1", 0, "this message mentions alpha topic"),
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			unit:    userDoc("s1", "u2", 1, "this message mentions beta topic"),
			endedAt: "2024-01-01T00:00:01Z",
		},
		{
			unit:    userDoc("s1", "u3", 2, "this message mentions gamma topic"),
			endedAt: "2024-01-01T00:00:02Z",
		},
	}}
}

func TestSearchReturnsBestMatchFirstWithSnippet(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := threeDocSearchSource()
	gen := fakeGeneration("fake-model")

	_, err := ix.Build(ctx, src, fakeSearchEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	hits, err := ix.Search(ctx, fakeSearchEncoder(), "alpha", 10)
	require.NoError(t, err)
	require.NotEmpty(t, hits)

	best := hits[0]
	assert.Equal(t, "s1", best.SessionID)
	assert.Equal(t, 0, best.Ordinal)
	assert.InDelta(t, 1.0, best.Score, 0.01)
	assert.Contains(t, best.Snippet, "alpha")
}

func TestSearchLimitCapsResults(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := threeDocSearchSource()
	gen := fakeGeneration("fake-model")

	_, err := ix.Build(ctx, src, fakeSearchEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	hits, err := ix.Search(ctx, fakeSearchEncoder(), "alpha", 1)
	require.NoError(t, err)
	assert.Len(t, hits, 1)
}

func TestSearchNoGenerationsReturnsErrNoActiveGeneration(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	_, err := ix.Search(ctx, fakeSearchEncoder(), "alpha", 10)
	assert.ErrorIs(t, err, ErrNoActiveGeneration)
}

func TestSearchBuildingOnlyReturnsBuildingError(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := threeDocSearchSource()

	_, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)

	gen := fakeGeneration("fake-model")
	_, err = ix.EnsureGeneration(ctx, gen, sqlitevec.StateBuilding)
	require.NoError(t, err)

	_, err = ix.Search(ctx, fakeSearchEncoder(), "alpha", 10)
	require.Error(t, err)
	var buildingErr *BuildingError
	require.ErrorAs(t, err, &buildingErr)
	assert.Equal(t, 0, buildingErr.Percent, "nothing has been embedded yet")
}

// TestSearchIgnoresBuildingGenerationOfDifferentDimension covers Search's
// active-generation-only query path: while a model/dimension change is in
// progress, a building generation of a different dimension coexists with
// the active one. kitvec.Search's default (query every live generation with
// one caller-supplied encoder) would try to run the active encoder's
// 3-dimensional query vector against the building generation's 5-dimensional
// vec0 table and fail. Search must encode once for, and query only, the
// active generation, so the building generation's differing dimension never
// matters and only active-generation hits come back.
func TestSearchIgnoresBuildingGenerationOfDifferentDimension(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := threeDocSearchSource()
	activeGen := fakeGeneration("active-model")

	_, err := ix.Build(ctx, src, fakeSearchEncoder(), activeGen, BuildOptions{})
	require.NoError(t, err)

	buildingGen := kitvec.Generation{Model: "building-model", Dimensions: 5}
	_, err = ix.EnsureGeneration(ctx, buildingGen, sqlitevec.StateBuilding)
	require.NoError(t, err)

	hits, err := ix.Search(ctx, fakeSearchEncoder(), "alpha", 10)
	require.NoError(t, err, "a building generation of a different dimension must not break search")
	require.NotEmpty(t, hits)
	assert.Equal(t, "s1", hits[0].SessionID)
	assert.Equal(t, 0, hits[0].Ordinal)
	assert.InDelta(t, 1.0, hits[0].Score, 0.01)
}

func TestStaleActiveTrueWhenFingerprintsDiffer(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := threeDocSearchSource()
	gen := fakeGeneration("fake-model")

	_, err := ix.Build(ctx, src, fakeSearchEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	stale, err := ix.StaleActive(ctx, "some-other-fingerprint")
	require.NoError(t, err)
	assert.True(t, stale)

	stale, err = ix.StaleActive(ctx, gen.Fingerprint())
	require.NoError(t, err)
	assert.False(t, stale, "matching fingerprint is not stale")
}

func TestStaleActiveFalseWhenNoActiveGeneration(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	stale, err := ix.StaleActive(ctx, "anything")
	require.NoError(t, err)
	assert.False(t, stale, "no active generation means nothing to compare")
}

// TestAnchorOrdinal pins the anchor policy: the anchor is the member message
// whose rune span contains the matched chunk's center rune, computed from
// the chunk's ACTUAL rune length (the final chunk is capped at the content's
// end, not at start+MaxRunes), with the earlier member winning when the
// center falls in the "\n\n" separator between two members.
func TestAnchorOrdinal(t *testing.T) {
	// Three members joined with "\n\n": member 10 spans runes [0,5),
	// separator [5,7), member 11 spans [7,12), separator [12,14),
	// member 12 spans [14,19).
	offsets := []db.UnitOffset{
		{Ordinal: 10, RuneStart: 0},
		{Ordinal: 11, RuneStart: 7},
		{Ordinal: 12, RuneStart: 14},
	}
	tests := []struct {
		name         string
		offsets      []db.UnitOffset
		contentRunes int
		chunkIndex   int
		opts         kitvec.SplitOptions
		want         int
	}{
		{
			name: "chunk fully inside first member", offsets: offsets,
			// Window [0,4), center 2, inside member 10's span.
			contentRunes: 19, chunkIndex: 0,
			opts: kitvec.SplitOptions{MaxRunes: 4, Overlap: 0}, want: 10,
		},
		{
			name: "single chunk run centers whole content", offsets: offsets,
			// Content fits one chunk: window [0,19), center 9, member 11.
			contentRunes: 19, chunkIndex: 0,
			opts: kitvec.SplitOptions{MaxRunes: 100, Overlap: 15}, want: 11,
		},
		{
			name: "center in separator anchors earlier member", offsets: offsets,
			// Window [4,8), center 6 falls in the separator [5,7): the
			// earlier member 10 wins the boundary tie.
			contentRunes: 19, chunkIndex: 1,
			opts: kitvec.SplitOptions{MaxRunes: 4, Overlap: 0}, want: 10,
		},
		{
			name: "center exactly at member start anchors that member", offsets: offsets,
			// Window [0,14), center 7 == member 11's RuneStart: member 11's
			// span contains rune 7.
			contentRunes: 19, chunkIndex: 0,
			opts: kitvec.SplitOptions{MaxRunes: 14, Overlap: 0}, want: 11,
		},
		{
			name: "short final chunk centers on actual length",
			// Member 12 starts at rune 13; content is 15 runes. Stride is
			// 10-1=9, so chunk 1's window is [9,15): actual length 6,
			// center 12 -> member 11. Centering on MaxRunes instead
			// ([9,19), center 14) would wrongly anchor member 12.
			offsets: []db.UnitOffset{
				{Ordinal: 10, RuneStart: 0},
				{Ordinal: 11, RuneStart: 7},
				{Ordinal: 12, RuneStart: 13},
			},
			contentRunes: 15, chunkIndex: 1,
			opts: kitvec.SplitOptions{MaxRunes: 10, Overlap: 1}, want: 11,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := anchorOrdinal(tt.offsets, tt.contentRunes, tt.chunkIndex, tt.opts)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestChunkWindowMatchesKitSplit cross-checks the anchor computation's
// deterministic window math against kitvec.Split itself: for every chunk kit
// actually produces, chunkWindow's [start,end) rune window must select
// exactly that chunk's text. If kit's stride semantics ever change, this
// test breaks instead of anchors silently drifting.
func TestChunkWindowMatchesKitSplit(t *testing.T) {
	tests := []struct {
		name    string
		content string
		opts    kitvec.SplitOptions
	}{
		{"ascii multi chunk", strings.Repeat("abcde", 9),
			kitvec.SplitOptions{MaxRunes: 10, Overlap: 2}},
		{"multi byte multi chunk", strings.Repeat("é", 25),
			kitvec.SplitOptions{MaxRunes: 7, Overlap: 1}},
		{"single chunk", "short",
			kitvec.SplitOptions{MaxRunes: 10, Overlap: 2}},
		{"short final chunk", strings.Repeat("x", 23),
			kitvec.SplitOptions{MaxRunes: 10, Overlap: 1}},
		{"overlap exceeding max runes is clamped", strings.Repeat("y", 25),
			kitvec.SplitOptions{MaxRunes: 10, Overlap: 50}},
		{"production overlap shape", strings.Repeat("word ", 100),
			kitvec.SplitOptions{MaxRunes: 40, Overlap: ChunkOverlap(40)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := kitvec.Split(tt.content, tt.opts)
			require.NotEmpty(t, chunks)
			runes := []rune(tt.content)
			for _, c := range chunks {
				start, end := chunkWindow(len(runes), c.Index, tt.opts)
				require.GreaterOrEqual(t, start, 0)
				require.LessOrEqual(t, end, len(runes))
				assert.Equal(t, string(runes[start:end]), c.Text,
					"chunk %d window mismatch", c.Index)
			}
		})
	}
}

// openSmallChunkIndex opens a test index whose split options force
// multi-chunk documents at tiny content sizes (MaxRunes = maxInputChars,
// Overlap = ChunkOverlap(maxInputChars)).
func openSmallChunkIndex(t *testing.T, maxInputChars int) *Index {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := Open(ctx, path, false, maxInputChars)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	return ix
}

// seedMirrorRow inserts one vector_messages row shaped like Refresh would
// write it for u, so hydrate-level tests can exercise anchoring without a
// full Build.
func seedMirrorRow(t *testing.T, ix *Index, docKey string, u db.EmbeddableUnit) {
	t.Helper()
	offsets, err := marshalOffsets(u.Offsets)
	require.NoError(t, err)
	_, err = ix.db.Exec(`
INSERT INTO vector_messages (doc_key, session_id, ordinal, ordinal_end,
    subordinate, offsets, content, content_hash)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		docKey, u.SessionID, u.Ordinal, u.OrdinalEnd, u.Subordinate,
		offsets, u.Content, contentHash(u.Content))
	require.NoError(t, err)
}

// TestHydrateHitsAnchorsRunChunks pins the full hydrate path for a run
// document: each matched chunk anchors to the member containing its center
// rune, the hit carries the run's ordinal range and subordinate flag, and
// the snippet is the intersection of the matched chunk's window with the
// ANCHOR member's own span — a substring of the anchor message's text, not
// run-level text spanning members — so the db layer's snippet centering can
// locate it inside the anchor message's content.
func TestHydrateHitsAnchorsRunChunks(t *testing.T) {
	ix := openSmallChunkIndex(t, 10) // stride 10-1=9
	ctx := context.Background()

	content := "aaaaa\n\nbbbbb\n\nccccc" // 19 runes, chunks [0,10) and [9,19)
	seedMirrorRow(t, ix, "r1", db.EmbeddableUnit{
		SessionID: "s1", Kind: "run", Ordinal: 5, OrdinalEnd: 7,
		Subordinate: true, Content: content,
		Offsets: []db.UnitOffset{
			{Ordinal: 5, RuneStart: 0, ByteStart: 0},
			{Ordinal: 6, RuneStart: 7, ByteStart: 7},
			{Ordinal: 7, RuneStart: 14, ByteStart: 14},
		},
	})

	hits, err := ix.hydrateHits(ctx, []kitvec.Hit[string]{
		{Doc: "r1", ChunkIndex: 0, Score: 0.9},
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	// Chunk 0's window [0,10) spans members 5 and 6 and centers on rune 5,
	// in the separator after member 5: the earlier member anchors, and the
	// snippet is clipped to member 5's own text rather than the whole
	// cross-member chunk "aaaaa\n\nbbb".
	assert.Equal(t, 5, hits[0].Ordinal, "anchor ordinal")
	assert.Equal(t, 5, hits[0].OrdinalStart)
	assert.Equal(t, 7, hits[0].OrdinalEnd)
	assert.True(t, hits[0].Subordinate)
	assert.Equal(t, "aaaaa", hits[0].Snippet,
		"run snippet must be a substring of the anchor member's own text")

	hits, err = ix.hydrateHits(ctx, []kitvec.Hit[string]{
		{Doc: "r1", ChunkIndex: 1, Score: 0.8},
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	// Chunk 1's window [9,19) centers on rune 14, member 7's first rune:
	// the snippet is member 7's slice of the window, not "bbb\n\nccccc".
	assert.Equal(t, 7, hits[0].Ordinal, "anchor ordinal")
	assert.Equal(t, 5, hits[0].OrdinalStart)
	assert.Equal(t, 7, hits[0].OrdinalEnd)
	assert.Equal(t, "ccccc", hits[0].Snippet,
		"run snippet must be a substring of the anchor member's own text")
}

// TestHydrateHitsDegenerateChunkIndexFallsBackToAnchorSpan pins the
// fallback for a stale/degenerate ChunkIndex whose re-split window misses
// the content entirely (content changed since embedding): the snippet falls
// back to the anchor member's own span text — never a panic, never text
// from a different member.
func TestHydrateHitsDegenerateChunkIndexFallsBackToAnchorSpan(t *testing.T) {
	ix := openSmallChunkIndex(t, 10)
	ctx := context.Background()

	content := "aaaaa\n\nbbbbb\n\nccccc"
	seedMirrorRow(t, ix, "r1", db.EmbeddableUnit{
		SessionID: "s1", Kind: "run", Ordinal: 5, OrdinalEnd: 7,
		Content: content,
		Offsets: []db.UnitOffset{
			{Ordinal: 5, RuneStart: 0, ByteStart: 0},
			{Ordinal: 6, RuneStart: 7, ByteStart: 7},
			{Ordinal: 7, RuneStart: 14, ByteStart: 14},
		},
	})

	hits, err := ix.hydrateHits(ctx, []kitvec.Hit[string]{
		{Doc: "r1", ChunkIndex: 99, Score: 0.9},
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, 7, hits[0].Ordinal)
	assert.Equal(t, "ccccc", hits[0].Snippet,
		"an out-of-range chunk window must fall back to the anchor member's span text")
}

// TestHydrateHitsCorruptOffsetsFailsWithDocKey seeds a mirror row whose
// offsets column holds invalid JSON and pins that hydration fails fast with
// the doc_key in the error rather than panicking or silently dropping the
// hit.
func TestHydrateHitsCorruptOffsetsFailsWithDocKey(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	_, err := ix.db.Exec(`
INSERT INTO vector_messages (doc_key, session_id, ordinal, ordinal_end,
    subordinate, offsets, content, content_hash)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"r-corrupt", "s1", 0, 1, false, `{"not": "an array"`, "some content", "h1")
	require.NoError(t, err)

	_, err = ix.hydrateHits(ctx, []kitvec.Hit[string]{
		{Doc: "r-corrupt", ChunkIndex: 0, Score: 0.9},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "r-corrupt",
		"the error must name the doc_key whose offsets are corrupt")
}

// TestHydrateHitsUserDocPassthrough pins that a user document (offsets "[]")
// passes its mirror ordinal through unchanged: Ordinal, OrdinalStart, and
// OrdinalEnd all equal the mirror row's ordinal and the anchor helper is
// never consulted.
func TestHydrateHitsUserDocPassthrough(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	seedMirrorRow(t, ix, "u1", db.EmbeddableUnit{
		SessionID: "s1", Kind: "user", Ordinal: 3, OrdinalEnd: 3,
		Content: "a plain user question",
	})

	hits, err := ix.hydrateHits(ctx, []kitvec.Hit[string]{
		{Doc: "u1", ChunkIndex: 0, Score: 0.7},
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, 3, hits[0].Ordinal)
	assert.Equal(t, 3, hits[0].OrdinalStart)
	assert.Equal(t, 3, hits[0].OrdinalEnd)
	assert.False(t, hits[0].Subordinate)
	assert.Equal(t, "a plain user question", hits[0].Snippet)
}

// TestHydrateHitsMultiByteSnippet pins that run snippets slice on rune
// boundaries: with every content rune multi-byte, byte-offset math would
// tear characters apart or select the wrong window.
func TestHydrateHitsMultiByteSnippet(t *testing.T) {
	ix := openSmallChunkIndex(t, 10) // stride 9
	ctx := context.Background()

	content := strings.Repeat("é", 5) + "\n\n" + strings.Repeat("ü", 5) // 12 runes
	seedMirrorRow(t, ix, "r1", db.EmbeddableUnit{
		SessionID: "s1", Kind: "run", Ordinal: 1, OrdinalEnd: 2,
		Content: content,
		Offsets: []db.UnitOffset{
			{Ordinal: 1, RuneStart: 0, ByteStart: 0},
			{Ordinal: 2, RuneStart: 7, ByteStart: 12},
		},
	})

	hits, err := ix.hydrateHits(ctx, []kitvec.Hit[string]{
		{Doc: "r1", ChunkIndex: 1, Score: 0.9},
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	// Chunk 1's window is [9,12): the last three ü runes, center rune 10
	// inside member 2's span.
	assert.Equal(t, "üüü", hits[0].Snippet,
		"run snippet must be a rune-sliced substring of the anchor member's own text")
	assert.True(t, utf8.ValidString(hits[0].Snippet))
	assert.Equal(t, 2, hits[0].Ordinal)
}

// TestSearchRunDocReturnsAnchoredHit pins Search end to end over a mixed
// mirror: a run document's hit is anchored to the member containing the
// matched content and carries its ordinal range and subordinate flag, while
// a user document's hit passes its own ordinal through.
func TestSearchRunDocReturnsAnchoredHit(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	// "run first message" is 17 runes; member 2 starts at rune 19 after the
	// "\n\n" separator. The whole run fits one chunk, whose center rune
	// (39/2 = 19) is member 2's first rune.
	run := db.EmbeddableUnit{
		SessionID: "s1", Kind: "run", SourceUUID: "a1",
		Ordinal: 1, OrdinalEnd: 2, Subordinate: true,
		Content: "run first message\n\nmentions alpha topic",
		Offsets: []db.UnitOffset{
			{Ordinal: 1, RuneStart: 0, ByteStart: 0},
			{Ordinal: 2, RuneStart: 19, ByteStart: 19},
		},
	}
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "u1", 0, "this message mentions beta topic"),
			endedAt: "2024-01-01T00:00:00Z"},
		{unit: run, endedAt: "2024-01-01T00:00:01Z"},
	}}
	gen := fakeGeneration("fake-model")
	_, err := ix.Build(ctx, src, fakeSearchEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	hits, err := ix.Search(ctx, fakeSearchEncoder(), "alpha", 10)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	best := hits[0]
	assert.Equal(t, "s1", best.SessionID)
	assert.Equal(t, 2, best.Ordinal, "anchor: member containing the chunk center")
	assert.Equal(t, 1, best.OrdinalStart)
	assert.Equal(t, 2, best.OrdinalEnd)
	assert.True(t, best.Subordinate)
	assert.Equal(t, "mentions alpha topic", best.Snippet,
		"snippet must be the anchor member's own slice of the chunk, not run-level text")

	hits, err = ix.Search(ctx, fakeSearchEncoder(), "beta", 10)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	best = hits[0]
	assert.Equal(t, 0, best.Ordinal)
	assert.Equal(t, 0, best.OrdinalStart)
	assert.Equal(t, 0, best.OrdinalEnd)
	assert.False(t, best.Subordinate)
}
