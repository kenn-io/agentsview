package sync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSharedTitleRegressionUsageOnlySweepIsNoop(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	root := t.TempDir()
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}, Machine: "devbox", Emitter: emitter, ArchiveContent: config.ArchiveContentUsage})
	t.Cleanup(engine.Close)
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "antigravity:"+id, parser.AgentAntigravity, "", root)
	title := "source title"
	path := writeConversationSummariesDB(t, root, map[string]*string{id: &title})
	for i := 0; i < 2; i++ {
		plan, err := engine.PlanChangedPathsContext(t.Context(), []string{path})
		require.NoError(t, err)
		result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
		require.NoError(t, err)
		assert.Zero(t, result.Stats.TitlesUpdated, "usage policy discards titles, sweep should not repeatedly report a change")
		assert.Empty(t, emitter.take(), "usage-only stable archive must not repeatedly notify")
	}
}

func TestSharedTitleRegressionRemoteQoderOwnership(t *testing.T) {
	engine, database := sharedTitleTestEngine(t, nil)
	engine.idPrefix = "remote~"
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSessionAt(t, database, "remote~qoder:"+id, parser.AgentQoder, "remote old", "remote:/home/u/.qoder/projects/p/"+id+".jsonl")
	title := "LOCAL MACHINE TITLE"
	path := writeQoderSessionsDB(t, t.TempDir(), "com.qoder.app.stable", map[string]*string{id: &title})
	engine.syncMu.Lock()
	n, err := engine.refreshSharedTitleDatabasesLocked(t.Context(), []parser.SharedTitleDatabase{{Agent: parser.AgentQoder, DBPath: path}})
	engine.syncMu.Unlock()
	require.NoError(t, err)
	assert.Zero(t, n, "local database cannot own a remote source simply by app directory name")
	requireStoredTitle(t, database, "remote~qoder:"+id, "remote old")
}

func TestSharedTitleRegressionJSONRecoverySweep(t *testing.T) {
	engine, database := sharedTitleTestEngine(t, nil)
	base := t.TempDir()
	root := filepath.Join(base, ".qoder")
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "qoder:"+id, parser.AgentQoder, "stale", root)
	title := "lower priority"
	path := writeQoderSessionsDB(t, base, "com.qoder.app.stable", map[string]*string{id: &title})
	writePath := filepath.Join(root, "projects", id+"-session.json")
	dbtest.WriteTestFile(t, writePath, []byte(`{broken`))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	requireStoredTitle(t, database, "qoder:"+id, "stale")
	dbtest.WriteTestFile(t, writePath, []byte(`{"title":"recovered JSON title"}`))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	requireStoredTitle(t, database, "qoder:"+id, "recovered JSON title")
}

func TestSharedTitleRegressionDisabledProviderNotRefreshed(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	root := t.TempDir()
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}, DisabledAgents: []parser.AgentType{parser.AgentAntigravity}, Machine: "devbox", Emitter: emitter})
	t.Cleanup(engine.Close)
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "antigravity:"+id, parser.AgentAntigravity, "disabled old", root)
	title := "must not sync disabled agent"
	writeConversationSummariesDB(t, root, map[string]*string{id: &title})
	require.NoError(t, engine.SyncSharedTitlesContext(t.Context()))
	requireStoredTitle(t, database, "antigravity:"+id, "disabled old")
	assert.Empty(t, emitter.take())
}

func TestSharedTitleRegressionDifferentMachineNotRefreshed(t *testing.T) {
	engine, database := sharedTitleTestEngine(t, nil)
	base := t.TempDir()
	root := filepath.Join(base, ".qoder")
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	old := "remote old"
	source := filepath.Join(root, "projects", id+".jsonl")
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "qoder:" + id, Agent: string(parser.AgentQoder), Machine: "another-machine",
		FilePath: &source, SessionName: &old,
	}))
	title := "local title"
	path := writeQoderSessionsDB(t, base, "com.qoder.app.stable", map[string]*string{id: &title})
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	requireStoredTitle(t, database, "qoder:"+id, old)
}

func TestSharedTitleRegressionJSONOverridesEqualSQLiteAndNull(t *testing.T) {
	for _, null := range []bool{false, true} {
		name := "equal SQLite"
		if null {
			name = "SQL NULL"
		}
		t.Run(name, func(t *testing.T) {
			emitter := &sharedTitleScopeRecorder{}
			engine, database := sharedTitleTestEngine(t, emitter)
			base := t.TempDir()
			root := filepath.Join(base, ".qoder")
			const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			stale := "stale"
			seedSharedTitleSession(t, database, "qoder:"+id, parser.AgentQoder, stale, root)
			fallback := &stale
			if null {
				fallback = nil
			}
			path := writeQoderSessionsDB(t, base, "com.qoder.app.stable", map[string]*string{id: fallback})
			dbtest.WriteTestFile(t, filepath.Join(root, "projects", id+"-session.json"), []byte(`{"title":"JSON wins"}`))
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
			requireStoredTitle(t, database, "qoder:"+id, "JSON wins")
			assert.Equal(t, []string{"sessions"}, emitter.take())
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
			assert.Empty(t, emitter.take())
		})
	}
}
