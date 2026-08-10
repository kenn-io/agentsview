package sync_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type traexRepairFixture struct {
	database *db.DB
	engine   *agentsync.Engine
	path     string
	uuid     string
}

func newTraeXRepairFixture(t *testing.T) traexRepairFixture {
	t.Helper()
	root := t.TempDir()
	sessionsRoot := filepath.Join(root, "sessions")
	const uuid = "019fbcca-9fd4-7d20-83dc-0762b2f839b3"
	path := filepath.Join(
		sessionsRoot, "2026", "08", "01",
		"rollout-2026-08-01T18-07-03-"+uuid+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/workspace/project", "codex-tui").
		AddCodexMessage(tsEarlyS1, "user", "first prompt").
		String()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentTraeX: {sessionsRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	return traexRepairFixture{
		database: database, engine: engine, path: path, uuid: uuid,
	}
}

func seedSharedPathProjectRepairConflict(
	t *testing.T,
	fx traexRepairFixture,
) {
	t.Helper()
	traexID := "traex:" + fx.uuid
	traex, err := fx.database.GetSessionFull(t.Context(), traexID)
	require.NoError(t, err)
	require.NotNil(t, traex)
	require.NotNil(t, traex.FileMtime)
	traex.Project = "roborev_ci_28293_3831737461"
	require.NoError(t, fx.database.UpsertSession(*traex))
	require.NoError(t, fx.database.SetSessionDataVersion(
		traexID, db.CurrentDataVersion(),
	))

	codex := *traex
	codex.ID = "codex:" + fx.uuid
	codex.Agent = string(parser.AgentCodex)
	codex.Project = "project"
	newerMtime := *traex.FileMtime + 1
	codex.FileMtime = &newerMtime
	require.NoError(t, fx.database.UpsertSession(codex))
	require.NoError(t, fx.database.SetSessionDataVersion(
		codex.ID, db.CurrentDataVersion(),
	))

	project, ok := fx.database.GetProjectByPath(fx.path)
	require.True(t, ok)
	require.Equal(t, "project", project,
		"path-only lookup precondition must select the newer Codex row")
	project, ok = fx.database.GetProjectByAgentPath(
		fx.path, string(parser.AgentTraeX),
	)
	require.True(t, ok)
	require.Equal(t, "roborev_ci_28293_3831737461", project)
}

func TestTraeXCachedSkipDoesNotBorrowCodexRepairState(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	fx := newTraeXRepairFixture(t)
	initial, err := fx.database.GetSessionFull(
		t.Context(), "traex:"+fx.uuid,
	)
	require.NoError(t, err)
	require.NotNil(t, initial)
	require.NotNil(t, initial.FileMtime)
	require.NotNil(t, initial.FileHash)
	cacheKey := fx.path + "?agent=traex?source_hash=" + *initial.FileHash
	fx.engine.InjectSkipCache(map[string]int64{
		cacheKey: *initial.FileMtime,
	})
	require.NotEmpty(t, fx.engine.SnapshotSkipCache(),
		"test must prime the provider skip-cache branch")
	seedSharedPathProjectRepairConflict(t, fx)

	stats := fx.engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)

	session, err := fx.database.GetSessionFull(
		t.Context(), "traex:"+fx.uuid,
	)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "project", session.Project)
	assert.False(t, session.LastWriteIncremental)
}

func TestTraeXIncrementalDoesNotBorrowCodexRepairState(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	fx := newTraeXRepairFixture(t)
	seedSharedPathProjectRepairConflict(t, fx)

	file, err := os.OpenFile(fx.path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = file.WriteString(
		testjsonl.CodexMsgJSON("assistant", "incremental reply", tsEarlyS5) + "\n",
	)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	info, err := os.Stat(fx.path)
	require.NoError(t, err)
	appendTime := info.ModTime().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(fx.path, appendTime, appendTime))

	fx.engine.SyncPaths([]string{fx.path})

	session, err := fx.database.GetSessionFull(
		t.Context(), "traex:"+fx.uuid,
	)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 2, session.MessageCount)
	assert.Equal(t, "project", session.Project)
	assert.False(t, session.LastWriteIncremental,
		"stale TraeX metadata must force an authoritative full parse")
}

