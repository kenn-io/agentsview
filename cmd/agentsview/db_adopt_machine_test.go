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
	assert := assert.New(t)
	require := require.New(t)

	isolateDirectCLISources(t)
	dir := testDataDir(t)
	database, err := db.Open(filepath.Join(dir, "sessions.db"))
	require.NoError(err)
	const sessionID = "codebuff:project:1704067200"
	for id, machine := range map[string]string{
		sessionID: "oldhost.example", "older-session": "olderhost.example", "peer-session": "peer.example",
	} {
		require.NoError(database.UpsertSession(db.Session{
			ID: id, Machine: machine, Project: "project", Agent: "codebuff", UserMessageCount: 2,
		}))
	}
	require.NoError(database.Close())
	cfg, err := config.LoadMinimal()
	require.NoError(err)
	// Startup records the installation without claiming unowned history.
	database, err = openDB(cfg)
	require.NoError(err)
	history, err := database.GetSession(t.Context(), sessionID)
	require.NoError(err)
	assert.Equal("oldhost.example", history.Machine)
	require.NoError(database.Close())
	output, err := executeCommand(newRootCommand(), "db", "adopt-machine", "--list")
	require.NoError(err)
	assert.Contains(output, "oldhost.example")
	assert.Contains(output, "peer.example")
	_, err = executeCommand(newRootCommand(), "db", "adopt-machine", "misspelled.example")
	require.ErrorContains(err, "not recorded")
	_, err = executeCommand(newRootCommand(), "db", "adopt-machine", "oldhost.example", "olderhost.example")
	require.NoError(err)
	database, err = openDB(cfg)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })
	for _, id := range []string{sessionID, "older-session"} {
		session, err := database.GetSession(t.Context(), id)
		require.NoError(err)
		assert.Equal(cfg.InstallationID, session.Machine)
	}
	peer, err := database.GetSession(t.Context(), "peer-session")
	require.NoError(err)
	assert.Equal("peer.example", peer.Machine)
	resolved, err := resolveBareCodebuffID(t.Context(), service.NewDirectBackend(database, nil), &cfg, "1704067200", "local")
	require.NoError(err)
	assert.Equal(sessionID, resolved)
}

func TestDBAdoptMachineRequiresExplicitSelection(t *testing.T) {
	cmd := newDBAdoptMachineCommand()
	cmd.SetArgs(nil)
	require.ErrorContains(t, cmd.Execute(), "select one or more")
}
