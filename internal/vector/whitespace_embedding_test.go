package vector

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
)

// blankWindowContent is long enough that its middle 4000-rune window (the
// test index's MaxRunes, stride 3400 after the 15% overlap) holds nothing but
// spaces, so kitvec.Split drops that window and numbers the surviving chunks
// 0 and 2. Everything that re-derives chunks from content has to cope with
// that gap.
func blankWindowContent() string {
	return strings.Repeat("a", 100) + strings.Repeat(" ", 7500) + strings.Repeat("b", 100)
}

// recordingEncoder returns a unit-vector encoder that appends every text it is
// asked to embed to seen.
func recordingEncoder(seen *[]string) kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		*seen = append(*seen, texts...)
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}
}

// TestBuildStampsWhitespaceOnlyDocumentWithoutEmbeddingIt covers the whole
// point of the kit 0.13 upgrade: a document whose content is only whitespace
// is stamped for the generation with no vectors and never becomes an
// embeddings request, so no provider ever gets the chance to reject it. The
// build still completes and auto-activates.
func TestBuildStampsWhitespaceOnlyDocumentWithoutEmbeddingIt(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "", 0, "   \n\t   "), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "", 1, "real content"), endedAt: "2024-01-01T00:00:01Z"},
	}}

	var seen []string
	result, err := ix.Build(ctx, src, recordingEncoder(&seen), fakeGeneration("fake-model"),
		BuildOptions{BatchSize: 32})
	require.NoError(t, err)
	assert.Equal(t, []string{"real content"}, seen,
		"whitespace-only content must never reach the embeddings endpoint")
	assert.True(t, result.Activated,
		"a stamped-without-vectors blank document still counts as covered")

	var stamps int
	require.NoError(t, ix.db.QueryRow(
		`SELECT COUNT(*) FROM message_vectors_stamps`).Scan(&stamps))
	assert.Equal(t, 2, stamps, "both documents are stamped")

	var chunks int
	require.NoError(t, ix.db.QueryRow(
		`SELECT COUNT(*) FROM message_vectors_chunks`).Scan(&chunks))
	assert.Equal(t, 1, chunks, "only the non-blank document has a chunk")
}

// TestEmptyEmbeddingInputIsPermanent pins the structured signal that replaced
// sniffing provider error bodies for whitespace wording: kit refuses a blank
// chunk before any HTTP call, and that refusal is a permanent per-document
// rejection, so a fill stamp-skips it instead of retrying it forever.
func TestEmptyEmbeddingInputIsPermanent(t *testing.T) {
	err := fmt.Errorf("encode chunk 0: %w", kitvec.ErrEmptyEmbeddingInput)
	assert.True(t, isPermanentEncodeError(err))
}

// TestBuildSkipsPermanentlyRejectedDocumentSharingABatch is the cross-document
// batching counterpart to TestBuildSkipsPermanentlyRejectedDocumentAndContinues.
// A configured batch_size (production always sets one) packs chunks from
// several documents into one encode call, so a permanent rejection arrives
// with no attribution. The build must still isolate the offending document and
// skip only it; aborting would wedge every later build at the same document.
func TestBuildSkipsPermanentlyRejectedDocumentSharingABatch(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "", 0, "one"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "", 1, "poison"), endedAt: "2024-01-01T00:00:01Z"},
		{unit: userDoc("s1", "", 2, "three"), endedAt: "2024-01-01T00:00:02Z"},
	}}

	rejectPoison := func(_ context.Context, texts []string) ([][]float32, error) {
		if slices.Contains(texts, "poison") {
			return nil, &HTTPStatusError{
				Status: 400, Body: "input exceeds maximum context length",
			}
		}
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}

	result, err := ix.Build(ctx, src, rejectPoison, fakeGeneration("fake-model"),
		BuildOptions{BatchSize: 32})
	require.NoError(t, err,
		"one poison document in a shared batch must not abort the whole build")
	assert.Equal(t, 2, result.Fill.Documents, "the two good documents still embed")
	assert.Equal(t, 1, result.Fill.Skipped, "only the poison document is skipped")
	assert.True(t, result.Activated)
}

// TestBuildTransientBatchErrorStillAborts guards the other side of batch
// isolation: a 5xx applies to the call, not to one input, so it must abort
// without probing each document slice separately.
func TestBuildTransientBatchErrorStillAborts(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "", 0, "one"), endedAt: "2024-01-01T00:00:00Z"},
		{unit: userDoc("s1", "", 1, "two"), endedAt: "2024-01-01T00:00:01Z"},
	}}

	var calls int
	failing := func(_ context.Context, _ []string) ([][]float32, error) {
		calls++
		return nil, &HTTPStatusError{Status: 503, Body: "upstream unavailable"}
	}

	result, err := ix.Build(ctx, src, failing, fakeGeneration("fake-model"),
		BuildOptions{BatchSize: 32})
	require.Error(t, err)
	assert.Zero(t, result.Fill.Skipped, "a transient failure must never skip-stamp")
	assert.Equal(t, 1, calls, "a transient failure must not trigger per-document probes")
}

// TestChunkSnippetResolvesIndexAcrossADroppedWindow covers a search hit on a
// document with a blank window: its chunk indexes have a gap, so resolving a
// snippet by slice position would return the wrong chunk's text (or none).
func TestChunkSnippetResolvesIndexAcrossADroppedWindow(t *testing.T) {
	split := kitvec.SplitOptions{MaxRunes: 10}
	content := strings.Repeat("a", 10) + strings.Repeat(" ", 10) + strings.Repeat("b", 10)
	chunks := kitvec.Split(content, split)
	require.Len(t, chunks, 2, "the all-whitespace middle window is dropped")
	require.Equal(t, []int{0, 2}, []int{chunks[0].Index, chunks[1].Index},
		"the surviving chunks keep their window numbers")

	assert.Equal(t, strings.Repeat("a", 10), chunkSnippet(content, 0, split))
	assert.Equal(t, strings.Repeat("b", 10), chunkSnippet(content, 2, split),
		"the second stored chunk resolves by its index, not its slice position")
	assert.Empty(t, chunkSnippet(content, 1, split),
		"the dropped window has no snippet")
}

// TestRepairKeepsDocumentWithADroppedWindow covers the repair scan's view of
// the same document: chunk indexes 0 and 2 are exactly what Split asks for, so
// repair must leave the document alone. Treating the gap as a missing chunk
// would re-embed the document on every repair run, forever.
func TestRepairKeepsDocumentWithADroppedWindow(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeUnitSource{rows: []fakeUnit{
		{unit: userDoc("s1", "", 0, blankWindowContent()), endedAt: "2024-01-01T00:00:00Z"},
	}}
	gen := fakeGeneration("fake-model")

	var seen []string
	built, err := ix.Build(ctx, src, recordingEncoder(&seen), gen, BuildOptions{BatchSize: 32})
	require.NoError(t, err)
	require.Equal(t, 2, built.Fill.Chunks)

	repaired, err := ix.Build(ctx, src, recordingEncoder(&seen), gen,
		BuildOptions{BatchSize: 32, RepairInvalid: true})
	require.NoError(t, err)
	assert.Zero(t, repaired.Repair.Documents,
		"a document whose chunk indexes skip a blank window is healthy")
}
