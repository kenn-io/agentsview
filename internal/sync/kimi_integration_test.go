package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

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
	engine := sync.NewEngine(testDB, sync.EngineConfig{
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
	require.NoError(t, testDB.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE sessions SET file_mtime = NULL WHERE id = ?",
			sessionID,
		)
		return err
	}))
	require.NoError(t, engine.SyncSingleSession(sessionID))
	assertSessionProject(t, testDB, sessionID, "kimi-code")
}