func TestTraeXIncrementalSyncStoresTranscriptMtime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	sessionsRoot := filepath.Join(root, "sessions")
	const uuid = "019fbcca-9fd4-7d20-83dc-0762b2f839b3"
	path := filepath.Join(
		sessionsRoot, "2026", "08", "01",
		"rollout-2026-08-01T18-07-03-"+uuid+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/workspace/project", "codex-tui").
		AddCodexMessage(tsEarlyS1, "user", "first prompt").
		String()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	rolloutTime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	appendTime := rolloutTime.Add(30 * time.Minute)
	indexTime := rolloutTime.Add(time.Hour)
	require.NoError(t, os.Chtimes(path, rolloutTime, rolloutTime))
	indexPath := filepath.Join(root, parser.CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"id":"`+uuid+`","thread_name":"Codex-only title"}`+"\n",
	), 0o644))
	require.NoError(t, os.Chtimes(indexPath, indexTime, indexTime))

	database := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentTraeX: {sessionsRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = file.WriteString(
		testjsonl.CodexMsgJSON("assistant", "incremental reply", tsEarlyS5) + "\n",
	)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, os.Chtimes(path, appendTime, appendTime))

	engine.SyncPaths([]string{path})

	session, err := database.GetSessionFull(t.Context(), "traex:"+uuid)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.FileMtime)
	assert.Equal(t, 2, session.MessageCount)
	assert.True(t, session.LastWriteIncremental)
	assert.Equal(t, appendTime.UnixNano(), *session.FileMtime)
	assert.NotEqual(t, indexTime.UnixNano(), *session.FileMtime,
		"TraeX incremental freshness must ignore Codex session_index.jsonl")
}

// TestTraeXReassignsCodexPathWithoutCrossAgentFreshness covers a TraeX root
// that was previously indexed as Codex. An append made before the first TraeX
// pass must create the TraeX session without adding relabeled messages to the
// Codex row returned by the path-based incremental lookup.
func TestTraeXReassignsCodexPathWithoutCrossAgentFreshness(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	const uuid = "019fbcca-9fd4-7d20-83dc-0762b2f839b3"
	dir := filepath.Join(root, "2026", "08", "01")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(
		dir, "rollout-2026-08-01T18-07-03-"+uuid+".jsonl",
	)
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/workspace/project", "codex-tui").
		AddCodexMessage(tsEarlyS1, "user", "first prompt").
		String()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	database := dbtest.OpenTestDB(t)
	newEngine := func(agent parser.AgentType) *agentsync.Engine {
		engine := agentsync.NewEngine(database, agentsync.EngineConfig{
			AgentDirs: map[parser.AgentType][]string{agent: {root}},
			Machine:   "local",
		})
		t.Cleanup(engine.Close)
		return engine
	}

	codexEngine := newEngine(parser.AgentCodex)
	stats := codexEngine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Equal(t, path, database.GetSessionFilePath("codex:"+uuid))

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = file.WriteString(
		testjsonl.CodexMsgJSON("user", "second prompt", tsEarlyS5) + "\n",
	)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	traexEngine := newEngine(parser.AgentTraeX)
	stats = traexEngine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)

	traexSession, err := database.GetSession(t.Context(), "traex:"+uuid)
	require.NoError(t, err)
	require.NotNil(t, traexSession)
	assert.Equal(t, 2, traexSession.MessageCount)

	codexSession, err := database.GetSession(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	require.NotNil(t, codexSession)
	assert.Equal(t, 1, codexSession.MessageCount,
		"TraeX append must not reach the previous Codex session")
}
