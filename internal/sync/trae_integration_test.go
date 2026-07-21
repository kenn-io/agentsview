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
	"go.kenn.io/agentsview/internal/db"
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
	assert.Equal(t, "trae:globalStorage:rewrite", second.results[0].Session.ID)
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
	assert.Equal(t, "trae:globalStorage:rewrite", second.results[0].Session.ID)
	assert.Equal(t, "bravo reply", second.results[0].Messages[1].Content)
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

func TestProcessFileProviderTraeCrossNamespaceIDsSurviveUpsert(t *testing.T) {
	root := t.TempDir()
	workspacePath := filepath.Join(root, "workspaceStorage", "hash", "state.vscdb")
	globalPath := filepath.Join(root, "globalStorage", "state.vscdb")
	writeTraeSyncDB(t, workspacePath, "workspace reply")
	writeTraeSyncDB(t, globalPath, "global reply")

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	for _, path := range []string{workspacePath, globalPath} {
		processed := engine.processFile(context.Background(), parser.DiscoveredFile{Path: path, Agent: parser.AgentTrae})
		require.NoError(t, processed.err)
		require.Len(t, processed.results, 1)
		written, _, failed, _ := engine.writeBatch([]pendingWrite{{
			sess:         processed.results[0].Session,
			msgs:         processed.results[0].Messages,
			forceReplace: processed.forceReplace,
		}}, syncWriteDefault, false)
		require.Equal(t, 1, written)
		require.Equal(t, 0, failed)
	}

	workspace, err := database.GetSessionFull(context.Background(), "trae:workspaceStorage:rewrite")
	require.NoError(t, err)
	global, err := database.GetSessionFull(context.Background(), "trae:globalStorage:rewrite")
	require.NoError(t, err)
	require.NotNil(t, workspace)
	require.NotNil(t, global)
	assert.Equal(t, "rewrite", workspace.SourceSessionID)
	assert.Equal(t, "rewrite", global.SourceSessionID)
	require.NotNil(t, workspace.FilePath)
	require.NotNil(t, global.FilePath)
	assert.Equal(t, workspacePath+"#rewrite", *workspace.FilePath)
	assert.Equal(t, globalPath+"#rewrite", *global.FilePath)

	workspaceMessages, err := database.GetMessages(context.Background(), workspace.ID, 0, 10, true)
	require.NoError(t, err)
	globalMessages, err := database.GetMessages(context.Background(), global.ID, 0, 10, true)
	require.NoError(t, err)
	assert.Equal(t, "workspace reply", workspaceMessages[1].Content)
	assert.Equal(t, "global reply", globalMessages[1].Content)
}

