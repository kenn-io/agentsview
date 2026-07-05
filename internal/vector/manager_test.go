package vector

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// blockingEncoder returns an encoder that blocks until release is closed,
// letting tests observe a Manager while its build is still in flight.
func blockingEncoder(release <-chan struct{}) kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		<-release
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}
}

// waitFor polls cond until it returns true or the deadline passes, failing
// the test otherwise. Used instead of a fixed sleep to avoid flakiness.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.Fail(t, "timed out waiting for condition", msg)
}

// generationIDByFingerprint looks up a generation's CLI-facing ordinal ID
// from its fingerprint, for tests that need to Activate/Retire a specific
// generation by ID.
func generationIDByFingerprint(t *testing.T, ix *Index, fp string) int64 {
	t.Helper()
	gens, err := ix.Generations(context.Background())
	require.NoError(t, err)
	for _, g := range gens {
		if g.Fingerprint == fp {
			return g.ID
		}
	}
	require.Fail(t, "generation not found for fingerprint", fp)
	return 0
}

func TestManagerStartBuildSetsRunningAndConcurrentStartReturnsErrBuildRunning(t *testing.T) {
	ix := openTestIndex(t)
	src := twoDocSource()
	gen := fakeGeneration("fake-model")
	release := make(chan struct{})
	m := NewManager(ix, src, blockingEncoder(release), gen, 10)

	require.NoError(t, m.StartBuild(BuildRequest{}))
	waitFor(t, func() bool { return m.Status().Running }, "build never reported running")

	err := m.StartBuild(BuildRequest{})
	assert.ErrorIs(t, err, ErrBuildRunning)

	close(release)
	waitFor(t, func() bool { return !m.Status().Running }, "build never finished")
	assert.Empty(t, m.Status().LastError)
}

func TestManagerTryBuildReturnsFalseWhileRunning(t *testing.T) {
	ix := openTestIndex(t)
	src := twoDocSource()
	gen := fakeGeneration("fake-model")
	release := make(chan struct{})
	m := NewManager(ix, src, blockingEncoder(release), gen, 10)

	require.NoError(t, m.StartBuild(BuildRequest{}))
	waitFor(t, func() bool { return m.Status().Running }, "build never reported running")

	started, err := m.TryBuild(context.Background(), BuildRequest{})
	assert.False(t, started, "TryBuild must drop rather than queue while running")
	assert.NoError(t, err)

	close(release)
	waitFor(t, func() bool { return !m.Status().Running }, "build never finished")
}

func TestManagerStatusTransitionsToLastResultOnCompletion(t *testing.T) {
	ix := openTestIndex(t)
	src := twoDocSource()
	gen := fakeGeneration("fake-model")
	m := NewManager(ix, src, fakeBuildEncoder(), gen, 10)

	require.NoError(t, m.StartBuild(BuildRequest{}))
	waitFor(t, func() bool { return !m.Status().Running }, "build never finished")

	status := m.Status()
	require.NotNil(t, status.LastResult)
	assert.Equal(t, gen.Fingerprint(), status.LastResult.Fingerprint)
	assert.True(t, status.LastResult.Activated)
	assert.Empty(t, status.LastError)
}

func TestManagerStatusSetsLastErrorOnEncoderFailure(t *testing.T) {
	ix := openTestIndex(t)
	src := twoDocSource()
	gen := fakeGeneration("fake-model")
	failingEncoder := func(_ context.Context, _ []string) ([][]float32, error) {
		return nil, fmt.Errorf("encoder rejected input")
	}
	m := NewManager(ix, src, failingEncoder, gen, 10)

	require.NoError(t, m.StartBuild(BuildRequest{}))
	waitFor(t, func() bool { return !m.Status().Running }, "build never finished")

	status := m.Status()
	assert.Contains(t, status.LastError, "encoder rejected input")
	assert.Nil(t, status.LastResult)
}

func TestManagerGenerationsDelegatesToIndex(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	gen := fakeGeneration("fake-model")
	m := NewManager(ix, src, fakeBuildEncoder(), gen, 10)

	_, err := m.TryBuild(ctx, BuildRequest{})
	require.NoError(t, err)

	want, err := ix.Generations(ctx)
	require.NoError(t, err)
	got, err := m.Generations(ctx)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestManagerActivateForceRefusalMatrix(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	genA := fakeGeneration("model-a")
	m := NewManager(ix, src, fakeBuildEncoder(), genA, 10)

	_, err := m.TryBuild(ctx, BuildRequest{})
	require.NoError(t, err, "genA becomes active")

	genB := fakeGeneration("model-b")
	fpB, err := ix.EnsureGeneration(ctx, genB, sqlitevec.StateBuilding)
	require.NoError(t, err, "genB registered but never filled, so it has Missing > 0")
	idB := generationIDByFingerprint(t, ix, fpB)

	err = m.Activate(ctx, idB, false)
	require.Error(t, err, "refuses activation of an incompletely embedded generation")
	assert.Contains(t, err.Error(), fmt.Sprintf("generation %d still has", idB))
	assert.Contains(t, err.Error(), "use --force")

	require.NoError(t, m.Activate(ctx, idB, true), "force overrides the refusal")

	active, ok, err := ix.ActiveFingerprint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, fpB, active, "genB is now active")

	idA := generationIDByFingerprint(t, ix, genA.Fingerprint())
	gens, err := ix.Generations(ctx)
	require.NoError(t, err)
	var stateA string
	for _, g := range gens {
		if g.ID == idA {
			stateA = g.State
		}
	}
	assert.Equal(t, string(sqlitevec.StateRetired), stateA, "activating genB retires the old active genA")
}

func TestManagerRetireRefusesActiveGenerationWithoutForce(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	gen := fakeGeneration("fake-model")
	m := NewManager(ix, src, fakeBuildEncoder(), gen, 10)

	_, err := m.TryBuild(ctx, BuildRequest{})
	require.NoError(t, err)
	id := generationIDByFingerprint(t, ix, gen.Fingerprint())

	err = m.Retire(ctx, id, false)
	require.Error(t, err, "refuses retiring the active generation without force")

	require.NoError(t, m.Retire(ctx, id, true), "force overrides the refusal")

	gens, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, gens, 1)
	assert.Equal(t, string(sqlitevec.StateRetired), gens[0].State)
}

func TestManagerActivateAndRetireRefuseWhileBuildRunning(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	src := twoDocSource()
	gen := fakeGeneration("fake-model")
	release := make(chan struct{})
	m := NewManager(ix, src, blockingEncoder(release), gen, 10)

	require.NoError(t, m.StartBuild(BuildRequest{}))
	waitFor(t, func() bool { return m.Status().Running }, "build never reported running")

	err := m.Activate(ctx, 1, true)
	assert.ErrorIs(t, err, ErrBuildRunning)

	err = m.Retire(ctx, 1, true)
	assert.ErrorIs(t, err, ErrBuildRunning)

	close(release)
	waitFor(t, func() bool { return !m.Status().Running }, "build never finished")
}
