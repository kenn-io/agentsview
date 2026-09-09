package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestUsageOnlyClearsExistingVectorContent(t *testing.T) {
	cfg := enabledVectorConfig(t)
	cfg.DBPath = filepath.Join(cfg.DataDir, "sessions.db")
	buildTestVectorsDB(t, cfg)
	// Recall has an independent store in the same vector database.
	recall, err := vector.OpenSpec(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), vector.RecallIndexSpec(), false, cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(t, err)
	_, err = recall.Build(t.Context(), testPushUnitSource(), fakePushEncoder(), kitvec.Generation{Model: "fake-model", Dimensions: 4}, vector.BuildOptions{})
	require.NoError(t, err)
	require.NoError(t, recall.Close())
	cfg.ArchiveContent = config.ArchiveContentUsage
	cfg.Vector.Enabled = false
	database, err := openDB(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	// Usage-only gating also applies when vectors are explicitly enabled.
	cfg.Vector.Enabled = true
	require.ErrorIs(t, requireVectorEnabled(cfg), db.ErrArchiveContentExcluded)
	assert.Nil(t, newVectorPushSource(cfg))
	assert.Nil(t, installDirectVectorSearcher(cfg, database))
	serving, err := setupVectorServing(t.Context(), cfg, database, nil)
	require.NoError(t, err)
	assert.Nil(t, serving.Scheduler)
	// Read the actual previously built generation: no old content can be exported.
	for _, spec := range []vector.IndexSpec{vector.MessageIndexSpec(), vector.RecallIndexSpec()} {
		ix, err := vector.OpenSpec(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), spec, true, cfg.Vector.Embeddings.MaxInputChars)
		require.NoError(t, err)
		export, ok, err := ix.BeginExport(t.Context(), nil)
		require.NoError(t, err)
		require.True(t, ok)
		docs, _, err := export.SessionDocs(t.Context(), "session-1")
		require.NoError(t, err)
		assert.Empty(t, docs)
		require.NoError(t, export.Close())
		require.NoError(t, ix.Close())
	}
}
