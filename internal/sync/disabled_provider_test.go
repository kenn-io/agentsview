package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	sessionsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestDisabledProviderPreservesArchivedSession(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := filepath.Join(t.TempDir(), "gemini")
	source := filepath.Join(root, "tmp", "chat", "session-existing.json")
	newSource := filepath.Join(root, "tmp", "project", "chats", "session-new.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(newSource), 0o755))
	require.NoError(t, os.WriteFile(newSource, []byte(testjsonl.GeminiSessionJSON(
		"new-local", "project", "2026-08-09T10:00:00Z", "2026-08-09T10:01:00Z",
		[]map[string]any{testjsonl.GeminiUserMsg(
			"user", "2026-08-09T10:00:00Z", "do not import locally",
		)},
	)), 0o644))
	require.NoError(t, database.UpsertSession(db.Session{
		ID:       "archived-gemini-session",
		Project:  "archived-project",
		Machine:  "test-machine",
		Agent:    string(parser.AgentGemini),
		FilePath: &source,
	}))

	engine := sessionsync.NewEngine(database, sessionsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGemini: {root},
		},
		SourceMachines: map[parser.AgentType]map[string]string{
			parser.AgentGemini: {root: "test-machine"},
		},
		DisabledAgents: []parser.AgentType{parser.AgentGemini},
		Machine:        "test-machine",
	})
	t.Cleanup(engine.Close)

	stats := engine.SyncAll(t.Context(), nil)
	assert.Zero(t, stats.Synced)
	engine.SyncPaths([]string{newSource})
	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), nil, true))
	imported, err := database.GetSessionFull(t.Context(), "gemini:new-local")
	require.NoError(t, err)
	assert.Nil(t, imported)

	stored, err := database.GetSessionFull(t.Context(), "archived-gemini-session")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Nil(t, stored.DeletedAt)
	assert.Nil(t, stored.DeletionCause)
	assert.Equal(t, source, *stored.FilePath)
}

func TestDisabledProviderPreservesArchivedSessionAcrossRebuild(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	base := t.TempDir()
	geminiRoot := filepath.Join(base, "gemini")
	geminiSource := filepath.Join(
		geminiRoot, "tmp", "project", "chats", "session-existing.json",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(geminiSource), 0o755))
	require.NoError(t, os.WriteFile(geminiSource, []byte(`{"sessionId":"old"}`), 0o644))
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "archived-gemini-rebuild", Project: "archived-project",
		Machine: "test-machine", Agent: string(parser.AgentGemini),
		FilePath: &geminiSource, MessageCount: 1, UserMessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages([]db.Message{{
		SessionID: "archived-gemini-rebuild", Ordinal: 0,
		Role: "user", Content: "preserve this archived message",
	}}))

	engine := sessionsync.NewEngine(database, sessionsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGemini: {geminiRoot},
		},
		DisabledAgents: []parser.AgentType{parser.AgentGemini},
		Machine:        "test-machine",
	})
	t.Cleanup(engine.Close)

	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "rebuild warnings: %v", stats.Warnings)

	stored, err := database.GetSessionFull(t.Context(), "archived-gemini-rebuild")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Nil(t, stored.DeletedAt)
	assert.Equal(t, geminiSource, *stored.FilePath)
	messages, err := database.GetMessages(
		t.Context(), "archived-gemini-rebuild", 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "preserve this archived message", messages[0].Content)
}
