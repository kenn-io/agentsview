package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/vector"
)

func TestHostedEmbeddingScheduleRetriesAndWakesForChanges(t *testing.T) {
	schedule := newHostedEmbeddingSchedule()
	schedule.delay.RandomizationFactor = 0
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.True(t, schedule.ready(1, now))
	schedule.finish(1, now, errors.New("unavailable"))
	assert.False(t, schedule.ready(1, now.Add(5*time.Second)))
	assert.True(t, schedule.ready(2, now.Add(5*time.Second)))
	schedule.finish(2, now.Add(5*time.Second), errors.New("unavailable"))
	assert.False(t, schedule.ready(2, now.Add(34*time.Second)))
	assert.True(t, schedule.ready(2, now.Add(35*time.Second)))
	schedule.finish(2, now.Add(35*time.Second), nil)
	assert.False(t, schedule.ready(2, now.Add(time.Minute)))
}

func TestHostedEmbeddingsDisabledWithoutContent(t *testing.T) {
	for _, cfg := range []config.Config{
		{},
		{Vector: config.VectorConfig{Enabled: true}, ArchiveContent: config.ArchiveContentUsage},
	} {
		runtime, err := prepareHostedEmbeddings(t.Context(), cfg, nil, time.Second)
		require.NoError(t, err)
		assert.Nil(t, runtime)
	}
}

func TestHostedEmbeddingsStopJoinsEncoding(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	runtime := newPGRawRuntime(t.Context(), time.Hour, func(ctx context.Context) {
		defer close(exited)
		_, _ = vector.EncodeUnitDocuments(ctx, []db.EmbeddableUnit{{SessionID: "s", Kind: "user", Content: "hello"}}, 128, func(ctx context.Context, _ []string) ([][]float32, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	})
	runtime.Start()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		runtime.Stop()
		require.FailNow(t, "encoder did not start")
	}
	runtime.Stop()
	select {
	case <-exited:
	default:
		require.FailNow(t, "runtime stopped before encoding exited")
	}
}
