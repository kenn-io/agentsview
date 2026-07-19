package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func writeTraeSyncDB(t *testing.T, path, reply string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.Ping())
	_, err = db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)
	value, err := json.Marshal(map[string]any{"list": []any{traeSyncSession("rewrite", reply)}})
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO ItemTable(key, value) VALUES (?, ?)`, "memento/icube-ai-agent-storage", value)
	require.NoError(t, err)
}

func rewriteTraeSyncDB(t *testing.T, path, reply string, mtime time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	value, err := json.Marshal(map[string]any{"list": []any{traeSyncSession("rewrite", reply)}})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE ItemTable SET value = ? WHERE key = ?`, value, "memento/icube-ai-agent-storage")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

func setTraeSyncDBSessions(t *testing.T, path string, sessions []any, mtime time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	value, err := json.Marshal(map[string]any{"list": sessions})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE ItemTable SET value = ? WHERE key = ?`, value, "memento/icube-ai-agent-storage")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

func seedTraeSyncWALDB(t *testing.T, path string, sessions []any) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.Ping())
	var journalMode string
	require.NoError(t, db.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode))
	require.Equal(t, "wal", journalMode)
	_, err = db.Exec("PRAGMA wal_autocheckpoint=0")
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)
	value, err := json.Marshal(map[string]any{"list": sessions})
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO ItemTable(key, value) VALUES (?, ?)`,
		"memento/icube-ai-agent-storage", value,
	)
	require.NoError(t, err)
}

func traeSyncSession(id, reply string) map[string]any {
	return map[string]any{
		"sessionId": id, "createdAt": 1715340600000, "updatedAt": 1715340900000,
		"messages": []any{
			map[string]any{"role": "user", "content": "same prompt"},
			map[string]any{"role": "assistant", "content": reply},
		},
	}
}

func writeTraeWorkspaceManifest(t *testing.T, dbPath, project string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(dbPath), "workspace.json")
	require.NoError(
		t,
		os.WriteFile(
			path,
			[]byte(`{"folder":"file:///Users/alice/code/`+project+`"}`),
			0o644,
		),
	)
	return path
}

func TestProcessFileProviderTraeSameSizeSameMtimeRewriteReparses(t *testing.T) {
	for _, seedCache := range []bool{true, false} {
		t.Run(map[bool]string{true: "skip cache", false: "fresh engine"}[seedCache], func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "globalStorage", "state.vscdb")
			writeTraeSyncDB(t, path, "initial reply")
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}}, Machine: "devbox"})
			first := engine.processFile(context.Background(), parser.DiscoveredFile{Path: path, Agent: parser.AgentTrae})
			require.NoError(t, first.err)
			require.Len(t, first.results, 1)
			initialHash := first.results[0].Session.File.Hash
			written, _, failed, _ := engine.writeBatch([]pendingWrite{{sess: first.results[0].Session, msgs: first.results[0].Messages, forceReplace: first.forceReplace}}, syncWriteDefault, false)
			require.Equal(t, 1, written)
			require.Equal(t, 0, failed)
			info, err := os.Stat(path)
			require.NoError(t, err)
			rewriteTraeSyncDB(t, path, "changed reply", info.ModTime())
			if seedCache {
				engine.cacheSkip(first.cacheKey, first.results[0].Session.File.Mtime)
			} else {
				engine = NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}}, Machine: "devbox"})
			}
			second := engine.processFile(context.Background(), parser.DiscoveredFile{Path: path, Agent: parser.AgentTrae})
			require.NoError(t, second.err)
			assert.False(t, second.skip)
			require.Len(t, second.results, 1)
			assert.Equal(t, "changed reply", second.results[0].Messages[1].Content)
			assert.NotEqual(t, initialHash, second.results[0].Session.File.Hash)
		})
	}
}

func TestProcessFileProviderTraeUnchangedSecondSyncDropsStoredVirtualResults(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "globalStorage", "state.vscdb")
	writeTraeSyncDB(t, path, "steady reply")
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})

	first := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  path,
		Agent: parser.AgentTrae,
	})
	require.NoError(t, first.err)
	require.Len(t, first.results, 1)

	written, _, failed, _ := engine.writeBatch([]pendingWrite{{
		sess:         first.results[0].Session,
		msgs:         first.results[0].Messages,
		forceReplace: first.forceReplace,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	second := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  path,
		Agent: parser.AgentTrae,
	})
	require.NoError(t, second.err)
	assert.False(t, second.skip)
	assert.Empty(t, second.results)
}

