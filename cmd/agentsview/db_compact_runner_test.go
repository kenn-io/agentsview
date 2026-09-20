package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestForegroundCompactRunnerReturnsPopulatedResult(t *testing.T) {
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer database.Close()
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{})
	defer engine.Close()

	result, err := newForegroundCompactRunner(engine, database)(
		t.Context(), db.CompactOptions{StagingDir: t.TempDir()},
	)
	require.NoError(t, err)
	require.Positive(t, result.Before.DatabaseBytes)
	require.Positive(t, result.After.DatabaseBytes)
}
