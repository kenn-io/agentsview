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

func writeTraeSyncDBWithoutStorageKey(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.Ping())
	_, err = db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO ItemTable(key, value) VALUES (?, ?)`, "memento/unrelated-chat-storage", `{"list":[{"sessionId":"ignored","messages":[{"role":"user","content":"wrong"}]}]}`)
	require.NoError(t, err)
}

func writeTraeSyncModularData(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(root), "ModularData", "ai-agent", "database.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("encrypted header"), 0o644))
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

func TestTraeEncryptedLayoutReportsUnsupportedSource(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "empty stub",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				db, err := sql.Open("sqlite3", path)
				require.NoError(t, err)
				defer db.Close()
				_, err = db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`)
				require.NoError(t, err)
				value, err := json.Marshal(map[string]any{"list": []any{map[string]any{
					"sessionId": "stub",
					"messages":  []any{},
				}}})
				require.NoError(t, err)
				_, err = db.Exec(`INSERT INTO ItemTable(key, value) VALUES (?, ?)`, "memento/icube-ai-agent-storage", value)
				require.NoError(t, err)
			},
		},
		{
			name: "missing storage key",
			setup: func(t *testing.T, path string) {
				writeTraeSyncDBWithoutStorageKey(t, path)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "Trae", "User")
			path := filepath.Join(root, "globalStorage", "state.vscdb")
			test.setup(t, path)
			writeTraeSyncModularData(t, root)

			database := dbtest.OpenTestDB(t)
			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
				Machine:   "devbox",
			})
			res := engine.processFile(context.Background(), parser.DiscoveredFile{
				Path:  path,
				Agent: parser.AgentTrae,
			})
			require.NoError(t, res.err)
			assert.True(t, res.skip)
			assert.False(t, res.forceReplace)
			assert.True(t, res.suppressPresenceSweep)
			assert.Zero(t, res.providerFailureCount)
			assert.Zero(t, res.providerWideFailureCount)
			assert.Empty(t, res.results)

			var stats SyncStats
			engine.anomalies.applyTo(&stats)
			assert.Equal(t, 1, stats.Anomalies.UnsupportedSourceLayoutsTotal)
			assert.Equal(t, 1, stats.Anomalies.UnsupportedSourceLayoutsByAgent[string(parser.AgentTrae)])
		})
	}
}

func TestReconcileTraeUnsupportedSiblingPreservesArchiveAuthority(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Trae", "User")
	globalPath := filepath.Join(root, "globalStorage", "state.vscdb")
	unsupportedPath := filepath.Join(root, "workspaceStorage", "unsupported", "state.vscdb")
	writeTraeSyncDB(t, globalPath, "supported reply")
	writeTraeSyncDB(t, unsupportedPath, "unsupported reply")
	workspaceInfo, err := os.Stat(unsupportedPath)
	require.NoError(t, err)
	setTraeSyncDBSessions(t, unsupportedPath, []any{
		traeSyncSession("unsupported", "unsupported reply"),
	}, workspaceInfo.ModTime())
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	t.Cleanup(engine.Close)

	initial := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 2, initial.Synced, "both siblings must be discovered and synced")
	assert.Zero(t, initial.Failed)
	supported, err := database.GetSession(t.Context(), "trae:rewrite")
	require.NoError(t, err)
	assert.NotNil(t, supported, "supported sibling must be seeded")
	unsupported, err := database.GetSession(t.Context(), "trae:unsupported")
	require.NoError(t, err)
	assert.NotNil(t, unsupported, "unsupported sibling must be seeded")

	require.NoError(t, os.Remove(unsupportedPath))
	writeTraeSyncDBWithoutStorageKey(t, unsupportedPath)
	writeTraeSyncModularData(t, root)

	stats, _, err := engine.ReconcileWatchRootsWithStats(
		t.Context(), nil, true, nil,
	)
	require.NoError(t, err)
	assert.Zero(t, stats.Failed)
	assert.Zero(t, engine.LastReconciliationResult().ProviderFailures)
	assert.Equal(t, 1, stats.Anomalies.UnsupportedSourceLayoutsTotal)
	unsupported, err = database.GetSession(t.Context(), "trae:unsupported")
	require.NoError(t, err)
	assert.NotNil(t, unsupported, "unsupported container member must remain active")

	globalInfo, err := os.Stat(globalPath)
	require.NoError(t, err)
	setTraeSyncDBSessions(t, globalPath, []any{}, globalInfo.ModTime().Add(time.Second))

	stats, _, err = engine.ReconcileWatchRootsWithStats(
		t.Context(), nil, true, nil,
	)
	require.NoError(t, err)
	assert.Zero(t, stats.Failed)
	assert.Zero(t, engine.LastReconciliationResult().ProviderFailures)

	supported, err = database.GetSession(t.Context(), "trae:rewrite")
	require.NoError(t, err)
	assert.Nil(t, supported, "supported sibling removal must tombstone")
	unsupported, err = database.GetSession(t.Context(), "trae:unsupported")
	require.NoError(t, err)
	assert.NotNil(t, unsupported, "unsupported container member must remain active")
}