func TestProcessFileProviderTraeChangedContainerDropsUnchangedSibling(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "globalStorage", "state.vscdb")
	writeTraeSyncDB(t, path, "alpha reply")
	info, err := os.Stat(path)
	require.NoError(t, err)
	setTraeSyncDBSessions(t, path, []any{
		traeSyncSession("rewrite", "alpha reply"),
		traeSyncSession("steady", "steady reply"),
	}, info.ModTime())

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	first := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  path,
		Agent: parser.AgentTrae,
	})
	require.NoError(t, first.err)
	require.Len(t, first.results, 2)

	writes := make([]pendingWrite, 0, len(first.results))
	for _, result := range first.results {
		writes = append(writes, pendingWrite{
			sess:         result.Session,
			msgs:         result.Messages,
			forceReplace: first.forceReplace,
		})
	}
	written, _, failed, _ := engine.writeBatch(writes, syncWriteDefault, false)
	require.Equal(t, 2, written)
	require.Equal(t, 0, failed)

	info, err = os.Stat(path)
	require.NoError(t, err)
	setTraeSyncDBSessions(t, path, []any{
		traeSyncSession("rewrite", "bravo reply"),
		traeSyncSession("steady", "steady reply"),
	}, info.ModTime())

	second := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  path,
		Agent: parser.AgentTrae,
	})
	require.NoError(t, second.err)
	require.Len(t, second.results, 1)
	assert.Equal(t, "trae:rewrite", second.results[0].Session.ID)
	assert.Equal(t, "bravo reply", second.results[0].Messages[1].Content)
}

func TestProcessFileProviderTraeWALWatcherEventDropsUnchangedSibling(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "globalStorage", "state.vscdb")
	seedTraeSyncWALDB(t, path, []any{
		traeSyncSession("rewrite", "alpha reply"),
		traeSyncSession("steady", "steady reply"),
	})

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	first := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  path,
		Agent: parser.AgentTrae,
	})
	require.NoError(t, first.err)
	require.Len(t, first.results, 2)

	writes := make([]pendingWrite, 0, len(first.results))
	for _, result := range first.results {
		writes = append(writes, pendingWrite{
			sess:         result.Session,
			msgs:         result.Messages,
			forceReplace: first.forceReplace,
		})
	}
	written, _, failed, _ := engine.writeBatch(writes, syncWriteDefault, false)
	require.Equal(t, 2, written)
	require.Equal(t, 0, failed)

	info, err := os.Stat(path)
	require.NoError(t, err)
	setTraeSyncDBSessions(t, path, []any{
		traeSyncSession("rewrite", "bravo reply"),
		traeSyncSession("steady", "steady reply"),
	}, info.ModTime())

	classified := engine.classifyPaths([]string{path + "-wal"})
	require.Len(t, classified, 1)
	assert.Equal(t, path, classified[0].Path)
	assert.Equal(t, parser.AgentTrae, classified[0].Agent)
	assert.False(t, classified[0].ForceParse)

	second := engine.processFile(context.Background(), classified[0])
	require.NoError(t, second.err)
	require.Len(t, second.results, 1)
	assert.Equal(t, "trae:rewrite", second.results[0].Session.ID)
	assert.Equal(t, "bravo reply", second.results[0].Messages[1].Content)
}

func TestSyncEngineTraeUnchangedSecondSyncSkipsContainer(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "globalStorage", "state.vscdb")
	writeTraeSyncDB(t, path, "steady reply")

	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	first := engine.SyncAll(context.Background(), nil)
	require.False(t, first.Aborted)
	assert.Equal(t, 1, first.Synced)

	second := engine.SyncAll(context.Background(), nil)
	require.False(t, second.Aborted)
	assert.Equal(t, 0, second.Synced)
	assert.Equal(t, 1, second.Skipped)
}

