package sync

import (
	"context"
	"database/sql"
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

func TestSourceMtimeWindsurfUsesProviderFingerprint(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Windsurf", "User")
	workspaceDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
	manifestPath := filepath.Join(workspaceDir, "workspace.json")
	dbPath := filepath.Join(workspaceDir, "state.vscdb")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	require.NoError(t, os.WriteFile(manifestPath, []byte(`{"folder":"file:///work/demo"}`), 0o644))
	writeSyncWindsurfStateDB(t, dbPath, `{
		"version": 1,
		"sessionId": "mtime-session",
		"requests": [{
			"requestId": "request-1",
			"message": {"text": "Question"},
			"response": [{"value": "Answer"}],
			"timestamp": 1710000000000
		}]
	}`)
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentWindsurf: {root},
		},
		Machine: "devbox",
	})
	defer engine.Close()

	stats := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced)
	virtualPath := dbPath + "#mtime-session"
	assert.Equal(t, virtualPath, engine.FindSourceFile("windsurf:mtime-session"))
	before := engine.SourceMtime("windsurf:mtime-session")
	require.NotZero(t, before)

	future := time.Unix(0, before).Add(2 * time.Second)
	require.NoError(t, os.Chtimes(manifestPath, future, future))

	after := engine.SourceMtime("windsurf:mtime-session")
	assert.Greater(t, after, before)
}

func writeSyncWindsurfStateDB(t *testing.T, dbPath, payload string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)
	_, err = conn.Exec(
		`INSERT INTO ItemTable (key, value) VALUES (?, ?)`,
		"workbench.panel.aichat.view.aichat.chatdata",
		payload,
	)
	require.NoError(t, err)
}
