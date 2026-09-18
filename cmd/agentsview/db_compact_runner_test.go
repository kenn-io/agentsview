package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestForegroundCompactRunnerReturnsPopulatedResult(t *testing.T) {
	require := require.New(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(err)
	defer database.Close()
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{})
	defer engine.Close()

	result, err := newForegroundCompactRunner(engine, database)(
		t.Context(), db.CompactOptions{StagingDir: t.TempDir()},
	)
	require.NoError(err)
	require.Positive(result.Before.DatabaseBytes)
	require.Positive(result.After.DatabaseBytes)
}