func TestProcessFileProviderTraeNamespaceLifecycleIsolation(t *testing.T) {
	root := t.TempDir()
	workspacePath := filepath.Join(root, "workspaceStorage", "hash", "state.vscdb")
	globalPath := filepath.Join(root, "globalStorage", "state.vscdb")
	writeTraeSyncDB(t, workspacePath, "workspace reply")
	writeTraeSyncDB(t, globalPath, "global reply")

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	for _, path := range []string{workspacePath, globalPath} {
		processed := engine.processFile(context.Background(), parser.DiscoveredFile{Path: path, Agent: parser.AgentTrae})
		require.NoError(t, processed.err)
		require.Len(t, processed.results, 1)
		written, _, failed, _ := engine.writeBatch([]pendingWrite{{
			sess:         processed.results[0].Session,
			msgs:         processed.results[0].Messages,
			forceReplace: processed.forceReplace,
		}}, syncWriteDefault, false)
		require.Equal(t, 1, written)
		require.Equal(t, 0, failed)
	}

	info, err := os.Stat(workspacePath)
	require.NoError(t, err)
	rewriteTraeSyncDB(t, workspacePath, "workspace rewritten", info.ModTime())
	processed := engine.processFile(context.Background(), parser.DiscoveredFile{Path: workspacePath, Agent: parser.AgentTrae})
	require.NoError(t, processed.err)
	require.Len(t, processed.results, 1)
	written, _, failed, _ := engine.writeBatch([]pendingWrite{{
		sess:         processed.results[0].Session,
		msgs:         processed.results[0].Messages,
		forceReplace: processed.forceReplace,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	workspaceMessages, err := database.GetMessages(context.Background(), "trae:workspaceStorage:rewrite", 0, 10, true)
	require.NoError(t, err)
	globalMessages, err := database.GetMessages(context.Background(), "trae:globalStorage:rewrite", 0, 10, true)
	require.NoError(t, err)
	assert.Equal(t, "workspace rewritten", workspaceMessages[1].Content)
	assert.Equal(t, "global reply", globalMessages[1].Content)
}

func TestResyncAllMigratesLegacyTraeNamespaceIDs(t *testing.T) {
	root := t.TempDir()
	workspacePath := filepath.Join(root, "workspaceStorage", "hash", "state.vscdb")
	globalPath := filepath.Join(root, "globalStorage", "state.vscdb")
	writeTraeSyncDB(t, workspacePath, "workspace reply")
	writeTraeSyncDB(t, globalPath, "global reply")

	archivePath := filepath.Join(root, "agentsview.db")
	dbtest.EnsureTestDBAt(t, archivePath)
	archive, err := db.Open(archivePath)
	require.NoError(t, err)

	startedAt := "2026-05-10T10:00:00Z"
	legacyPath := workspacePath + "#rewrite"
	require.NoError(t, archive.UpsertSession(db.Session{
		ID:               "trae:rewrite",
		Project:          "agentsview",
		Machine:          "devbox",
		Agent:            string(parser.AgentTrae),
		SourceSessionID:  "rewrite",
		StartedAt:        &startedAt,
		EndedAt:          &startedAt,
		MessageCount:     2,
		UserMessageCount: 1,
		FilePath:         &legacyPath,
	}))
	dbtest.SeedMessages(t, archive,
		dbtest.UserMsg("trae:rewrite", 0, "same prompt"),
		db.Message{
			SessionID:     "trae:rewrite",
			Ordinal:       1,
			Role:          "assistant",
			Content:       "legacy reply",
			ContentLength: len("legacy reply"),
		},
	)
	require.NoError(t, archive.SetSessionDataVersion("trae:rewrite", 68))
	require.NoError(t, archive.Close())

	conn, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	_, err = conn.Exec(`PRAGMA user_version = 68`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	reopened, err := db.Open(archivePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.True(t, reopened.NeedsResync())

	engine := NewEngine(reopened, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	stats := engine.SyncAll(context.Background(), nil)
	require.False(t, stats.Aborted)
	assert.Equal(t, 0, stats.OrphanedCopied)

	legacy, err := reopened.GetSessionFull(context.Background(), "trae:rewrite")
	require.NoError(t, err)
	assert.Nil(t, legacy)

	workspace, err := reopened.GetSessionFull(context.Background(), "trae:workspaceStorage:rewrite")
	require.NoError(t, err)
	require.NotNil(t, workspace)
	global, err := reopened.GetSessionFull(context.Background(), "trae:globalStorage:rewrite")
	require.NoError(t, err)
	require.NotNil(t, global)

	workspaceMessages, err := reopened.GetMessages(context.Background(), workspace.ID, 0, 10, true)
	require.NoError(t, err)
	globalMessages, err := reopened.GetMessages(context.Background(), global.ID, 0, 10, true)
	require.NoError(t, err)
	assert.Equal(t, "workspace reply", workspaceMessages[1].Content)
	assert.Equal(t, "global reply", globalMessages[1].Content)
}

func TestFindSourceFileTraeRelocationUsesQualifiedRawID(t *testing.T) {
	root := t.TempDir()
	oldDB := filepath.Join(root, "workspaceStorage", "old-hash", "state.vscdb")
	newDB := filepath.Join(root, "workspaceStorage", "hash", "state.vscdb")
	writeTraeSyncDB(t, oldDB, "old reply")
	writeTraeSyncDB(t, newDB, "new reply")

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	processed := engine.processFile(context.Background(), parser.DiscoveredFile{Path: oldDB, Agent: parser.AgentTrae})
	require.NoError(t, processed.err)
	require.Len(t, processed.results, 1)
	written, _, failed, _ := engine.writeBatch([]pendingWrite{{
		sess:         processed.results[0].Session,
		msgs:         processed.results[0].Messages,
		forceReplace: processed.forceReplace,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Equal(t, 0, failed)

	info, err := os.Stat(oldDB)
	require.NoError(t, err)
	setTraeSyncDBSessions(t, oldDB, []any{traeSyncSession("other", "old moved")}, info.ModTime())
	assert.Equal(t, newDB+"#rewrite", engine.FindSourceFile("trae:workspaceStorage:rewrite"))
}
