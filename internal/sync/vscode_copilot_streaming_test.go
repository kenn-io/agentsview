package sync

import (
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

func assertGrowingCopilotResponse(t *testing.T, database *db.DB, sessionID string) {
	t.Helper()
	messages, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Contains(t, messages[1].Content, "The newest release is **v0.46.1**.")
	require.Len(t, messages[1].ToolCalls, 2)
}

func TestVSCodeCopilotGrowingResponsePins(t *testing.T) {
	initial, todo, _ := strings.Cut(growingCopilotStart, "\n")
	start := initial + "\n" + `{"kind":2,"k":["requests",0,"response"],"v":[{"value":"Let me check."}]}` + "\n"
	for _, agent := range []parser.AgentType{parser.AgentVSCodeCopilot, parser.AgentPositron} {
		for _, writer := range []string{"ordinary", "bulk", "watcher"} {
			t.Run(string(agent)+"/"+writer, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "workspaceStorage", "abc123", "chatSessions", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl")
				dbtest.WriteTestFile(t, path, []byte(start))
				database := dbtest.OpenTestDB(t)
				engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local"})
				t.Cleanup(engine.Close)
				sync := func() {
					if writer == "watcher" {
						require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
						return
					}
					mode := syncWriteDefault
					if writer == "bulk" {
						mode = syncWriteBulk
					}
					require.Equal(t, 1, engine.syncAllLocked(t.Context(), nil, time.Time{}, nil, mode, true, false).Synced)
				}
				sync()
				sessionID := string(agent) + ":aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
				messages, err := database.GetAllMessages(t.Context(), sessionID)
				require.NoError(t, err)
				require.Len(t, messages, 2)
				note := "Keep this answer"
				_, err = database.PinMessage(t.Context(), sessionID, messages[1].ID, &note)
				require.NoError(t, err)
				beforePins, err := database.ListPinnedMessages(t.Context(), "", "")
				require.NoError(t, err)
				require.Len(t, beforePins, 1)
				dbtest.WriteTestFile(t, path, []byte(start+todo+growingCopilotFinish))
				sync()
				assertGrowingCopilotResponse(t, database, sessionID)
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
