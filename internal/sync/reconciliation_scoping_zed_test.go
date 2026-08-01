package sync_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// TestReconcileProviderRootsZedContainerPassReclaimsRemovedMember pins the
// container topology for the shared multi-session base at engine level, on a
// provider that is not OpenCode-family: a pass asked about the Zed threads.db
// itself proves the container's whole virtual membership, so a removed thread
// is reclaimed and a surviving thread keeps its session, instead of the
// bare-path proof admitting no member and completing as a no-op.
func TestReconcileProviderRootsZedContainerPassReclaimsRemovedMember(
	t *testing.T,
) {
	zedDir := t.TempDir()
	dbPath := filepath.Join(zedDir, "threads", "threads.db")
	createZedThreadsDB(t, dbPath, []zedThreadFixture{
		{
			id: "kept", summary: "Kept thread",
			updatedAt: "2026-06-09T02:30:00Z", dataType: "json",
			data: []byte(`{"messages":[]}`),
		},
		{
			id: "removed", summary: "Removed thread",
			updatedAt: "2026-06-09T02:31:00Z", dataType: "json",
			data: []byte(`{"messages":[]}`),
		},
	})
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentZed: {zedDir}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)

	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec("DELETE FROM threads WHERE id = 'removed'")
	require.NoError(t, conn.Close())
	require.NoError(t, err)

	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), parser.AgentZed, []string{dbPath},
	))

	removed, err := database.GetSession(t.Context(), "zed:removed")
	require.NoError(t, err)
	assert.Nil(t, removed,
		"a container-scoped pass reclaims a removed member")
	kept, err := database.GetSession(t.Context(), "zed:kept")
	require.NoError(t, err)
	assert.NotNil(t, kept, "a surviving member keeps its session")
}
