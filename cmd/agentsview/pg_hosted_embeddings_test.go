package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/vector"
)

func TestHostedEmbeddingsDisabledWithoutContent(t *testing.T) {
	for _, cfg := range []config.Config{
		{},
		{Vector: config.VectorConfig{Enabled: true}, ArchiveContent: config.ArchiveContentUsage},
	} {
		runtime, err := prepareHostedEmbeddings(t.Context(), cfg, config.PGConfig{}, nil, time.Second)
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
