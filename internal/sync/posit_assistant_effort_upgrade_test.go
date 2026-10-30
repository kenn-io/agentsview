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

func TestPositAssistantEffortUpgradeReparsesInPlaceAndLeavesOtherAgents(t *testing.T) {
	const (
		convID          = "22222222-2222-4222-8222-222222222222"
		sessionID       = "posit-assistant:" + convID
		gptmeID         = "gptme:2026-10-19-unaffected"
		previousVersion = 157
	)
	positRoot := t.TempDir()
	convDir := filepath.Join(positRoot, "ws1", convID)
	writeSyncPositAssistantFile(t, filepath.Join(convDir, "conversation.json"), `{
		"schemaVersion": "3",
		"root": {"id": "`+convID+`",
			"timestamp": 1735689600000, "metadata": {"kind": "main"}},
		"messages": [
			{"id": "n1", "parentId": "`+convID+`", "isActive": true,
				"lmMessageIds": [0, 1], "timestamp": 1735689600000}
		],
		"files": []
	}`)
	writeSyncPositAssistantFile(t, filepath.Join(convDir, "lm-messages.jsonl"),
		`{"id":0,"message":{"role":"user","content":"Inspect the auth flow."}}`+"\n"+
			`{"id":1,"message":{"role":"assistant","content":[{"type":"text","text":"Looking."}],"providerOptions":{"providerMetadata":{"positai":{"timestamp":1735689601000,"modelId":"claude-sonnet-4-6","thinkingEffort":"high"}}}}}`+"\n")

	gptmeRoot := t.TempDir()
	gptmePath := filepath.Join(gptmeRoot, "2026-10-19-unaffected", "conversation.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(gptmePath), 0o755))
	require.NoError(t, os.WriteFile(gptmePath, []byte(`{"role":"user","content":"hi","timestamp":"2026-10-19T10:00:00.000000"}
{"role":"assistant","content":"hello","timestamp":"2026-10-19T10:00:01.000000"}
`), 0o600))

	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	config := EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentPositAssistant: {positRoot},
			parser.AgentGptme:          {gptmeRoot},
		},
		Machine: "local",
	}
	engine := NewEngine(t.Context(), database, config)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	hashBefore, ok := database.GetSessionFileHash(t.Context(), sessionID)
	require.True(t, ok)
	require.NotEmpty(t, hashBefore)
	gptmeVersion := database.GetSessionDataVersion(t.Context(), gptmeID)
	engine.Close()
	require.NoError(t, database.Close())

	// Model an archive written before effort capture, without touching the
	// source. Only the Posit Assistant session is stamped with the old version.
	conn, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE messages SET reasoning_effort = '' WHERE session_id = ?", sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE sessions SET data_version = ? WHERE id = ?", previousVersion, sessionID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "PRAGMA user_version = 157")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	assert.Equal(t, []parser.AgentType{parser.AgentPositAssistant},
		db.ScopedDataVersionProviders(previousVersion, previousVersion+1),
		"the effort capture must reparse only Posit Assistant sources in place")
	reopened, err := db.Open(t.Context(), archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	upgraded := NewEngine(t.Context(), reopened, config)
	defer upgraded.Close()
	stale, err := reopened.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, stale, 2)
	require.Empty(t, stale[1].ReasoningEffort)
	require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced,
		"the version bump must reparse only the Posit Assistant source")

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
	assert.Equal(t, "high", messages[1].ReasoningEffort)
	assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(t.Context(), sessionID))
	assert.Equal(t, gptmeVersion, reopened.GetSessionDataVersion(t.Context(), gptmeID),
		"an unaffected agent's session must be left as it was")
	page, err := reopened.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 2, "the upgrade must not create duplicate sessions")
}
