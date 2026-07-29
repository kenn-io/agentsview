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
	cfg := config.Config{
		DBPath:           filepath.Join(t.TempDir(), "sessions.db"),
		LocalMachineName: "workstation.example",
	}
	database, err := openDB(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "hostname-local", Project: "project",
		Machine: cfg.LocalMachineName, Agent: "claude",
	}))
	_, err = database.EnsureArtifactOrigin("desktop-a1b2c3")
	require.NoError(t, err)

	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "hostname-local", pending[0].SessionID)
}
