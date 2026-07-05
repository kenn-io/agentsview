package vector

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// fakeBuildEncoder returns a deterministic 3-dimensional encoder that never
// fails, for tests that only care about fill/activation bookkeeping rather
// than the vectors themselves.
func fakeBuildEncoder() kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}
}

// twoDocSource returns a fakeMessageSource with two distinct messages in
// one session, the small corpus most build tests share.
func twoDocSource() *fakeMessageSource {
	return &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u1", Ordinal: 0, Content: "hello"},
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u2", Ordinal: 1, Content: "world"},
			endedAt: "2024-01-01T00:00:01Z",
		},
	}}
}

func fakeGeneration(model string) kitvec.Generation {
	return kitvec.Generation{Model: model, Dimensions: 3}
}

func TestBuildFirstBuildEmbedsAllAndActivates(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	gen := fakeGeneration("fake-model")

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	assert.True(t, result.Activated)
	assert.Equal(t, gen.Fingerprint(), result.Fingerprint)
	assert.Equal(t, 2, result.Fill.Documents)

	active, ok, err := ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, gen.Fingerprint(), active)
}

func TestBuildSecondBuildNoChangesFillsZero(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	gen := fakeGeneration("fake-model")

	_, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, result.Fill.Documents)
	assert.False(t, result.Activated, "already active, no re-activation")
}

func TestBuildContentChangeReembedsExactlyOne(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	gen := fakeGeneration("fake-model")

	_, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	// A real edit bumps the session's ended_at, so give the changed row a
	// newer timestamp than the watermark the first build advanced to, or
	// the fake source's incremental scan (mimicking the real one) would
	// never resurface it.
	src.rows[0].msg.Content = "changed"
	src.rows[0].endedAt = "2024-01-02T00:00:00Z"

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Fill.Documents, "only the changed document is re-embedded")
	assert.False(t, result.Activated, "target was already active")
}

func TestBuildModelChangeBuildsSecondGenerationAndRetiresOld(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	gen1 := fakeGeneration("model-a")

	_, err := ix.Build(ctx, src, fakeBuildEncoder(), gen1, BuildOptions{})
	require.NoError(t, err)

	gen2 := fakeGeneration("model-b")
	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen2, BuildOptions{})
	require.NoError(t, err)
	assert.True(t, result.Activated)
	assert.Equal(t, gen2.Fingerprint(), result.Fingerprint)
	assert.Equal(t, 2, result.Fill.Documents)

	active, ok, err := ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, gen2.Fingerprint(), active)

	gens, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, gens, 2)
	var oldState string
	for _, g := range gens {
		if g.Fingerprint == gen1.Fingerprint() {
			oldState = g.State
		}
	}
	assert.Equal(t, string(sqlitevec.StateRetired), oldState, "old active generation is retired")
}

func TestBuildFullRebuildSameFingerprintReembedsEverything(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	gen := fakeGeneration("fake-model")

	_, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{FullRebuild: true})
	require.NoError(t, err)
	assert.Equal(t, 2, result.Fill.Documents, "full rebuild re-embeds every document")
	assert.False(t, result.Activated, "already-active generation stays active without reactivation")

	active, ok, err := ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, gen.Fingerprint(), active)
}

// TestBuildFullRebuildRetiredGenerationReembeds covers the resolveBuildTarget
// gap where a FullRebuild request targets a fingerprint that already exists
// as a retired generation (from an earlier model switch): without resetting
// it, EnsureGeneration would reuse its old stamps and Fill would find
// nothing pending, silently reactivating stale embeddings instead of
// performing the requested rebuild.
func TestBuildFullRebuildRetiredGenerationReembeds(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	genA := fakeGeneration("model-a")
	genB := fakeGeneration("model-b")

	_, err := ix.Build(ctx, src, fakeBuildEncoder(), genA, BuildOptions{})
	require.NoError(t, err)
	_, err = ix.Build(ctx, src, fakeBuildEncoder(), genB, BuildOptions{})
	require.NoError(t, err, "genA is now retired, genB active")

	var encodeCalls int
	countingEncoder := func(_ context.Context, texts []string) ([][]float32, error) {
		encodeCalls++
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}

	result, err := ix.Build(ctx, src, countingEncoder, genA, BuildOptions{FullRebuild: true})
	require.NoError(t, err)
	assert.Equal(t, 2, result.Fill.Documents,
		"full rebuild on a retired generation must re-embed every document")
	assert.Positive(t, encodeCalls, "encoder must actually be invoked, not skipped")
}

