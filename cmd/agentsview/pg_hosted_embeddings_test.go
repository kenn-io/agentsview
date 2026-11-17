package main

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestHostedEmbeddingScheduleRetriesAcrossChanges(t *testing.T) {
	schedule := newHostedEmbeddingSchedule()
	schedule.delay.RandomizationFactor = 0
	schedule.delay.Multiplier = 2
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.True(t, schedule.ready(1, now))
	schedule.finish(1, now, errors.New("unavailable"))
	assert.False(t, schedule.ready(1, now.Add(5*time.Second)))
	assert.False(t, schedule.ready(2, now.Add(5*time.Second)))
	require.True(t, schedule.ready(2, now.Add(30*time.Second)))
	schedule.finish(2, now.Add(30*time.Second), errors.New("unavailable"))
	assert.False(t, schedule.ready(3, now.Add(89*time.Second)))
	assert.True(t, schedule.ready(3, now.Add(90*time.Second)))
	schedule.finish(3, now.Add(90*time.Second), nil)
	assert.False(t, schedule.ready(3, now.Add(2*time.Minute)))
	assert.True(t, schedule.ready(4, now.Add(2*time.Minute)))
}

func TestHostedEmbeddingsDisabledOrUsageOnly(t *testing.T) {
	for _, cfg := range []config.Config{
		{},
		{Vector: config.VectorConfig{Enabled: true}, ArchiveContent: config.ArchiveContentUsage},
	} {
		runtime, err := prepareHostedEmbeddings(t.Context(), cfg, nil, time.Second)
		require.NoError(t, err)
		assert.Nil(t, runtime)
	}
}
