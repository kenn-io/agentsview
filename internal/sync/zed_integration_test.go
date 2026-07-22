package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestSyncSingleSessionZedUsesVirtualSourcePath(t *testing.T) {

	zedDir := t.TempDir()
	dbPath := filepath.Join(zedDir, "threads", "threads.db")
	createZedThreadsDB(t, dbPath, []zedThreadFixture{
		{
			id:        "exists",
			summary:   "Existing thread",
			updatedAt: "2026-06-09T02:30:00Z",
			dataType:  "json",
			data:      []byte(`{"messages":[{"User":{"content":[{"Text":"hello"}]}}]}`),
		},
		{
			id:        "other",
			summary:   "Other thread",
			updatedAt: "2026-06-09T02:31:00Z",
			dataType:  "json",
			data:      []byte(`{"messages":[{"User":{"content":[{"Text":"skip"}]}}]}`),
		},
	})

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentZed: {zedDir},
		},
		Machine: "local",
	})

	source := engine.FindSourceFile("zed:exists")
	assert.Equal(t, dbPath+"#exists", source)
	require.NoError(t, engine.SyncSingleSession("zed:exists"))

	exists, err := database.GetSession(t.Context(), "zed:exists")
	require.NoError(t, err)
	require.NotNil(t, exists)
	assert.Equal(t, 1, exists.MessageCount)
	filePath := database.GetSessionFilePath("zed:exists")
	assert.Equal(t, dbPath+"#exists", filePath)

	other, err := database.GetSession(t.Context(), "zed:other")
	require.NoError(t, err)
	assert.Nil(t, other)
}

func TestSyncSingleSessionZedForceRewritesUnchangedSession(t *testing.T) {

	zedDir := t.TempDir()
	dbPath := filepath.Join(zedDir, "threads", "threads.db")
	createZedThreadsDB(t, dbPath, []zedThreadFixture{{
		id:        "exists",
		summary:   "Existing thread",
		updatedAt: "2026-06-09T02:30:00Z",
		dataType:  "json",
		data:      []byte(`{"messages":[{"User":{"content":[{"Text":"hello"}]}}]}`),
	}})

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentZed: {zedDir},
		},
		Machine: "local",
	})
	require.NoError(t, engine.SyncSingleSession("zed:exists"))
	sess, err := database.GetSession(t.Context(), "zed:exists")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 1, sess.MessageCount)

	sess.MessageCount = 0
	require.NoError(t, database.UpsertSession(*sess))

	require.NoError(t, engine.SyncSingleSession("zed:exists"))

	sess, err = database.GetSession(t.Context(), "zed:exists")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 1, sess.MessageCount)
	assert.Equal(t, dbPath+"#exists", database.GetSessionFilePath("zed:exists"))
}

func TestSyncPathsZedDeletedPhysicalDBPreservesSessions(t *testing.T) {

	zedDir := t.TempDir()
	dbPath := filepath.Join(zedDir, "threads", "threads.db")
	createZedThreadsDB(t, dbPath, []zedThreadFixture{{
		id:        "exists",
		summary:   "Existing thread",
		updatedAt: "2026-06-09T02:30:00Z",
		dataType:  "json",
		data:      []byte(`{"messages":[{"User":{"content":[{"Text":"hello"}]}}]}`),
	}})

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentZed: {zedDir},
		},
		Machine: "local",
	})
	stats := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)
	require.NoError(t, os.Remove(dbPath))

	engine.SyncPaths([]string{dbPath})

	// The SQLite store is a persistent archive: removing the backing DB file
	// must not delete the already-synced session.
	sess, err := database.GetSession(t.Context(), "zed:exists")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "zed:exists", sess.ID)
}

func TestReconcileWatchRootsZedDeletedPhysicalDBPreservesSessions(t *testing.T) {
	zedDir := t.TempDir()
	dbPath := filepath.Join(zedDir, "threads", "threads.db")
	createZedThreadsDB(t, dbPath, []zedThreadFixture{{
		id:        "archived-thread",
		summary:   "Archived thread",
		updatedAt: "2026-06-09T02:30:00Z",
		dataType:  "json",
		data:      []byte(`{"messages":[{"User":{"content":[{"Text":"hello"}]}}]}`),
	}})
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentZed: {zedDir},
		},
		Machine: "local",
	})
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, os.Remove(dbPath))

	require.NoError(t, engine.ReconcileWatchRoots(
		t.Context(), []string{zedDir}, false,
	))

	sess, err := database.GetSession(t.Context(), "zed:archived-thread")
	require.NoError(t, err)
	assert.NotNil(t, sess,
		"reconciliation must preserve an archived Zed member when threads.db vanishes")
}