// TestBuildScopeChangeToIncludeAutomatedForcesFullRefreshAndEmbedsOlderDoc
// covers the interplay between a widening include-automated scope change and
// the refresh watermark: the automated doc is older than the first build's
// watermark (set from the human doc's later ended_at, since the automated
// doc never entered the scan at all under the default scope). Without
// scope-change detection forcing a full (since="") rescan, the second
// build's incremental scan would stay restricted to the stored watermark and
// permanently miss the now-in-scope but chronologically older document.
func TestBuildScopeChangeToIncludeAutomatedForcesFullRefreshAndEmbedsOlderDoc(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	gen := fakeGeneration("fake-model")

	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "human", Ordinal: 0, Content: "hello"},
			endedAt: "2024-01-02T00:00:00Z",
		},
		{
			msg:       db.EmbeddableMessage{SessionID: "s2", SourceUUID: "auto", Ordinal: 0, Content: "roborev output"},
			endedAt:   "2024-01-01T00:00:00Z", // older than the human doc
			automated: true,
		},
	}}

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Fill.Documents, "the automated doc is excluded by the default scope")
	assert.ElementsMatch(t, []string{"u:s1:human"}, mirrorDocKeys(t, ix))

	result, err = ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{IncludeAutomated: true})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Fill.Documents, "only the newly in-scope automated doc is embedded")
	assert.ElementsMatch(t, []string{"u:s1:human", "u:s2:auto"}, mirrorDocKeys(t, ix),
		"the older automated doc must be picked up despite predating the stored refresh watermark")
}

// TestBuildScopeChangeToExcludeAutomatedRemovesOutOfScopeMirrorRow covers the
// narrowing direction: reverting to the default scope after building with
// IncludeAutomated: true must reconcile the automated document's mirror row
// (and its vectors) away, without touching the still-in-scope human
// document's existing embedding.
func TestBuildScopeChangeToExcludeAutomatedRemovesOutOfScopeMirrorRow(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	gen := fakeGeneration("fake-model")

	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "human", Ordinal: 0, Content: "hello"},
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			msg:       db.EmbeddableMessage{SessionID: "s2", SourceUUID: "auto", Ordinal: 0, Content: "roborev output"},
			endedAt:   "2024-01-02T00:00:00Z",
			automated: true,
		},
	}}

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{IncludeAutomated: true})
	require.NoError(t, err)
	assert.Equal(t, 2, result.Fill.Documents)
	assert.ElementsMatch(t, []string{"u:s1:human", "u:s2:auto"}, mirrorDocKeys(t, ix))

	result, err = ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{IncludeAutomated: false})
	require.NoError(t, err)
	assert.Equal(t, 0, result.Fill.Documents,
		"no embedding work: the human doc was already embedded and stays so")
	assert.Equal(t, 1, result.Refresh.Deleted,
		"the now-out-of-scope automated row must be reconciled away")
	assert.ElementsMatch(t, []string{"u:s1:human"}, mirrorDocKeys(t, ix))

	_, ok := readMirrorRow(t, ix, "u:s2:auto")
	assert.False(t, ok, "the out-of-scope mirror row must be removed")
}

