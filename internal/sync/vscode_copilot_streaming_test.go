package sync

import (
	"path/filepath"
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

func TestVSCodeCopilotGrowingResponse(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentVSCodeCopilot, parser.AgentPositron} {
		for _, mode := range []syncWriteMode{syncWriteDefault, syncWriteBulk} {
			name := "ordinary"
			if mode == syncWriteBulk {
				name = "bulk"
			}
			t.Run(string(agent)+"/"+name, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "workspaceStorage", "abc123", "chatSessions", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl")
				dbtest.WriteTestFile(t, path, []byte(growingCopilotStart))
				database := dbtest.OpenTestDB(t)
				engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local"})
				t.Cleanup(engine.Close)
				require.Equal(t, 1, engine.syncAllLocked(t.Context(), nil, time.Time{}, nil, mode, true, false).Synced)
				page, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
				require.NoError(t, err)
				require.Len(t, page.Sessions, 1)
				session := page.Sessions[0]
				messages, err := database.GetAllMessages(t.Context(), session.ID)
				require.NoError(t, err)
				require.Len(t, messages, 2)
				assert.Contains(t, messages[1].Content, "Updated todo list")

				dbtest.WriteTestFile(t, path, []byte(growingCopilotStart+growingCopilotFinish))
				require.Equal(t, 1, engine.syncAllLocked(t.Context(), nil, time.Time{}, nil, mode, true, false).Synced)
				assertGrowingCopilotResponse(t, database, session.ID)
			})
		}
	}
}

func assertGrowingCopilotResponse(t *testing.T, database *db.DB, sessionID string) {
	t.Helper()
	messages, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Contains(t, messages[1].Content, "The newest release is **v0.46.1**.")
	require.Len(t, messages[1].ToolCalls, 2)
	assert.Equal(t, "vscode_fetchWebPage_internal", messages[1].ToolCalls[1].ToolName)
	assert.Equal(t, "toolu_0000000000000000000000001", messages[1].ToolCalls[1].ToolUseID)
}
