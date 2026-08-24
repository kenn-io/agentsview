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
	assert.NotNil(t, removed,
		"a container-scoped pass reclaims a removed member")
	kept, err := database.GetSession(t.Context(), "zed:kept")
	require.NoError(t, err)
	assert.NotNil(t, kept, "a surviving member keeps its session")
}

// TestReconcileProviderRootsZedContainerPassPreservesMovedMember pins the
// relocation guard for persistent-archive members under scoped container
// authority: a thread that moved from the requested container to another
// configured root's container is a move, not a deletion, so a pass scoped to
// the source container must preserve its session even though its own stream
// no longer yields the member.
func TestReconcileProviderRootsZedContainerPassPreservesMovedMember(
	t *testing.T,
) {
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	firstDB := filepath.Join(firstDir, "threads", "threads.db")
	secondDB := filepath.Join(secondDir, "threads", "threads.db")
	moved := zedThreadFixture{
		id: "moved", summary: "Moved thread",
		updatedAt: "2026-06-09T02:30:00Z", dataType: "json",
		data: []byte(`{"messages":[]}`),
	}
	createZedThreadsDB(t, firstDB, []zedThreadFixture{moved})
	createZedThreadsDB(t, secondDB, nil)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentZed: {firstDir, secondDir},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	// The thread moves between containers; only the source container is
	// reconciled, so the pass cannot see the destination's membership.
	firstConn, err := sql.Open("sqlite3", firstDB)
	require.NoError(t, err)
	_, err = firstConn.Exec("DELETE FROM threads WHERE id = 'moved'")
	require.NoError(t, firstConn.Close())
	require.NoError(t, err)
	secondConn, err := sql.Open("sqlite3", secondDB)
	require.NoError(t, err)
	_, err = secondConn.Exec(`INSERT INTO threads (
		id, summary, updated_at, data_type, data,
		parent_id, folder_paths, created_at
	) VALUES ('moved', 'Moved thread', '2026-06-09T02:32:00Z', 'json',
		'{"messages":[]}', NULL, '', '')`)
	require.NoError(t, secondConn.Close())
	require.NoError(t, err)

	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), parser.AgentZed, []string{firstDB},
	))

	survivor, err := database.GetSession(t.Context(), "zed:moved")
	require.NoError(t, err)
	assert.NotNil(t, survivor,
		"a member found under another configured root is a move, not a deletion")
}