// TestBuildLegacyMirrorMissingScopeKeyForcesFullRefresh covers a mirror built
// before the include-automated scope feature existed: a refresh watermark is
// already stamped (this is not a first-ever build), but
// scope_include_automated was never written since setIncludeAutomatedScope
// did not exist yet. Without treating that missing key as a scope change,
// Build would run an incremental (since=watermark) scan forever and never
// pick up a document older than the stored watermark, nor would it ever
// reconcile away now-out-of-scope automated rows a legacy mirror might carry.
func TestBuildLegacyMirrorMissingScopeKeyForcesFullRefresh(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	gen := fakeGeneration("fake-model")

	// Simulate the pre-scope-feature mirror state directly: a stamped
	// watermark with no scope_include_automated row in vector_meta.
	require.NoError(t, ix.setRefreshWatermark(ctx, "2024-06-01T00:00:00Z"))
	_, hasScope, err := ix.storedIncludeAutomatedScope(ctx)
	require.NoError(t, err)
	require.False(t, hasScope, "test setup must not pre-seed a scope key")

	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "human", Ordinal: 0, Content: "hello"},
			endedAt: "2024-01-01T00:00:00Z", // older than the stored watermark
		},
	}}

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	assert.Empty(t, src.gotSince,
		"a legacy mirror with no stored scope key must force a full (since=\"\") rescan")
	assert.Equal(t, 1, result.Fill.Documents,
		"the pre-watermark document must be picked up despite predating the stored refresh watermark")
	assert.ElementsMatch(t, []string{"u:s1:human"}, mirrorDocKeys(t, ix))

	storedScope, hasScope, err := ix.storedIncludeAutomatedScope(ctx)
	require.NoError(t, err)
	assert.True(t, hasScope,
		"Build must stamp the scope key so later builds compare against a real stored value")
	assert.False(t, storedScope)
}

// TestCountPendingIncludesRevisionChangedDocs covers countPending's
// BuildProgress.Total denominator: a document whose mirror content_hash
// changed since it was last stamped must still count as pending, matching
// the s.revision = d.content_hash predicate generationCoverageQuery's
// Missing column uses, or Total under-reports outstanding work.
func TestCountPendingIncludesRevisionChangedDocs(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	gen := fakeGeneration("fake-model")

	_, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)

	fp := gen.Fingerprint()
	total, err := ix.countPending(ctx, fp)
	require.NoError(t, err)
	assert.Zero(t, total, "fully embedded generation has nothing pending")

	// Simulate content changing without a mirror refresh reconciling the
	// stamp: the stamp's revision no longer matches content_hash.
	_, err = ix.db.ExecContext(ctx,
		`UPDATE vector_messages SET content_hash = 'changed-hash' WHERE doc_key = 'u:s1:u1'`)
	require.NoError(t, err)

	total, err = ix.countPending(ctx, fp)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total, "content-changed doc must count as pending, not complete")
}

// TestCountPendingSumsChunksAcrossMultiChunkDocuments covers the units bug
// where BuildProgress.Total counted pending documents while Done counted
// encoded chunks: a message that splits into several chunks would drive the
// reported percentage past 100%. countPending must sum chunks, matching
// Done's unit, not count the document once.
func TestCountPendingSumsChunksAcrossMultiChunkDocuments(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	longContent := strings.Repeat("word ", 2000) // far past the 4000-rune split threshold
	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u1", Ordinal: 0, Content: "short"},
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u2", Ordinal: 1, Content: longContent},
			endedAt: "2024-01-01T00:00:01Z",
		},
	}}
	gen := fakeGeneration("fake-model")

	_, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	fp := gen.Fingerprint()

	longChunks := len(kitvec.Split(longContent, ix.split))
	require.Greater(t, longChunks, 1,
		"content must actually split into multiple chunks for this test to be meaningful")

	// Simulate the long document changing without a mirror refresh
	// reconciling the stamp, so it counts as pending again (same technique
	// as TestCountPendingIncludesRevisionChangedDocs).
	_, err = ix.db.ExecContext(ctx,
		`UPDATE vector_messages SET content_hash = 'changed-hash' WHERE doc_key = 'u:s1:u2'`)
	require.NoError(t, err)

	total, err := ix.countPending(ctx, fp)
	require.NoError(t, err)
	assert.EqualValues(t, longChunks, total,
		"Total must sum the pending document's chunks, not count it as one document")
}

