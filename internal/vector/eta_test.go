package vector

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildETAEstimatorPublishesSmoothedRateAfterWarmup(t *testing.T) {
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	var estimator buildETAEstimator

	assert.False(t, estimator.sample("embedding", 0, 1000, base).Ready)
	assert.False(t, estimator.sample("embedding", 100, 1000, base.Add(2*time.Second)).Ready)

	got := estimator.sample("embedding", 200, 1000, base.Add(4*time.Second))
	require.True(t, got.Ready)
	assert.InDelta(t, 50, got.RatePerSecond, 0.001)
	assert.Equal(t, 16*time.Second, got.Remaining)

	assert.True(t, estimator.sample("embedding", 200, 1000, base.Add(6*time.Second)).Ready,
		"an unchanged observation must retain the current estimate")
	got = estimator.sample("embedding", 300, 1000, base.Add(8*time.Second))
	require.True(t, got.Ready)
	assert.InDelta(t, 42.5, got.RatePerSecond, 0.001,
		"the resumed delta must include the stalled interval in its instantaneous rate")
	assert.InDelta(t, time.Duration(16.470588*float64(time.Second)), got.Remaining,
		float64(time.Millisecond))
}

func TestBuildETAEstimatorResetsDiscontinuousProgress(t *testing.T) {
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		phase string
		done  int64
		total int64
	}{
		{name: "phase changes", phase: "scanning", done: 200, total: 1000},
		{name: "total changes", phase: "embedding", done: 200, total: 1200},
		{name: "counter regresses", phase: "embedding", done: 150, total: 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var estimator buildETAEstimator
			estimator.sample("embedding", 0, 1000, base)
			estimator.sample("embedding", 100, 1000, base.Add(time.Second))
			require.True(t, estimator.sample("embedding", 200, 1000, base.Add(2*time.Second)).Ready)

			got := estimator.sample(tt.phase, tt.done, tt.total, base.Add(3*time.Second))
			assert.False(t, got.Ready)
			assert.Zero(t, got.RatePerSecond)
			assert.Zero(t, got.Remaining)
		})
	}
}

func TestBuildETAEstimatorRequiresKnownTotalAndCanBeReset(t *testing.T) {
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	var estimator buildETAEstimator

	estimator.sample("scanning", 0, 0, base)
	estimator.sample("scanning", 100, 0, base.Add(time.Second))
	assert.False(t, estimator.sample("scanning", 200, 0, base.Add(2*time.Second)).Ready)

	estimator.reset()
	assert.False(t, estimator.sample("embedding", 200, 1000, base.Add(3*time.Second)).Ready,
		"the first observation after reset must establish a new baseline")
}
