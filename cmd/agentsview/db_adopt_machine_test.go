package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestDBAdoptMachineRepairsSelectedHistoryAndBareCodebuffLookup(t *testing.T) {
	isolateDirectCLISources(t)
	dir := testDataDir(t)
	database, err := db.Open(filepath.Join(dir, "sessions.db"))
	require.NoError(t, err)
	const sessionID = "codebuff:project:1704067200"
	for id, machine := range map[string]string{
		sessionID: "oldhost.example", "older-session": "olderhost.example", "peer-session": "peer.example",
	} {
		require.NoError(t, database.UpsertSession(db.Session{
			ID: id, Machine: machine, Project: "project", Agent: "codebuff", UserMessageCount: 2,
		}))
	}
	require.NoError(t, database.Close())
	cfg, err := config.LoadMinimal()
	require.NoError(t, err)
	_, err = openDB(cfg)
	require.ErrorIs(t, err, db.ErrMachineOwnershipRequired)
	// The suggested inspection command works before writable startup can run.
	output, err := executeCommand(newRootCommand(), "db", "adopt-machine", "--list")
	require.NoError(t, err)
	assert.Contains(t, output, "oldhost.example")
	assert.Contains(t, output, "peer.example")
	_, err = executeCommand(newRootCommand(), "db", "adopt-machine", "misspelled.example")
	require.ErrorContains(t, err, "not recorded")
	_, err = executeCommand(newRootCommand(), "db", "adopt-machine", "oldhost.example", "olderhost.example")
	require.NoError(t, err)
	database, err = openDB(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	for _, id := range []string{sessionID, "older-session"} {
		session, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, cfg.InstallationID, session.Machine)
	}
	peer, err := database.GetSession(t.Context(), "peer-session")
	require.NoError(t, err)
	assert.Equal(t, "peer.example", peer.Machine)
	resolved, err := resolveBareCodebuffID(t.Context(), service.NewDirectBackend(database, nil), &cfg, "1704067200", "local")
	require.NoError(t, err)
	assert.Equal(t, sessionID, resolved)
}

func TestDBAdoptMachineRequiresExplicitSelection(t *testing.T) {
	for _, args := range [][]string{nil, {"--no-local-sessions", "oldhost.example"}} {
		cmd := newDBAdoptMachineCommand()
		cmd.SetArgs(args)
		require.ErrorContains(t, cmd.Execute(), "select one or more")
	}
}
