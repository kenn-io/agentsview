package db_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestLegacyUpgradePreservesIncompleteRetry(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	body := testjsonl.CodexForkedSessionMetaJSON(id, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", root, "codex_cli_rs", "2026-10-08T12:00:00Z") + "\n" + testjsonl.CodexMsgJSON("user", "child without parent turns", "2026-10-08T12:00:01Z") + "\n"
	dbtest.WriteTestFile(t, filepath.Join(root, "rollout-"+id+".jsonl"), []byte(body))
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "PRAGMA user_version=129")
		return err
	}))
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()
	require.Equal(t, db.CurrentDataVersion()-1, database.GetSessionDataVersion(t.Context(), "codex:"+id), "missing parent output must remain retryable")
	// Reproduce the retry stamp written by the legacy parser before reopening.
	require.NoError(t, database.SetSessionDataVersion(t.Context(), "codex:"+id, 129))
	path := database.Path()
	require.NoError(t, database.Close())
	reopened, err := db.OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	assert.True(t, reopened.NeedsResync())
	assert.Equal(t, 129, reopened.GetSessionDataVersion(t.Context(), "codex:"+id))
	require.NoError(t, reopened.Close())
}

func TestSimulatedScopedUpgradeReparsesUnchangedFreebuffSource(t *testing.T) {
	version := db.CurrentDataVersion()
	t.Cleanup(db.InstallDataVersionScopesForTest(map[int][]parser.AgentType{version: {parser.AgentCodebuff}}))
	database := dbtest.OpenTestDB(t)
	root, unaffected := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "project", "chats", "2026-09-15T10-00-00.000Z", "chat-messages.json")
	dbtest.WriteTestFile(t, source, []byte(`[{"id":"user-1","variant":"user","content":"hello","timestamp":"10:00 AM"}]`))
	dbtest.WriteTestFile(t, filepath.Join(filepath.Dir(source), "run-state.json"), []byte(`{"sessionState":{"mainAgentState":{"agentType":"base2-free-minimax-m3"}}}`))
	dbtest.WriteTestFile(t, filepath.Join(unaffected, "project", "unchanged.jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-10-08T12:00:00Z", "unaffected").String()))
	cfg := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodebuff: {root}, parser.AgentClaude: {unaffected}}}
	engine := sync.NewEngine(t.Context(), database, cfg)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()
	var id string
	require.NoError(t, database.Reader().QueryRow(t.Context(), "SELECT id FROM sessions WHERE agent='freebuff'").Scan(&id))
	messages, err := database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	messages[0].Content = "obsolete parser output"
	messages[0].ContentLength = len(messages[0].Content)
	require.NoError(t, database.ReplaceSessionMessages(t.Context(), id, messages))
	_, found, err := database.GetProviderStatHash(t.Context(), parser.AgentCodebuff, source)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), fmt.Sprintf("UPDATE sessions SET data_version=%d; PRAGMA user_version=%d", version-1, version-1))
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	database, err = db.OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	needsSync, _, err := db.ArchiveResyncState(t.Context(), path)
	require.NoError(t, err)
	require.True(t, needsSync)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	cfg.AgentDirs[parser.AgentCodebuff] = []string{root, unaffected}
	dbtest.WriteTestFile(t, filepath.Join(unaffected, "project", "chats", "broken", "chat-messages.json"), []byte("{"))
	engine = sync.NewEngine(t.Context(), database, cfg)
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 1, stats.Synced)
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 1, stats.Failed)
	needsSync, _, err = db.ArchiveResyncState(t.Context(), path)
	require.NoError(t, err)
	assert.False(t, needsSync)
	assert.Equal(t, source, database.GetSessionFilePath(t.Context(), id))
	messages, err = database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "hello", messages[0].Content)
}

func TestScopedUpgradeWaitsForAuthoritativeFullPass(t *testing.T) {
	version := db.CurrentDataVersion()
	t.Cleanup(db.InstallDataVersionScopesForTest(
		map[int][]parser.AgentType{version: {parser.AgentCodebuff}},
	))
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version=%d", version-1))
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	database, err := db.OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.False(t, database.NeedsResync(), "a scoped upgrade must not force a rebuild")

	waiting := func() (startup, coldRead bool) {
		t.Helper()
		startup, _, err := db.ArchiveResyncState(t.Context(), path)
		require.NoError(t, err)
		reader, err := db.OpenReadOnly(t.Context(), path)
		require.NoError(t, err)
		defer func() { require.NoError(t, reader.Close()) }()
		return startup, reader.NeedsResync()
	}
	startup, coldRead := waiting()
	assert.True(t, startup)
	assert.True(t, coldRead, "cold reads must wait for affected sources")

	clean := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodebuff: {t.TempDir()},
	}}
	broken := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(broken, "opencode.db"), []byte("not a database"))
	failing := sync.EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{
		parser.AgentOpenCode: {broken},
	}}
	passes := []struct {
		name string
		cfg  sync.EngineConfig
		run  func(*sync.Engine) sync.SyncStats
		done bool
	}{
		{
			name: "incremental pass",
			cfg:  clean,
			run: func(e *sync.Engine) sync.SyncStats {
				return e.SyncAllSince(t.Context(), time.Now(), nil)
			},
		},
		{
			name: "full pass with provider failure",
			cfg:  failing,
			run: func(e *sync.Engine) sync.SyncStats {
				return e.SyncAll(t.Context(), nil)
			},
		},
		{
			name: "authoritative full pass",
			cfg:  clean,
			run: func(e *sync.Engine) sync.SyncStats {
				return e.SyncAll(t.Context(), nil)
			},
			done: true,
		},
	}
	for _, pass := range passes {
		engine := sync.NewEngine(t.Context(), database, pass.cfg)
		pass.run(engine)
		engine.Close()
		startup, coldRead := waiting()
		assert.Equal(t, !pass.done, startup, pass.name)
		assert.Equal(t, !pass.done, coldRead, pass.name)
	}
}
