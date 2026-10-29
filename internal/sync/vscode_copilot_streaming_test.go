package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

const growingCopilotStart = `{"kind":0,"v":{"version":3,"sessionId":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","creationDate":1786000235709,"requests":[{"requestId":"r1","timestamp":1786000291425,"message":{"text":"What is the newest release?"},"response":[]}]}}
{"kind":2,"k":["requests",0,"response"],"v":[{"kind":"toolInvocationSerialized","presentation":"hidden","toolId":"manage_todo_list","toolCallId":"00000000-0000-4000-8000-000000000004","pastTenseMessage":{"value":"Updated todo list"},"isComplete":true}]}
`

const growingCopilotFinish = `{"kind":2,"k":["requests",0,"response"],"v":[{"kind":"toolInvocationSerialized","presentation":"hidden","toolId":"copilot_fetchWebPage","toolCallId":"toolu_0000000000000000000000001","isComplete":true},{"kind":"toolInvocationSerialized","toolId":"vscode_fetchWebPage_internal","toolCallId":"toolu_0000000000000000000000001","pastTenseMessage":{"value":"Fetched https://example.com/releases"},"isComplete":true},{"value":"The newest release is **v0.46.1**."}]}
`

func TestVSCodeCopilotGrowingResponsePins(t *testing.T) {
	initial, todo, _ := strings.Cut(growingCopilotStart, "\n")
	textStart := initial + "\n" + `{"kind":2,"k":["requests",0,"response"],"v":[{"value":"Let me check."}]}` + "\n"
	for _, agent := range []parser.AgentType{parser.AgentVSCodeCopilot, parser.AgentPositron} {
		for _, tc := range []struct {
			name                        string
			mode                        syncWriteMode
			watcher, legacy, fullResync bool
		}{
			{name: "ordinary"},
			{name: "bulk", mode: syncWriteBulk},
			{name: "watcher", watcher: true},
			{name: "legacy/ordinary", legacy: true},
			{name: "legacy/bulk", mode: syncWriteBulk, legacy: true},
			{name: "legacy/full-resync", legacy: true, fullResync: true},
		} {
			t.Run(string(agent)+"/"+tc.name, func(t *testing.T) {
				start, finish := textStart, textStart+todo+growingCopilotFinish
				if tc.legacy {
					start, finish = growingCopilotStart, growingCopilotStart+growingCopilotFinish
				}
				root := t.TempDir()
				path := filepath.Join(root, "workspaceStorage", "abc123", "chatSessions", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl")
				dbtest.WriteTestFile(t, path, []byte(start))
				database := dbtest.OpenTestDB(t)
				agentDirs := map[parser.AgentType][]string{agent: {root}}
				engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: agentDirs, Machine: "local"})
				t.Cleanup(engine.Close)
				sync := func() {
					if tc.watcher {
						require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
						return
					}
					require.Equal(t, 1, engine.syncAllLocked(t.Context(), nil, time.Time{}, nil, tc.mode, true, false).Synced)
				}
				sync()
				sessionID := string(agent) + ":aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
				messages, err := database.GetAllMessages(t.Context(), sessionID)
				require.NoError(t, err)
				require.Len(t, messages, 2)
				_, err = database.PinMessage(t.Context(), sessionID, messages[1].ID, new("Keep this answer"))
				require.NoError(t, err)
				dbtest.WriteTestFile(t, path, []byte(finish))
				if tc.legacy {
					// Model the old writer: partial messages, but the completed source's fingerprint.
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
				}
				if tc.fullResync {
					stats := engine.ResyncAll(t.Context(), nil)
					require.False(t, stats.Aborted, "warnings: %v", stats.Warnings)
					require.Zero(t, stats.Failed)
				} else {
					sync()
				}
				messages, err = database.GetAllMessages(t.Context(), sessionID)
				require.NoError(t, err)
				require.Len(t, messages, 2)
				assert.Contains(t, messages[1].Content, "The newest release is **v0.46.1**.")
				require.Len(t, messages[1].ToolCalls, 2)
				assert.Equal(t, path, database.GetSessionFilePath(t.Context(), sessionID))
				assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), sessionID))
				pins, err := database.ListPinnedMessages(t.Context(), sessionID, "")
				require.NoError(t, err)
				require.Len(t, pins, 1)
				assert.Equal(t, 1, pins[0].Ordinal)
			})
		}
	}
}
