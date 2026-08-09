package sync_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	sessionsync "go.kenn.io/agentsview/internal/sync"
)

func TestDisabledProviderPreservesArchivedSession(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := filepath.Join(t.TempDir(), "gemini")
	source := filepath.Join(root, "tmp", "chat", "session-existing.json")
	require.NoError(t, database.UpsertSession(db.Session{
		ID:       "archived-gemini-session",
		Project:  "archived-project",
		Machine:  "test-machine",
		Agent:    string(parser.AgentGemini),
		FilePath: &source,
	}))

	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGemini: {root},
		},
		SourceMachines: map[parser.AgentType]map[string]string{
			parser.AgentGemini: {root: "test-machine"},
		},
		DisabledAgents:   []parser.AgentType{parser.AgentGemini},
		LocalMachineName: "test-machine",
	}
	engine := sessionsync.NewEngine(database, sessionsync.EngineConfig{
		AgentDirs:      cfg.SyncAgentDirs(),
		SourceMachines: cfg.SyncSourceMachines(),
		Machine:        cfg.LocalMachineName,
	})
	t.Cleanup(engine.Close)

	stats := engine.SyncAll(t.Context(), nil)
	assert.Zero(t, stats.Synced)
	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), nil, true))

	stored, err := database.GetSessionFull(t.Context(), "archived-gemini-session")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Nil(t, stored.DeletedAt)
	assert.Nil(t, stored.DeletionCause)
	assert.Equal(t, source, *stored.FilePath)
}
