package sync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestCommandCodeVersionUpgradeReparsesUnchangedSourceAndClearsSkip(t *testing.T) {
	const (
		rawSessionID = "00000000-0000-0000-0000-000000000001"
		sessionID    = "commandcode:" + rawSessionID
	)
	const previousVersion = 148
	root := t.TempDir()
	projectDir := filepath.Join(root, "example-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, rawSessionID+".jsonl")
	content := []byte(`{"type":"session","version":3,"timestamp":"2026-10-14T21:35:12.880Z","cwd":"/example/project"}
{"type":"message","timestamp":"2026-10-14T21:36:15.163Z","message":{"role":"user","content":[{"type":"text","text":"Check logs"}]}}
{"type":"message","timestamp":"2026-10-14T21:36:20.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Ready."}]}}
`)
	require.NoError(t, os.WriteFile(path, content, 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.OpenIsolated(t.Context(), archivePath)
	require.NoError(t, err)
	// The old parser skipped v3 files without creating a session.
	require.NoError(t, database.ReplaceSkippedFiles(t.Context(), map[string]int64{
		path + db.ProviderAgentSkipMarker + string(parser.AgentCommandCode): info.ModTime().UnixNano(),
	}))
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", previousVersion))
		return err
	}))
	require.NoError(t, database.Close())

	reopened, err := db.OpenIsolated(t.Context(), archivePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	engine := NewEngine(t.Context(), reopened, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCommandCode: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	assert.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(t.Context(), sessionID))
}

func TestCommandCodeV2RewriteReplacesMessages(t *testing.T) {
	const sessionID = "00000000-0000-0000-0000-000000000001"
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, sessionID+".jsonl")
	v2 := `{"timestamp":"2026-10-14T21:00:01Z","sessionId":"` + sessionID + `","role":"user","content":[{"type":"text","text":"Read logs"}],"gitBranch":"main"}
{"timestamp":"2026-10-14T21:00:02Z","sessionId":"` + sessionID + `","role":"assistant","content":[{"type":"tool-call","toolCallId":"call_1","toolName":"Read","input":{"file_path":"server.log"}}],"gitBranch":"main"}
{"timestamp":"2026-10-14T21:00:03Z","sessionId":"` + sessionID + `","role":"tool","content":[{"type":"tool-result","toolCallId":"call_1","toolName":"Read","output":{"type":"text","value":"error: boom"}}],"gitBranch":"main"}
{"timestamp":"2026-10-14T21:00:04Z","sessionId":"` + sessionID + `","role":"user","content":[{"type":"text","text":"Explain the error"}],"gitBranch":"main"}
`
	require.NoError(t, os.WriteFile(path, []byte(v2), 0o600))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCommandCode: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetAllMessages(t.Context(), "commandcode:"+sessionID)
	require.NoError(t, err)
	require.Len(t, before, 3)

	// migrateToV3 folds the tool turn into the following user turn,
	// retaining the tool turn's timestamp. Resuming then adds an assistant.
	v3 := `{"type":"session","version":3,"timestamp":"2026-10-14T21:00:00Z","cwd":"/example/project"}
{"type":"message","timestamp":"2026-10-14T21:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Read logs"}]}}
{"type":"message","timestamp":"2026-10-14T21:00:02Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{"file_path":"server.log"}}]}}
{"type":"message","timestamp":"2026-10-14T21:00:03Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"error: boom"}]},{"type":"text","text":"Explain the error"}]}}
{"type":"message","timestamp":"2026-10-14T21:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"Startup failed."}]},"usage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":0,"cacheWriteTokens":0},"model":"model-a"}
`
	require.NoError(t, os.WriteFile(path, []byte(v3), 0o600))
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	after, err := database.GetAllMessages(t.Context(), "commandcode:"+sessionID)
	require.NoError(t, err)
	require.Len(t, after, 4)
	assert.Equal(t, "Explain the error", after[2].Content)
	assert.Equal(t, "Startup failed.", after[3].Content)
	assert.Equal(t, "assistant", after[3].Role)
	require.Len(t, after[1].ToolCalls, 1)
	assert.Equal(t, "call_1", after[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "error: boom", after[1].ToolCalls[0].ResultContent)
}