func TestBuildProgressReceivesFinalDoneEqualToTotalChunks(t *testing.T) {
	previous := progressInterval
	progressInterval = 0
	t.Cleanup(func() { progressInterval = previous })

	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	gen := fakeGeneration("fake-model")

	var calls []BuildProgress
	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{
		Progress: func(p BuildProgress) { calls = append(calls, p) },
	})
	require.NoError(t, err)
	require.NotEmpty(t, calls)
	last := calls[len(calls)-1]
	assert.EqualValues(t, result.Fill.Chunks, last.Done)
	assert.EqualValues(t, result.Fill.Chunks, last.Total,
		"Total must also be in chunks so Done/Total settles at exactly 100%")
	assert.Equal(t, "embedding", last.Phase)
}

// TestBuildProgressNeverExceedsTotalWithMultiChunkMessage covers the
// regression this units fix addresses directly: a message long enough to
// split into several chunks must never push a progress call's Done past its
// Total (which would render as a percentage over 100%).
func TestBuildProgressNeverExceedsTotalWithMultiChunkMessage(t *testing.T) {
	previous := progressInterval
	progressInterval = 0
	t.Cleanup(func() { progressInterval = previous })

	ix := openTestIndex(t)
	ctx := context.Background()
	longContent := strings.Repeat("word ", 2000)
	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u1", Ordinal: 0, Content: "short"},
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", SourceUUID: "u2", Ordinal: 1, Content: longContent},
			endedAt: "2024-01-01T00:00:01Z",
		},
	}}
	gen := fakeGeneration("fake-model")
	require.Greater(t, len(kitvec.Split(longContent, ix.split)), 1,
		"content must actually split into multiple chunks for this test to be meaningful")

	var calls []BuildProgress
	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{
		Progress: func(p BuildProgress) { calls = append(calls, p) },
	})
	require.NoError(t, err)
	require.NotEmpty(t, calls)
	for _, p := range calls {
		assert.LessOrEqualf(t, p.Done, p.Total, "progress must never exceed 100%%: %+v", p)
	}
	last := calls[len(calls)-1]
	assert.EqualValues(t, result.Fill.Chunks, last.Done)
	assert.EqualValues(t, result.Fill.Chunks, last.Total)
}

func TestBuildEncoderErrorAbortsAndRetryResumesWithoutReembedding(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", Ordinal: 0, Content: "one"},
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", Ordinal: 1, Content: "bad"},
			endedAt: "2024-01-01T00:00:01Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", Ordinal: 2, Content: "three"},
			endedAt: "2024-01-01T00:00:02Z",
		},
	}}
	gen := fakeGeneration("fake-model")

	failOnBad := func(_ context.Context, texts []string) ([][]float32, error) {
		if slices.Contains(texts, "bad") {
			return nil, fmt.Errorf("encoder rejected input")
		}
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}

	_, err := ix.Build(ctx, src, failOnBad, gen, BuildOptions{})
	require.Error(t, err)

	var stampCount int
	require.NoError(t, ix.db.QueryRow(
		`SELECT COUNT(*) FROM message_vectors_stamps`,
	).Scan(&stampCount))
	assert.Equal(t, 1, stampCount, "only the document before the failing one was stamped")

	result, err := ix.Build(ctx, src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.Fill.Documents, "retry embeds only the two remaining documents")
	assert.True(t, result.Activated)
}

