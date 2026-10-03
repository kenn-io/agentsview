package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestNativeBodyUpgradeReparsesUnchangedSourceAndKeepsOrphan(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project", "native-upgrade.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"type":"user","uuid":"prompt","timestamp":"2026-01-01T10:00:00Z","message":{"role":"user","content":"hello"}}`+"\n"+
			`{"type":"assistant","uuid":"answer","timestamp":"2026-01-01T10:00:01Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"plan"},{"type":"text","text":"answer"}]}}`+"\n",
	), 0o600))
	database := openTestDB(t)
	cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local"}
	engine := NewEngine(t.Context(), database, cfg)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()
	archivePath := database.Path()
	require.NoError(t, database.Close())
	// Fixture writes retain the archive's registered projection functions.
	raw, err := sql.Open("agentsview_archive_sqlite3", archivePath)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `UPDATE messages SET content='[Thinking] plan\nanswer',thinking_text='',content_layout=NULL WHERE role='assistant'`)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `UPDATE sessions SET data_version=113`)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `INSERT INTO sessions (id,agent,project,machine,message_count) VALUES ('retained-native','gemini','project','local',1)`)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `INSERT INTO messages (session_id,ordinal,role,content,content_length) VALUES ('retained-native',0,'assistant','[Thinking] archived only',24)`)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `PRAGMA user_version=113`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	reopened, err := db.OpenIsolated(t.Context(), archivePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.True(t, reopened.NeedsResync())
	upgraded := NewEngine(t.Context(), reopened, cfg)
	t.Cleanup(upgraded.Close)
	stats, err := upgraded.SyncThenRun(t.Context(), false, nil, func(full bool) error {
		assert.True(t, full)
		return nil
	})
	require.NoError(t, err)
	require.False(t, stats.Aborted)
	require.Zero(t, stats.Failed)
	messages, err := reopened.GetAllMessages(t.Context(), "native-upgrade")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "answer", messages[1].Content)
	assert.Equal(t, "plan", messages[1].ThinkingText)
	assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "thinking", End: 4}, {Kind: "text", End: 6}}}, messages[1].ContentLayout)
	retained, err := reopened.GetAllMessages(t.Context(), "retained-native")
	require.NoError(t, err)
	require.Len(t, retained, 1)
	assert.Equal(t, "[Thinking] archived only", retained[0].Content)
	assert.Nil(t, retained[0].ContentLayout)
	assert.False(t, reopened.NeedsResync())
}
