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
	assert := assert.New(t)
	require := require.New(t)

	cfg := enabledVectorConfig(t)
	cfg.DBPath = filepath.Join(cfg.DataDir, "sessions.db")
	buildTestVectorsDB(t, cfg)
	// Recall has an independent store in the same vector database.
	recall, err := vector.OpenSpec(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), vector.RecallIndexSpec(), false, cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(err)
	_, err = recall.Build(t.Context(), testPushUnitSource(), fakePushEncoder(), kitvec.Generation{Model: "fake-model", Dimensions: 4}, vector.BuildOptions{})
	require.NoError(err)
	require.NoError(recall.Close())
	cfg.ArchiveContent = config.ArchiveContentUsage
	cfg.Vector.Enabled = false
	database, err := openDB(cfg)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })
	// Usage-only gating also applies when vectors are explicitly enabled.
	cfg.Vector.Enabled = true
	require.ErrorIs(requireVectorEnabled(cfg), db.ErrArchiveContentExcluded)
	assert.Nil(newVectorPushSource(cfg))
	assert.Nil(installDirectVectorSearcher(cfg, database))
	serving, err := setupVectorServing(t.Context(), cfg, database, nil)
	require.NoError(err)
	assert.Nil(serving.Scheduler)
	// Read the actual previously built generation: no old content can be exported.
	for _, spec := range []vector.IndexSpec{vector.MessageIndexSpec(), vector.RecallIndexSpec()} {
		ix, err := vector.OpenSpec(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), spec, true, cfg.Vector.Embeddings.MaxInputChars)
		require.NoError(err)
		export, ok, err := ix.BeginExport(t.Context(), nil)
		require.NoError(err)
		require.True(ok)
		docs, _, err := export.SessionDocs(t.Context(), "session-1")
		require.NoError(err)
		assert.Empty(docs)
		require.NoError(export.Close())
		require.NoError(ix.Close())
	}
}