// TestBuildSkipsPermanentlyRejectedDocumentAndContinues is the fix-1
// regression test: a single document the endpoint permanently rejects
// (400) must not wedge the whole build. kit stamps it without vectors so
// the scan moves past it, later documents still embed, and the generation
// still auto-activates once every document — including the skipped one —
// is stamped.
func TestBuildSkipsPermanentlyRejectedDocumentAndContinues(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", Ordinal: 0, Content: "one"},
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", Ordinal: 1, Content: "poison"},
			endedAt: "2024-01-01T00:00:01Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", Ordinal: 2, Content: "three"},
			endedAt: "2024-01-01T00:00:02Z",
		},
	}}
	gen := fakeGeneration("fake-model")

	var calls int
	rejectPoison := func(_ context.Context, texts []string) ([][]float32, error) {
		calls++
		if slices.Contains(texts, "poison") {
			return nil, &HTTPStatusError{Status: http.StatusBadRequest, Body: "token window overflow"}
		}
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}

	result, err := ix.Build(ctx, src, rejectPoison, gen, BuildOptions{})
	require.NoError(t, err, "a permanently-rejected document must not abort the whole build")
	assert.Equal(t, 2, result.Fill.Documents, "the two good documents still embed")
	assert.Equal(t, 1, result.Fill.Skipped, "the poison document is counted as skipped")
	assert.True(t, result.Activated,
		"coverage is complete (every document stamped) once the poison doc is stamped-skipped")

	var stampCount int
	require.NoError(t, ix.db.QueryRow(
		`SELECT COUNT(*) FROM message_vectors_stamps`,
	).Scan(&stampCount))
	assert.Equal(t, 3, stampCount, "the skipped document is still stamped, just without vectors")

	// kit only re-embeds a skipped document once its content_hash changes;
	// a later build over unchanged content must not retry the encoder for
	// it at all.
	callsBefore := calls
	result2, err := ix.Build(ctx, src, rejectPoison, gen, BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, result2.Fill.Documents)
	assert.Equal(t, 0, result2.Fill.Skipped)
	assert.Equal(t, callsBefore, calls,
		"unchanged content must not re-invoke the encoder for the already-skipped document")
}

// TestBuild5xxEncodeErrorStillAbortsFill guards the other side of the OnEncodeError
// wiring: a transient (5xx) failure must still abort the whole fill rather
// than being skipped, since it is likely to succeed on a later retry and
// permanently giving up on the document would lose it from the index.
func TestBuild5xxEncodeErrorStillAbortsFill(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := &fakeMessageSource{rows: []fakeRow{
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", Ordinal: 0, Content: "one"},
			endedAt: "2024-01-01T00:00:00Z",
		},
		{
			msg:     db.EmbeddableMessage{SessionID: "s1", Ordinal: 1, Content: "bad"},
			endedAt: "2024-01-01T00:00:01Z",
		},
	}}
	gen := fakeGeneration("fake-model")

	fail500 := func(_ context.Context, texts []string) ([][]float32, error) {
		if slices.Contains(texts, "bad") {
			return nil, &HTTPStatusError{Status: http.StatusInternalServerError, Body: "boom"}
		}
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}

	_, err := ix.Build(ctx, src, fail500, gen, BuildOptions{})
	require.Error(t, err, "a transient (5xx) encode error must still abort the fill")
	var statusErr *HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusInternalServerError, statusErr.Status)
}

// TestResolveBuildTargetRetiresOtherBuildingGeneration is the fix-3
// regression test: a generation left in state building by an abandoned
// (config changed mid-build) first build must be retired once a new
// building generation is established, so fingerprintByState's ORDER BY
// ordinal LIMIT 1 lookup (used for both BuildingFingerprint and the
// building-percent shown to a caller with no active generation) resolves
// to the generation actually being built, not the abandoned one.
func TestResolveBuildTargetRetiresOtherBuildingGeneration(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()

	genA := fakeGeneration("model-a")
	fpA, err := ix.EnsureGeneration(ctx, genA, sqlitevec.StateBuilding)
	require.NoError(t, err)

	genB := fakeGeneration("model-b")
	target, wasBuilding, err := ix.resolveBuildTarget(ctx, genB, genB.Fingerprint(), false)
	require.NoError(t, err)
	assert.True(t, wasBuilding)
	assert.Equal(t, genB.Fingerprint(), target)

	gens, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, gens, 2)
	for _, g := range gens {
		switch g.Fingerprint {
		case fpA:
			assert.Equal(t, string(sqlitevec.StateRetired), g.State,
				"the abandoned generation must be retired, not left building forever")
		case target:
			assert.Equal(t, string(sqlitevec.StateBuilding), g.State)
		}
	}

	building, ok, err := ix.BuildingFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, target, building,
		"the building-generation lookup must resolve to B, not the abandoned A")
}

