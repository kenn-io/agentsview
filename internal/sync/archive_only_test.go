package sync_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
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

func TestArchiveOnlyPromptLinkUpgradeRebuildsWithoutSources(t *testing.T) {
	ctx := t.Context()
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSessionWithMessages(t, database, "retained", "project", []db.Message{dbtest.UserMsg("retained", 0, "https://github.com/o/r/pull/42")})
	require.NoError(t, database.EnableArchiveOnly(ctx))
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET pr_links=structured_pr_links WHERE id='retained'`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM stats WHERE key='prompt_pr_links_backfill_v1'`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, (1<<20)+183))
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	database, err := db.OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	engine := sessionsync.NewEngine(ctx, database, sessionsync.EngineConfig{})
	defer engine.Close()
	assert.True(t, engine.SyncAll(ctx, nil).Aborted)
	stats, err := engine.ResyncAllWithOptions(ctx, nil, sessionsync.RebuildOptions{})
	require.NoError(t, err)
	require.True(t, stats.ArchiveRebuilt)
	needs, rebuild, err := db.ArchiveResyncState(ctx, path)
	require.NoError(t, err)
	assert.False(t, needs)
	assert.False(t, rebuild)
}
