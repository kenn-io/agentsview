package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

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
