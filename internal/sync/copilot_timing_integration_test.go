package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopilotBlockedResultPreservesExecutionTiming(t *testing.T) {
	root := t.TempDir()
	eventsPath := filepath.Join(root, "session-state", "timing", "events.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(eventsPath), 0o755))
	initialEvents := `{"type":"session.start","data":{"sessionId":"timing"},"timestamp":"2026-04-26T10:00:00Z"}
{"type":"user.message","data":{"content":"read the file"},"timestamp":"2026-04-26T10:00:00Z"}
{"type":"assistant.message","data":{"toolRequests":[{"toolCallId":"call_read","name":"view","arguments":"{\"path\":\"README.md\"}"}]},"timestamp":"2026-04-26T10:00:01Z"}
{"type":"tool.execution_start","data":{"toolCallId":"call_read"},"timestamp":"2026-04-26T10:00:01.100Z"}
`
	require.NoError(t, os.WriteFile(eventsPath, []byte(initialEvents), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCopilot: {root},
		},
		Machine:                 "local",
		BlockedResultCategories: []string{"Read", "Glob"},
	})
	stats := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced)
	beforeCompletion, err := database.GetMessages(context.Background(), "copilot:timing", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, beforeCompletion, 2)
	require.Len(t, beforeCompletion[1].ToolCalls, 1)
	require.Len(t, beforeCompletion[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "started", beforeCompletion[1].ToolCalls[0].ResultEvents[0].Status)

	completionEvents := `{"type":"tool.execution_complete","data":{"toolCallId":"call_read","success":true,"result":"private file body"},"timestamp":"2026-04-26T10:00:04.825Z"}
{"type":"user.message","data":{"content":"next request"},"timestamp":"2026-04-26T22:38:05Z"}
{"type":"session.shutdown","data":{},"timestamp":"2026-04-26T22:38:06Z"}
`
	file, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = file.WriteString(completionEvents)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	stats = engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced)

	messages, err := database.GetMessages(context.Background(), "copilot:timing", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	require.Len(t, messages[1].ToolCalls, 1)
	call := messages[1].ToolCalls[0]
	assert.Empty(t, call.ResultContent)
	require.Len(t, call.ResultEvents, 2)
	assert.Empty(t, call.ResultEvents[1].Content)
	assert.Equal(t, "completed", call.ResultEvents[1].Status)
	assert.Equal(t, "2026-04-26T10:00:04.825Z", call.ResultEvents[1].Timestamp)

	timing, err := database.GetSessionTiming(context.Background(), "copilot:timing")
	require.NoError(t, err)
	require.NotNil(t, timing)
	require.Len(t, timing.Turns, 1)
	require.Len(t, timing.Turns[0].Calls, 1)
	require.NotNil(t, timing.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(3_725), *timing.Turns[0].Calls[0].DurationMs)
	require.NotNil(t, timing.Turns[0].DurationMs)
	assert.Equal(t, int64(3_825), *timing.Turns[0].DurationMs)
}
