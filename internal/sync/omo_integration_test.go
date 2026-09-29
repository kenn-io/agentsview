package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestOMOSyncIndexesOneOMORowAndStaysIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	sessionPath := filepath.Join(root, "encoded-cwd", "session-omo.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(`{"type":"session","version":3,"id":"session-omo","timestamp":"2025-01-01T10:00:00Z","cwd":"/Users/alice/code/pi-project"}
{"type":"message","id":"msg-1","timestamp":"2025-01-01T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Inspect the source."}]}}
{"type":"message","id":"msg-2","timestamp":"2025-01-01T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"Looks ready."},{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"ls"}}],"model":"claude-opus-4-5"}}
`), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOMO: {root},
		},
		Machine: "local",
	})

	first := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 0, first.Failed)
	require.Equal(t, 1, countAgentSessions(t, database, string(parser.AgentOMO)))
	require.Equal(t, 0, countAgentSessions(t, database, string(parser.AgentPi)))

	var storedID string
	err := database.Reader().QueryRow(t.Context(),
		"SELECT id FROM sessions WHERE agent = ?", string(parser.AgentOMO),
	).Scan(&storedID)
	require.NoError(t, err)
	require.Equal(t, "omo:session-omo", storedID)
	sessionID := storedID
	sess, err := database.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, string(parser.AgentOMO), sess.Agent)
	assert.Empty(t, sess.Entrypoint)
	assert.Equal(t, "pi_project", sess.Project)
	var storedPath string
	err = database.Reader().QueryRow(t.Context(),
		"SELECT file_path FROM sessions WHERE id = ?", sessionID,
	).Scan(&storedPath)
	require.NoError(t, err)
	assert.Equal(t, sessionPath, storedPath)
	assertToolCallCount(t, database, sessionID, 1)

	second := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 0, second.Failed)
	assert.Equal(t, 1, countAgentSessions(t, database, string(parser.AgentOMO)))
	assert.Equal(t, 0, countAgentSessions(t, database, string(parser.AgentPi)))
	again, err := database.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, again)
	var storedPathAgain string
	err = database.Reader().QueryRow(t.Context(),
		"SELECT file_path FROM sessions WHERE id = ?", sessionID,
	).Scan(&storedPathAgain)
	require.NoError(t, err)
	assert.Equal(t, sessionPath, storedPathAgain)
	assert.Equal(t, sess.Project, again.Project)
}

func countAgentSessions(t *testing.T, database *db.DB, agent string) int {
	t.Helper()
	var count int
	err := database.Reader().QueryRow(t.Context(),
		"SELECT COUNT(*) FROM sessions WHERE agent = ?", agent,
	).Scan(&count)
	require.NoError(t, err)
	return count
}
