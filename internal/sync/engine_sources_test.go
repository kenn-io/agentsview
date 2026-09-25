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

func writeGeminiSession(t *testing.T, root, id string) {
	t.Helper()
	path := filepath.Join(root, "tmp", "project", "chats", "session-"+id+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.GeminiSessionJSON(
		id, "project", "2026-08-09T10:00:00Z", "2026-08-09T10:01:00Z",
		[]map[string]any{testjsonl.GeminiUserMsg(
			"user", "2026-08-09T10:00:00Z", "hello from "+id,
		)},
	)), 0o644))
}

func requireSessionState(
	t *testing.T, database *db.DB, id string, wantLive bool,
) {
	t.Helper()
	stored, err := database.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	if !wantLive {
		assert.Nil(t, stored, "session %s should not be imported", id)
		return
	}
	require.NotNil(t, stored, "session %s should be imported", id)
	assert.Nil(t, stored.DeletedAt, "session %s should stay live", id)
}

func TestReconfigureSourcesAppliesWithoutNewEngine(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	base := t.TempDir()
	primary := filepath.Join(base, "gemini")
	alternate := filepath.Join(base, "gemini-work")
	writeGeminiSession(t, primary, "primary")
	writeGeminiSession(t, alternate, "alternate")

	engine := sessionsync.NewEngine(t.Context(), database, sessionsync.EngineConfig{
		AgentDirs:      map[parser.AgentType][]string{parser.AgentGemini: {primary}},
		DisabledAgents: []parser.AgentType{parser.AgentGemini},
		Machine:        "test-machine",
	})
	t.Cleanup(engine.Close)

	engine.SyncAll(t.Context(), nil)
	requireSessionState(t, database, "gemini:primary", false)

	// Enabling the provider takes effect on the same engine.
	engine.ReconfigureSources(sessionsync.SourceConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {primary}},
	})
	engine.SyncAll(t.Context(), nil)
	requireSessionState(t, database, "gemini:primary", true)
	requireSessionState(t, database, "gemini:alternate", false)

	// Adding a root makes its sessions discoverable.
	engine.ReconfigureSources(sessionsync.SourceConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGemini: {primary, alternate},
		},
	})
	require.NoError(t, engine.ReconcileProviderRootsGrouped(
		t.Context(), []sessionsync.ProviderRootsGroup{{
			Agent: parser.AgentGemini, Roots: []string{alternate},
		}},
	))
	requireSessionState(t, database, "gemini:alternate", true)
	assert.Equal(t, []string{primary, alternate},
		engine.ReconciliationRootsForAgent(string(parser.AgentGemini)))

	// Disabling it again stops imports but keeps archived sessions.
	engine.ReconfigureSources(sessionsync.SourceConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGemini: {primary, alternate},
		},
		DisabledAgents: []parser.AgentType{parser.AgentGemini},
	})
	writeGeminiSession(t, primary, "after-disable")
	engine.SyncAll(t.Context(), nil)
	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), nil, true))
	requireSessionState(t, database, "gemini:after-disable", false)
	requireSessionState(t, database, "gemini:primary", true)
	requireSessionState(t, database, "gemini:alternate", true)
}
