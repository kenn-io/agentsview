package vector

import (
	"context"
	"fmt"
	"slices"
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
	assert.Equal(t, "embedding", last.Phase)
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