func TestReconcileWatchRootsZedDeletedMemberTombstonesSession(t *testing.T) {
	zedDir := t.TempDir()
	dbPath := filepath.Join(zedDir, "threads", "threads.db")
	createZedThreadsDB(t, dbPath, []zedThreadFixture{
		{
			id: "deleted-thread", summary: "Deleted thread",
			updatedAt: "2026-06-09T02:30:00Z", dataType: "json",
			data: []byte(`{"messages":[{"User":{"content":[{"Text":"gone"}]}}]}`),
		},
		{
			id: "surviving-thread", summary: "Surviving thread",
			updatedAt: "2026-06-09T02:31:00Z", dataType: "json",
			data: []byte(`{"messages":[{"User":{"content":[{"Text":"kept"}]}}]}`),
		},
	})
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentZed: {zedDir}},
		Machine:   "local",
	})
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	beforeDelete, err := database.GetSessionFull(t.Context(), "zed:deleted-thread")
	require.NoError(t, err)
	require.NotNil(t, beforeDelete)

	zedDB, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = zedDB.Exec("DELETE FROM threads WHERE id = ?", "deleted-thread")
	require.NoError(t, err)
	require.NoError(t, zedDB.Close())

	require.NoError(t, engine.ReconcileWatchRoots(
		t.Context(), []string{zedDir}, false,
	))

	deleted, err := database.GetSession(t.Context(), "zed:deleted-thread")
	require.NoError(t, err)
	assert.Nil(t, deleted,
		"a present authoritative container must retire a missing virtual member")
	archived, err := database.GetSessionFull(t.Context(), "zed:deleted-thread")
	require.NoError(t, err)
	require.NotNil(t, archived)
	require.NotNil(t, archived.DeletedAt)
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)
	assert.Equal(t, beforeDelete.MessageCount, archived.MessageCount,
		"source loss must retain the archived transcript")
	surviving, err := database.GetSession(t.Context(), "zed:surviving-thread")
	require.NoError(t, err)
	assert.NotNil(t, surviving)
}

func TestSyncSingleSessionZedMissingThreadReturnsNotFound(t *testing.T) {

	zedDir := t.TempDir()
	createZedThreadsDB(t, filepath.Join(zedDir, "threads", "threads.db"), []zedThreadFixture{{
		id:        "exists",
		summary:   "Existing thread",
		updatedAt: "2026-06-09T02:30:00Z",
		dataType:  "json",
		data:      []byte(`{"messages":[]}`),
	}})

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentZed: {zedDir},
		},
		Machine: "local",
	})

	assert.Empty(t, engine.FindSourceFile("zed:missing"))
	err := engine.SyncSingleSession("zed:missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

type zedThreadFixture struct {
	id        string
	summary   string
	updatedAt string
	dataType  string
	data      []byte
}

const zedThreadsTestSchema = `CREATE TABLE threads (
	id TEXT PRIMARY KEY,
	summary TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	data_type TEXT NOT NULL,
	data BLOB NOT NULL,
	parent_id TEXT,
	folder_paths TEXT,
	folder_paths_order TEXT,
	created_at TEXT
)`

func createZedThreadsDB(
	t *testing.T,
	dbPath string,
	threads []zedThreadFixture,
) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	copySQLiteSchemaTemplate(
		t, dbPath, "zed threads", &zedSchemaOnce,
		&zedSchemaBytes, &zedSchemaErr,
		zedThreadsTestSchema,
	)

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()

	for _, thread := range threads {
		_, err = db.Exec(`INSERT INTO threads (
			id, summary, updated_at, data_type, data,
			parent_id, folder_paths, created_at
		) VALUES (?, ?, ?, ?, ?, NULL, '', '')`,
			thread.id,
			thread.summary,
			thread.updatedAt,
			thread.dataType,
			thread.data,
		)
		require.NoError(t, err)
	}
}
