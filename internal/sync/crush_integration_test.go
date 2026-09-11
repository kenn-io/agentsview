package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

const crushSyncTestSchema = `
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		parent_session_id TEXT,
		title TEXT NOT NULL,
		message_count INTEGER NOT NULL DEFAULT 0,
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		cost REAL NOT NULL DEFAULT 0.0,
		updated_at INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		summary_message_id TEXT,
		todos TEXT
	);
	CREATE TABLE messages (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		role TEXT NOT NULL,
		parts TEXT NOT NULL DEFAULT '[]',
		model TEXT,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		finished_at INTEGER,
		provider TEXT,
		is_summary_message INTEGER DEFAULT 0 NOT NULL
	);
	CREATE INDEX idx_messages_session_id ON messages (session_id);
`

func TestSyncCrushSkipsUnchangedSessionsAndReparsesChangedOnes(t *testing.T) {
	dataDir, dbPath, sourceDB := writeSyncCrushDB(t)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCrush: {dataDir},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)

	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})

	session, err := database.GetSession(context.Background(), "crush:sess-001")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 2, session.MessageCount)
	assert.Equal(t, dbPath+"#sess-001", database.GetSessionFilePath("crush:sess-001"))

	messages, err := database.GetMessages(context.Background(), "crush:sess-001", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "Inspect the auth flow.", messages[0].Content)

	// A second full sync of unchanged data must skip the session via the
	// provider fingerprint rather than reparse it.
	runSyncAndAssert(t, engine, SyncStats{})

	writtenSessions := 0
	engine.writeBatchOverride = func(
		batch []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		writtenSessions += len(batch)
		return len(batch), 0, 0, 0
	}

	stats := SyncStats{}
	aborted := engine.syncProviderDBBackedAgent(
		context.Background(), parser.AgentCrush, "crush",
		syncWriteBulk, false,
		newRootSyncScope([]string{dataDir}), &stats, func(int, int) {},
	)
	assert.False(t, aborted)
	assert.Zero(t, writtenSessions,
		"unchanged Crush sessions must not be reparsed on a warm full sync")

	// A same-second message insert must change the fingerprint and reparse.
	insertSyncCrushMessage(t, sourceDB, "msg-review", "assistant", `[
		{"type":"text","data":{"text":"Review complete."}}
	]`, 1_789_093_630, "glm-5.3-flash")
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})
	messages, err = database.GetMessages(context.Background(), "crush:sess-001", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	assert.Equal(t, "Review complete.", messages[2].Content)
}

func TestSyncCrushReparsesWhenRegistryProjectPathChanges(t *testing.T) {
	dataDir, _, _ := writeSyncCrushDB(t)
	oldProjectDir := filepath.Join(t.TempDir(), "old-project")
	newProjectDir := filepath.Join(t.TempDir(), "new-project")
	require.NoError(t, os.MkdirAll(oldProjectDir, 0o755))
	require.NoError(t, os.MkdirAll(newProjectDir, 0o755))

	registryDir := t.TempDir()
	registryPath := filepath.Join(registryDir, parser.CrushProjectsFileName)
	registry := `{"projects":[{"path":"` + filepath.ToSlash(oldProjectDir) +
		`","data_dir":"` + filepath.ToSlash(dataDir) + `"}]}`
	require.NoError(t, os.WriteFile(registryPath, []byte(registry), 0o600))

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCrush: {registryDir},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})

	session, err := database.GetSession(context.Background(), "crush:sess-001")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, oldProjectDir, session.Cwd)
	assert.Equal(t, "old_project", session.Project)

	registry = `{"projects":[{"path":"` + filepath.ToSlash(newProjectDir) +
		`","data_dir":"` + filepath.ToSlash(dataDir) + `"}]}`
	require.NoError(t, os.WriteFile(registryPath, []byte(registry), 0o600))
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})

	session, err = database.GetSession(context.Background(), "crush:sess-001")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, newProjectDir, session.Cwd)
	assert.Equal(t, "new_project", session.Project)
}

func TestReconcileProviderRootsCrushDBFileRootTombstonesDeletedSession(t *testing.T) {
	_, dbPath, sourceDB := writeSyncCrushDB(t)
	database := openTestDB(t)
	// Configure the database-file root that normalizeCrushRoots accepts and
	// ResolveReconciliationScopes must map onto the data directory.
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCrush: {dbPath},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})

	_, err := sourceDB.Exec(`
		DELETE FROM messages WHERE session_id = 'sess-001';
		DELETE FROM sessions WHERE id = 'sess-001';
	`)
	require.NoError(t, err)
	require.NoError(t, sourceDB.Close())

	// Reconcile with the original crush.db request root: the provider must
	// expand it to the data directory so virtual members are in scope.
	require.NoError(t, engine.ReconcileProviderRoots(
		context.Background(), parser.AgentCrush, []string{dbPath},
	))

	active, err := database.GetSession(context.Background(), "crush:sess-001")
	require.NoError(t, err)
	assert.NotNil(t, active)
	archived, err := database.GetSessionFull(context.Background(), "crush:sess-001")
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
}

func TestReconcileProviderRootsCrushDataDirSkipsUnchangedSession(t *testing.T) {
	dataDir, _, _ := writeSyncCrushDB(t)
	engine := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCrush: {dataDir},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})

	writtenSessions := 0
	engine.writeBatchOverride = func(
		batch []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		writtenSessions += len(batch)
		return len(batch), 0, 0, 0
	}

	require.NoError(t, engine.ReconcileProviderRoots(
		context.Background(), parser.AgentCrush, []string{dataDir},
	))
	assert.Zero(t, writtenSessions,
		"unchanged Crush sessions must not be rewritten during reconciliation")
}

func writeSyncCrushDB(t *testing.T) (string, string, *sql.DB) {
	t.Helper()
	projectDir := t.TempDir()
	dataDir := filepath.Join(projectDir, ".crush")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	dbPath := filepath.Join(dataDir, parser.CrushDBName)
	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.Exec(crushSyncTestSchema)
	require.NoError(t, err)
	_, err = database.Exec(`
		INSERT INTO sessions (
			id, title, created_at, updated_at,
			prompt_tokens, completion_tokens, cost
		) VALUES (
			'sess-001', 'Auth review', 1789093626, 1789093746, 100, 20, 0.0125
		)
	`)
	require.NoError(t, err)
	insertSyncCrushMessage(t, database, "msg-user", "user", `[
		{"type":"text","data":{"text":"Inspect the auth flow."}}
	]`, 1_789_093_626, "")
	insertSyncCrushMessage(t, database, "msg-assistant", "assistant", `[
		{"type":"text","data":{"text":"I will inspect the file."}}
	]`, 1_789_093_629, "glm-5.3-flash")
	return dataDir, dbPath, database
}

func insertSyncCrushMessage(
	t *testing.T, database *sql.DB, id, role, parts string, created int64, model string,
) {
	t.Helper()
	_, err := database.Exec(`
		INSERT INTO messages (
			id, session_id, role, parts, model, created_at, updated_at
		) VALUES (?, 'sess-001', ?, ?, ?, ?, ?)
	`, id, role, parts, model, created, created)
	require.NoError(t, err)
	_, err = database.Exec(`
		UPDATE sessions SET message_count = (
			SELECT COUNT(*) FROM messages WHERE session_id = 'sess-001'
		) WHERE id = 'sess-001'
	`)
	require.NoError(t, err)
}