// TestBuildRetiresAbandonedBuildingGenerationEndToEnd drives the same
// scenario through the public Build entry point: an interrupted first
// build (genA registered as building but never filled, standing in for a
// crashed process) followed by a full Build under a different config
// (genB) must retire genA rather than leave two generations in state
// building.
func TestBuildRetiresAbandonedBuildingGenerationEndToEnd(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()

	genA := fakeGeneration("model-a")
	_, err := ix.Refresh(ctx, src, true, true)
	require.NoError(t, err)
	fpA, err := ix.EnsureGeneration(ctx, genA, sqlitevec.StateBuilding)
	require.NoError(t, err)

	genB := fakeGeneration("model-b")
	result, err := ix.Build(ctx, src, fakeBuildEncoder(), genB, BuildOptions{})
	require.NoError(t, err)
	assert.True(t, result.Activated)

	gens, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, gens, 2)
	var stateA, stateB string
	for _, g := range gens {
		switch g.Fingerprint {
		case fpA:
			stateA = g.State
		case genB.Fingerprint():
			stateB = g.State
		}
	}
	assert.Equal(t, string(sqlitevec.StateRetired), stateA, "abandoned generation A must be retired")
	assert.Equal(t, string(sqlitevec.StateActive), stateB)

	_, ok, err := ix.BuildingFingerprint(ctx)
	require.NoError(t, err)
	assert.False(t, ok, "no generation should remain in state building")
}

// TestBuildActiveFingerprintEarlyReturnRetiresAbandonedBuildingGeneration
// covers a gap in resolveBuildTarget's active-fingerprint early return: when
// the requested generation is already active, the target-resolution path
// never reaches EnsureGeneration, which is the only other place that retires
// abandoned building generations. Without also retiring on this path, a
// first build of some other fingerprint that registered as building and
// then failed (a crashed process, or config that got reverted back to the
// still-active fingerprint before the failed build could be retried) stays
// in state building forever once every subsequent build targets the active
// generation again.
func TestBuildActiveFingerprintEarlyReturnRetiresAbandonedBuildingGeneration(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	genA := fakeGeneration("model-a")

	_, err := ix.Build(ctx, src, fakeBuildEncoder(), genA, BuildOptions{})
	require.NoError(t, err, "genA is now active")

	// Simulate an abandoned first build of a different config: registered as
	// building, but never filled or retired (standing in for a crashed
	// process, or a config change that was reverted before the failed build
	// could be cleaned up).
	genB := fakeGeneration("model-b")
	fpB, err := ix.EnsureGeneration(ctx, genB, sqlitevec.StateBuilding)
	require.NoError(t, err)

	// Build again with genA, the still-active fingerprint: this must take
	// the active-fingerprint early-return path in resolveBuildTarget, not
	// the EnsureGeneration path.
	result, err := ix.Build(ctx, src, fakeBuildEncoder(), genA, BuildOptions{})
	require.NoError(t, err)
	assert.False(t, result.Activated, "already-active generation stays active without reactivation")

	gens, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, gens, 2)
	for _, g := range gens {
		if g.Fingerprint == fpB {
			assert.Equal(t, string(sqlitevec.StateRetired), g.State,
				"the abandoned building generation must be retired once a build "+
					"resolves back to the active fingerprint")
		}
	}

	_, ok, err := ix.BuildingFingerprint(ctx)
	require.NoError(t, err)
	assert.False(t, ok, "no generation should remain in state building")
}
