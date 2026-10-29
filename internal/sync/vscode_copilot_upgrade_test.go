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
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestVSCodeCopilotUpgradePreservesIdentity(t *testing.T) {
	const uuid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	for _, tc := range []struct {
		name            string
		agent           parser.AgentType
		previousVersion int
		extension       string
		initial         string
		growingResponse bool
	}{
		{"shared-tool-id", parser.AgentVSCodeCopilot, 146, ".json", `{"version":3,"sessionId":"` + uuid + `","creationDate":1786000235709,` +
			`"requests":[{"requestId":"r1","timestamp":1786000291425,"message":{"text":"newest release?"},"response":[` +
			`{"kind":"toolInvocationSerialized","presentation":"hidden","invocationMessage":"","isComplete":true,"toolCallId":"toolu_fetch","toolId":"copilot_fetchWebPage"},` +
			`{"kind":"toolInvocationSerialized","pastTenseMessage":{"value":"Fetched https://example.com/releases"},"isComplete":true,"toolCallId":"toolu_fetch","toolId":"vscode_fetchWebPage_internal"},` +
			`{"value":"The newest release is v2."}]}]}`, false},
		{"growing-vscode-copilot", parser.AgentVSCodeCopilot, 153, ".jsonl", growingCopilotStart, true},
		{"growing-positron", parser.AgentPositron, 153, ".jsonl", growingCopilotStart, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "workspaceStorage", "abc123", "chatSessions", uuid+tc.extension)
			dbtest.WriteTestFile(t, path, []byte(tc.initial))
			database := dbtest.OpenTestDB(t)
			agentDirs := map[parser.AgentType][]string{tc.agent: {root}}
			engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: agentDirs, Machine: "local"})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			page, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 1)
			before := page.Sessions[0]

			if tc.growingResponse {
				// The old writer kept this first parsed response while advancing
				// the fingerprint to the completed file.
				dbtest.WriteTestFile(t, path, []byte(growingCopilotStart+growingCopilotFinish))
				hash, err := ComputeFileHash(path)
				require.NoError(t, err)
				info, err := os.Stat(path)
				require.NoError(t, err)
				require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET file_hash = ?, file_size = ?, file_mtime = ? WHERE id = ?`, hash, info.Size(), info.ModTime().UnixNano(), before.ID)
					return err
				}))
			} else {
				// Before version 147, the archive kept a hidden placeholder as
				// a second call under the same tool_use_id.
				require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), `INSERT INTO tool_calls
						(message_id, session_id, tool_name, category, tool_use_id, call_index)
						SELECT message_id, session_id, 'copilot_fetchWebPage', category, tool_use_id, call_index
						FROM tool_calls WHERE session_id = ?`, before.ID)
					return err
				}))
			}
			require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", tc.previousVersion))
				return err
			}))
			require.NoError(t, database.SetSessionDataVersion(t.Context(), before.ID, tc.previousVersion))
			engine.Close()
			dbPath := database.Path()
			require.NoError(t, database.Close())

			database, err = db.OpenIsolated(t.Context(), dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			assert.Equal(t, tc.previousVersion, database.GetSessionDataVersion(t.Context(), before.ID))
			engine = NewEngine(t.Context(), database, EngineConfig{AgentDirs: agentDirs, Machine: "local"})
			t.Cleanup(engine.Close)
			// The upgrade must reparse even when the source is unchanged.
			require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))
			assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), before.ID))
			after, err := database.GetSession(t.Context(), before.ID)
			require.NoError(t, err)
			require.NotNil(t, after)
			assert.Equal(t, before.FilePath, after.FilePath)
			if tc.growingResponse {
				assertGrowingCopilotResponse(t, database, before.ID)
			} else {
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
		})
	}
}
