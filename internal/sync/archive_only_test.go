package sync_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	sessionsync "go.kenn.io/agentsview/internal/sync"
)

func TestArchiveOnlyRefusesReceivingHostSources(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	writeGeminiSession(t, root, "archived")
	cfg := sessionsync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}, Machine: "original-device"}
	collector := sessionsync.NewEngine(t.Context(), database, cfg)
	require.Equal(t, 1, collector.SyncAll(t.Context(), nil).Synced)
	collector.Close()
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "INSERT INTO archive_metadata (key, value) VALUES ('archive_only', '1')")
		return err
	}))
	writeGeminiSession(t, root, "receiving-host")
	engine := sessionsync.NewEngine(t.Context(), database, cfg)
	defer engine.Close()
	assert.True(t, engine.SyncAll(t.Context(), nil).Aborted)
	path := filepath.Join(root, "tmp", "project", "chats", "session-receiving-host.json")
	require.ErrorContains(t, engine.SyncPathsContext(t.Context(), []string{path}), "archive-only")
	require.ErrorContains(t, engine.SyncSingleSessionContext(t.Context(), "gemini:archived"), "archive-only")
	engine.ReconfigureSources(sessionsync.SourceConfig{AgentDirs: cfg.AgentDirs})
	require.ErrorContains(t, engine.ReconcileProviderRootsGrouped(t.Context(), []sessionsync.ProviderRootsGroup{{Agent: parser.AgentGemini, Roots: []string{root}}}), "archive-only")
	stats, err := engine.ResyncAllWithOptions(t.Context(), nil, sessionsync.RebuildOptions{})
	require.NoError(t, err)
	require.True(t, stats.ArchiveRebuilt)
	requireSessionState(t, database, "gemini:archived", true)
	requireSessionState(t, database, "gemini:receiving-host", false)
	var mode string
	require.NoError(t, database.Reader().QueryRow(t.Context(), "SELECT value FROM archive_metadata WHERE key='archive_only'").Scan(&mode))
	assert.Equal(t, "1", mode)
}