func TestProcessFileProviderTraeRemovedWALSidecarDoesNotForceParse(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "globalStorage", "state.vscdb")
	writeTraeSyncDB(t, path, "steady reply")

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	first := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  path,
		Agent: parser.AgentTrae,
	})
	require.NoError(t, first.err)
	require.Len(t, first.results, 1)

	written, _, failed, _ := engine.writeBatch([]pendingWrite{{
		sess:         first.results[0].Session,
		msgs:         first.results[0].Messages,
		forceReplace: first.forceReplace,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	classified := engine.classifyPaths([]string{path + "-wal"})
	require.Len(t, classified, 1)
	assert.Equal(t, path, classified[0].Path)
	assert.False(t, classified[0].ForceParse)

	engine.SyncPathsContext(context.Background(), []string{path + "-wal"})
	stats := engine.LastSyncStats()
	assert.Equal(t, 0, stats.Synced)
	assert.Equal(t, 0, stats.Failed)
}

func TestSyncEngineTraeWorkspaceManifestChangesBypassWarmGate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workspaceStorage", "hash", "state.vscdb")
	writeTraeSyncDB(t, path, "steady reply")
	manifestPath := writeTraeWorkspaceManifest(t, path, "alpha")

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	first := engine.SyncAll(context.Background(), nil)
	require.False(t, first.Aborted)
	assert.Equal(t, 1, first.Synced)

	session, err := database.GetSession(context.Background(), "trae:rewrite")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "alpha", session.Project)

	require.NoError(
		t,
		os.WriteFile(
			manifestPath,
			[]byte(`{"folder":"file:///Users/alice/code/beta"}`),
			0o644,
		),
	)
	classified := engine.classifyPaths([]string{manifestPath})
	require.Len(t, classified, 1)
	assert.Equal(t, path, classified[0].Path)
	assert.False(t, classified[0].ForceParse)

	engine.SyncPathsContext(context.Background(), []string{manifestPath})
	stats := engine.LastSyncStats()
	assert.Equal(t, 1, stats.Synced)
	assert.Equal(t, 0, stats.Failed)

	session, err = database.GetSession(context.Background(), "trae:rewrite")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "beta", session.Project)

	require.NoError(t, os.Remove(manifestPath))
	classified = engine.classifyPaths([]string{manifestPath})
	require.Len(t, classified, 1)
	assert.Equal(t, path, classified[0].Path)
	assert.False(t, classified[0].ForceParse)

	engine.SyncPathsContext(context.Background(), []string{manifestPath})
	stats = engine.LastSyncStats()
	assert.Equal(t, 1, stats.Synced)
	assert.Equal(t, 0, stats.Failed)

	session, err = database.GetSession(context.Background(), "trae:rewrite")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "unknown", session.Project)
}

func TestSyncEngineTraeWorkspaceManifestChangeAcrossSyncAllReparses(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workspaceStorage", "hash", "state.vscdb")
	writeTraeSyncDB(t, path, "steady reply")
	manifestPath := writeTraeWorkspaceManifest(t, path, "alpha")

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	first := engine.SyncAll(context.Background(), nil)
	require.False(t, first.Aborted)
	assert.Equal(t, 1, first.Synced)

	require.NoError(
		t,
		os.WriteFile(
			manifestPath,
			[]byte(`{"folder":"file:///Users/alice/code/beta"}`),
			0o644,
		),
	)

	second := engine.SyncAll(context.Background(), nil)
	require.False(t, second.Aborted)
	assert.Equal(t, 1, second.Synced)
	assert.Equal(t, 0, second.Failed)

	session, err := database.GetSession(context.Background(), "trae:rewrite")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "beta", session.Project)
}

func TestSyncPathsTraeChangedContainerScopesGateCaptureAndLoad(t *testing.T) {
	root := t.TempDir()
	changedDB := filepath.Join(
		root, "workspaceStorage", "hash-a", "state.vscdb",
	)
	otherDB := filepath.Join(
		root, "workspaceStorage", "hash-b", "state.vscdb",
	)
	writeTraeSyncDB(t, changedDB, "reply a")
	writeTraeSyncDB(t, otherDB, "reply b")
	changedManifest := writeTraeWorkspaceManifest(t, changedDB, "alpha")
	writeTraeWorkspaceManifest(t, otherDB, "bravo")

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	first := engine.SyncAll(context.Background(), nil)
	require.False(t, first.Aborted)
	assert.Equal(t, 2, first.Synced)

	require.NoError(
		t,
		os.WriteFile(
			changedManifest,
			[]byte(`{"folder":"file:///Users/alice/code/beta"}`),
			0o644,
		),
	)

	origStat := statSQLiteContainerState
	t.Cleanup(func() { statSQLiteContainerState = origStat })
	var statPaths []string
	statSQLiteContainerState = func(dbPath string) (parser.SQLiteContainerState, bool) {
		statPaths = append(statPaths, filepath.Clean(dbPath))
		return parser.StatSQLiteContainerState(dbPath)
	}

	var openPaths []string
	restoreOpenHook := parser.SetWindsurfDBOpenHookForTest(func(path string) {
		openPaths = append(openPaths, filepath.Clean(path))
	})
	t.Cleanup(restoreOpenHook)

	engine.SyncPathsContext(context.Background(), []string{changedManifest})
	stats := engine.LastSyncStats()
	assert.Equal(t, 1, stats.Synced)
	assert.Equal(t, 0, stats.Failed)

	assert.Equal(
		t,
		[]string{filepath.Clean(changedDB)},
		uniqueContainerPaths(statPaths),
	)
	assert.Equal(
		t,
		[]string{filepath.Clean(changedDB)},
		uniqueContainerPaths(openPaths),
	)
}