func TestReconcileTraeUnsupportedContainerRemovalPreservesArchive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Trae", "User")
	globalPath := filepath.Join(root, "globalStorage", "state.vscdb")
	unsupportedPath := filepath.Join(root, "workspaceStorage", "unsupported", "state.vscdb")
	writeTraeSyncDB(t, globalPath, "supported reply")
	writeTraeSyncDB(t, unsupportedPath, "unsupported reply")
	workspaceInfo, err := os.Stat(unsupportedPath)
	require.NoError(t, err)
	setTraeSyncDBSessions(t, unsupportedPath, []any{
		traeSyncSession("unsupported", "unsupported reply"),
	}, workspaceInfo.ModTime())
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	t.Cleanup(engine.Close)

	initial := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 2, initial.Synced)
	assert.Zero(t, initial.Failed)
	seeded, err := database.GetSession(t.Context(), "trae:unsupported")
	require.NoError(t, err)
	assert.NotNil(t, seeded)

	require.NoError(t, os.Remove(unsupportedPath))
	stats, _, err := engine.ReconcileWatchRootsWithStats(
		t.Context(), nil, true, nil,
	)
	require.NoError(t, err)
	assert.Zero(t, stats.Failed)
	assert.Zero(t, engine.LastReconciliationResult().ProviderFailures)

	preserved, err := database.GetSession(t.Context(), "trae:unsupported")
	require.NoError(t, err)
	assert.NotNil(t, preserved, "vanished unsupported container must preserve its archive member")
}

func TestWatchBatchTraeMissingContainerPreservesArchive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Trae", "User")
	path := filepath.Join(root, "globalStorage", "state.vscdb")
	writeTraeSyncDB(t, path, "archived reply")
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	t.Cleanup(engine.Close)

	initial := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, initial.Synced)
	require.Zero(t, initial.Failed)
	require.NoError(t, os.Remove(path))

	err := ApplyWatchBatch(t.Context(), engine, WatchBatch{
		Paths: []string{path, path + "-wal"},
	}, nil)
	require.NoError(t, err)
	assert.Zero(t, engine.LastSyncStats().Failed)

	active, err := database.GetSession(t.Context(), "trae:rewrite")
	require.NoError(t, err)
	assert.NotNil(t, active,
		"a vanished persistent container cannot prove member deletion")
	archived, err := database.GetSessionFull(t.Context(), "trae:rewrite")
	require.NoError(t, err)
	require.NotNil(t, archived)
	assert.Nil(t, archived.DeletionCause)
}

func TestReconcileTraeCompleteContainerTombstonesRemovedMember(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Trae", "User")
	path := filepath.Join(root, "globalStorage", "state.vscdb")
	writeTraeSyncDB(t, path, "first reply")
	info, err := os.Stat(path)
	require.NoError(t, err)
	setTraeSyncDBSessions(t, path, []any{
		traeSyncSession("removed", "first reply"),
		traeSyncSession("kept", "second reply"),
	}, info.ModTime())
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	t.Cleanup(engine.Close)

	initial := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 2, initial.Synced)
	assert.Zero(t, initial.Failed)

	info, err = os.Stat(path)
	require.NoError(t, err)
	setTraeSyncDBSessions(t, path, []any{
		traeSyncSession("kept", "second reply"),
	}, info.ModTime().Add(time.Second))
	stats, _, err := engine.ReconcileWatchRootsWithStats(
		t.Context(), nil, true, nil,
	)
	require.NoError(t, err)
	assert.Zero(t, stats.Failed)

	kept, err := database.GetSession(t.Context(), "trae:kept")
	require.NoError(t, err)
	assert.NotNil(t, kept, "remaining container member must stay active")
	removed, err := database.GetSession(t.Context(), "trae:removed")
	require.NoError(t, err)
	assert.Nil(t, removed, "removed container member must not stay active")
	archived, err := database.GetSessionFull(t.Context(), "trae:removed")
	require.NoError(t, err)
	require.NotNil(t, archived)
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)
}

func TestReconcileTraeEmptyContainerTombstonesLastMember(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Trae", "User")
	path := filepath.Join(root, "globalStorage", "state.vscdb")
	writeTraeSyncDB(t, path, "last reply")
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTrae: {root}},
		Machine:   "devbox",
	})
	t.Cleanup(engine.Close)

	initial := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 1, initial.Synced)
	assert.Zero(t, initial.Failed)

	info, err := os.Stat(path)
	require.NoError(t, err)
	setTraeSyncDBSessions(t, path, []any{}, info.ModTime().Add(time.Second))
	stats, _, err := engine.ReconcileWatchRootsWithStats(
		t.Context(), nil, true, nil,
	)
	require.NoError(t, err)
	assert.Zero(t, stats.Failed)

	active, err := database.GetSession(t.Context(), "trae:rewrite")
	require.NoError(t, err)
	assert.Nil(t, active, "last removed member must not stay active")
	archived, err := database.GetSessionFull(t.Context(), "trae:rewrite")
	require.NoError(t, err)
	require.NotNil(t, archived, "last removed member must remain archived")
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)
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

	classified, err := engine.classifyPaths(
		context.Background(), []string{path + "-wal"},
	)
	require.NoError(t, err)
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

	classified, err := engine.classifyPaths(
		context.Background(), []string{path + "-wal"},
	)
	require.NoError(t, err)
	require.Len(t, classified, 1)
	assert.Equal(t, path, classified[0].Path)
	assert.False(t, classified[0].ForceParse)

	engine.SyncPathsContext(context.Background(), []string{path + "-wal"})
	stats := engine.LastSyncStats()
	assert.Equal(t, 0, stats.Synced)
	assert.Equal(t, 0, stats.Failed)
}
