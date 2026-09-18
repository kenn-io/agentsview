package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestOpenDBConfiguresArtifactLocalMachineOwnership(t *testing.T) {
	require := require.New(t)

	cfg := config.Config{
		DBPath:         filepath.Join(t.TempDir(), "sessions.db"),
		InstallationID: "workstation.example",
	}
	database, err := openDB(cfg)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })
	require.NoError(database.UpsertSession(db.Session{
		ID: "hostname-local", Project: "project",
		Machine: cfg.InstallationID, Agent: "claude",
	}))
	_, err = database.EnsureArtifactOrigin("desktop-a1b2c3")
	require.NoError(err)

	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(t, "hostname-local", pending[0].SessionID)
}
