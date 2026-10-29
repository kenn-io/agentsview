package sync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestVSCodeCopilotSharedToolCallIDResyncPreservesIdentity(t *testing.T) {
	root := t.TempDir()
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	path := filepath.Join(root, "workspaceStorage", "abc123", "chatSessions", uuid+".json")
	dbtest.WriteTestFile(t, path, []byte(`{"version":3,"sessionId":"`+uuid+`","creationDate":1786000235709,`+
		`"requests":[{"requestId":"r1","timestamp":1786000291425,"message":{"text":"newest release?"},"response":[`+
		`{"kind":"toolInvocationSerialized","presentation":"hidden","invocationMessage":"","isComplete":true,"toolCallId":"toolu_fetch","toolId":"copilot_fetchWebPage"},`+
		`{"kind":"toolInvocationSerialized","pastTenseMessage":{"value":"Fetched https://example.com/releases"},"isComplete":true,"toolCallId":"toolu_fetch","toolId":"vscode_fetchWebPage_internal"},`+
		`{"value":"The newest release is v2."}]}]}`))

	database := dbtest.OpenTestDB(t)
	agentDirs := map[parser.AgentType][]string{parser.AgentVSCodeCopilot: {root}}
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: agentDirs, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	page, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	before := page.Sessions[0]

	// Model a released archive that stored the hidden placeholder as a
	// second call under the same tool_use_id, before data version 147.
	const previousVersion = 146
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO tool_calls
			(message_id, session_id, tool_name, category, tool_use_id, call_index)
			SELECT message_id, session_id, 'copilot_fetchWebPage', category, tool_use_id, call_index
			FROM tool_calls WHERE session_id = ?`, before.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", previousVersion))
		return err
	}))
	require.NoError(t, database.SetSessionDataVersion(t.Context(), before.ID, previousVersion))
	engine.Close()
	dbPath := database.Path()
	require.NoError(t, database.Close())

	database, err = db.OpenIsolated(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	assert.Equal(t, previousVersion, database.GetSessionDataVersion(t.Context(), before.ID))
	engine = NewEngine(t.Context(), database, EngineConfig{AgentDirs: agentDirs, Machine: "local"})
	t.Cleanup(engine.Close)
	// The version upgrade must reparse even when the source is unchanged.
	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))
	assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), before.ID))

	after, err := database.GetSession(t.Context(), before.ID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, before.FilePath, after.FilePath)
	msgs, err := database.GetAllMessages(t.Context(), before.ID)
	require.NoError(t, err)
	var calls []string
	for _, msg := range msgs {
		for _, call := range msg.ToolCalls {
			calls = append(calls, call.ToolUseID+" "+call.ToolName)
		}
	}
	assert.Equal(t, []string{"toolu_fetch vscode_fetchWebPage_internal"}, calls)
}

func TestVSCodeCopilotGrowingUpgradePins(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentVSCodeCopilot, parser.AgentPositron} {
		for _, mode := range []syncWriteMode{syncWriteDefault, syncWriteBulk} {
			t.Run(fmt.Sprintf("%s/%d", agent, mode), func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "workspaceStorage", "abc123", "chatSessions", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl")
				dbtest.WriteTestFile(t, path, []byte(growingCopilotStart))
				database := dbtest.OpenTestDB(t)
				agentDirs := map[parser.AgentType][]string{agent: {root}}
				engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: agentDirs, Machine: "local"})
				t.Cleanup(engine.Close)
				require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
				sessionID := string(agent) + ":aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
				before, err := database.GetSession(t.Context(), sessionID)
				require.NoError(t, err)
				require.NotNil(t, before)
				messages, err := database.GetAllMessages(t.Context(), sessionID)
				require.NoError(t, err)
				require.Len(t, messages, 2)
				note := "Keep this answer"
				_, err = database.PinMessage(t.Context(), sessionID, messages[1].ID, &note)
				require.NoError(t, err)
				beforePins, err := database.ListPinnedMessages(t.Context(), "", "")
				require.NoError(t, err)
				require.Len(t, beforePins, 1)
				// Model the old writer: partial messages, but the completed source's fingerprint.
				dbtest.WriteTestFile(t, path, []byte(growingCopilotStart+growingCopilotFinish))
				hash, err := ComputeFileHash(path)
				require.NoError(t, err)
				info, err := os.Stat(path)
				require.NoError(t, err)
				require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(t.Context(), `UPDATE messages SET source_uuid = '' WHERE session_id = ?`, sessionID); err != nil {
						return err
					}
					if _, err := tx.ExecContext(t.Context(), `UPDATE sessions SET file_hash = ?, file_size = ?, file_mtime = ?, data_version = 153 WHERE id = ?`, hash, info.Size(), info.ModTime().UnixNano(), sessionID); err != nil {
						return err
					}
					_, err := tx.ExecContext(t.Context(), "PRAGMA user_version = 153")
					return err
				}))
				engine.Close()
				dbPath := database.Path()
				require.NoError(t, database.Close())
				database, err = db.OpenIsolated(t.Context(), dbPath)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, database.Close()) })
				assert.Equal(t, 153, database.GetSessionDataVersion(t.Context(), sessionID))
				engine = NewEngine(t.Context(), database, EngineConfig{AgentDirs: agentDirs, Machine: "local"})
				t.Cleanup(engine.Close)
				require.Equal(t, 1, engine.syncAllLocked(t.Context(), nil, time.Time{}, nil, mode, true, false).Synced)
				assertGrowingCopilotResponse(t, database, sessionID)
				after, err := database.GetSession(t.Context(), sessionID)
				require.NoError(t, err)
				require.NotNil(t, after)
				assert.Equal(t, before.FilePath, after.FilePath)
				assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), sessionID))
				pins, err := database.ListPinnedMessages(t.Context(), "", "")
				require.NoError(t, err)
				require.Len(t, pins, 1)
				assert.Equal(t, &note, pins[0].Note)
				assert.Equal(t, beforePins[0].CreatedAt, pins[0].CreatedAt)
				require.NotNil(t, pins[0].Content)
				assert.Contains(t, *pins[0].Content, "The newest release is **v0.46.1**.")
			})
		}
	}
}
