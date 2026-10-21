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

func TestGptmeModelUpgradeReparsesUnchangedSource(t *testing.T) {
	const (
		sessionID       = "gptme:2026-10-19-routed-model"
		routedModel     = "openrouter/z-ai/glm-5.3-flash@z-ai"
		wantModel       = "openrouter/z-ai/glm-5.3-flash"
		previousVersion = 139
	)
	root := t.TempDir()
	path := filepath.Join(root, "2026-10-19-routed-model", "conversation.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := []byte(`{"role":"user","content":"hi","timestamp":"2026-10-19T10:00:00.000000"}
{"role":"assistant","content":"hello","timestamp":"2026-10-19T10:00:01.000000","metadata":{"model":"openrouter/z-ai/glm-5.3-flash@z-ai","usage":{"input_tokens":10,"output_tokens":2}}}
`)
	require.NoError(t, os.WriteFile(path, content, 0o600))
	infoBefore, err := os.Stat(path)
	require.NoError(t, err)
	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	config := EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGptme: {root}},
		Machine:   "local",
	}
	engine := NewEngine(t.Context(), database, config)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	hashBefore, ok := database.GetSessionFileHash(t.Context(), sessionID)
	require.True(t, ok)
	require.NotEmpty(t, hashBefore)
	engine.Close()
	require.NoError(t, database.Close())

	// Model an archive written before normalization, without touching its source.
	conn, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE messages SET model = ? WHERE session_id = ? AND role = 'assistant'", routedModel, sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE sessions SET data_version = ? WHERE id = ?", previousVersion, sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "PRAGMA user_version = 139")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	assert.Equal(t, []parser.AgentType{parser.AgentGptme}, db.ScopedDataVersionProviders(previousVersion, previousVersion+1),
		"the model repair must reparse only gptme sources in place")
	reopened, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	upgraded := NewEngine(t.Context(), reopened, config)
	defer upgraded.Close()
	stale, err := reopened.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, stale, 2)
	require.Equal(t, routedModel, stale[1].Model)
	require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced,
		"the version bump must bypass freshness checks on unchanged sources")

	after, err := reopened.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, before.ID, after.ID)
	assert.Equal(t, before.FilePath, after.FilePath)
	hashAfter, ok := reopened.GetSessionFileHash(t.Context(), sessionID)
	require.True(t, ok)
	assert.Equal(t, hashBefore, hashAfter)
	messages, err := reopened.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, wantModel, messages[1].Model)
	assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(t.Context(), sessionID))
	page, err := reopened.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 1, "the upgrade must not create a duplicate session")
	infoAfter, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, infoBefore.ModTime(), infoAfter.ModTime())
	unchanged, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, unchanged)
}
