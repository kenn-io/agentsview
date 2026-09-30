package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestSyncKimiConfigUpdateCwdPrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	kimiDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), testDB, sync.EngineConfig{
		AgentDirs:          map[parser.AgentType][]string{parser.AgentKimi: {kimiDir}},
		IncludeCwdPrefixes: []string{"/Users/helix/Code"},
		Machine:            "local",
	})

	workdirDir := "wd_kimi-code_057f5c09ee3f"
	sessionDir := "session_uuid-cwd"
	wirePath := filepath.Join(kimiDir, workdirDir, sessionDir, "wire.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(wirePath), 0o755))
	fixture, err := os.ReadFile(filepath.Join(
		"..", "parser", "testdata", "kimi-config-update-cwd.jsonl",
	))
	require.NoError(t, err)
	fixture = append(fixture,
		[]byte(`{"type":"turn.prompt","input":[{"type":"text","text":"cwd"}]}`+"\n")...)
	require.NoError(t, os.WriteFile(wirePath, fixture, 0o644))

	sessionID := "kimi:" + workdirDir + ":" + sessionDir
	engine.SyncPaths([]string{wirePath})

	session, err := testDB.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "/Users/helix/Code/mcp-hub", session.Cwd)
}

// A title file written after the transcript makes the source newer than
// wire.jsonl alone. The stored freshness must match that, or every sync
// reparses the session.
func TestSyncKimiTitledSessionSkipsWhenUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	kimiDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	cfg := sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentKimi: {kimiDir}},
		Machine:   "local",
	}

	sessionDir := filepath.Join(kimiDir, "wd_kimi-code_057f5c09ee3f", "session_titled")
	wirePath := filepath.Join(sessionDir, "wire.jsonl")
	statePath := filepath.Join(sessionDir, "state.json")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(wirePath, []byte(
		`{"type": "metadata", "protocol_version": "1.3"}`+"\n"+
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Hello Kimi"}]}}}`+"\n"+
			`{"timestamp": 1704067202.0, "message": {"type": "TurnEnd", "payload": {}}}`+"\n",
	), 0o644))
	require.NoError(t, os.WriteFile(statePath,
		[]byte(`{"custom_title":"Kimi title"}`), 0o644))
	wireTime := time.Date(2026, time.June, 4, 10, 0, 0, 0, time.UTC)
	stateTime := wireTime.Add(5 * time.Minute)
	require.NoError(t, os.Chtimes(wirePath, wireTime, wireTime))
	require.NoError(t, os.Chtimes(statePath, stateTime, stateTime))

	first := sync.NewEngine(t.Context(), testDB, cfg)
	t.Cleanup(first.Close)
	stats := first.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)

	second := sync.NewEngine(t.Context(), testDB, cfg)
	t.Cleanup(second.Close)
	stats = second.SyncAll(t.Context(), nil)
	assert.Equal(t, 0, stats.Synced)
	assert.Equal(t, 1, stats.Skipped)
}

// TestSyncPathsAndSingleSession_KimiNewLayout pins down both engine
// sites that previously broke for the new .kimi-code layout:
//
//   - SyncPaths (file-watcher entry point) must classify a 5-segment
//     new-layout wire.jsonl and import it with the decoded project
//     name rather than dropping the event.
//   - SyncSingleSession must re-derive the project from the workdir
//     directory, not the literal "agents" segment two levels up.
func TestSyncPathsAndSingleSession_KimiNewLayout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	kimiDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentKimi: {kimiDir},
		},
		Machine: "local",
	})

	workdirDir := "wd_kimi-code_057f5c09ee3f"
	sessionDir := "session_cf2c3d74-c9d2-4ae4-95b7-d1d817298382"
	wirePath := filepath.Join(
		kimiDir, workdirDir, sessionDir, "agents", "main", "wire.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(wirePath), 0o755))
	require.NoError(t, os.WriteFile(wirePath, []byte(
		`{"type": "metadata", "protocol_version": "1.3"}`+"\n"+
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Hello Kimi"}]}}}`+"\n"+
			`{"timestamp": 1704067202.0, "message": {"type": "TurnEnd", "payload": {}}}`+"\n",
	), 0o644))

	sessionID := "kimi:" + workdirDir + ":main:" + sessionDir

	// SyncPaths routes through classifyOnePath; the new-layout file
	// must be classified, imported, and carry the decoded project.
	engine.SyncPaths([]string{wirePath})
	assertSessionProject(t, testDB, sessionID, "kimi-code")

	// Force a single-session resync by clearing file_mtime; the
	// project must remain the decoded workdir, not "agents".
	require.NoError(t, testDB.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			"UPDATE sessions SET file_mtime = NULL WHERE id = ?",
			sessionID,
		)
		return err
	}))
	require.NoError(t, engine.SyncSingleSession(sessionID))
	assertSessionProject(t, testDB, sessionID, "kimi-code")
}
