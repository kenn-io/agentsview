package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func writeCopilotSyncFixture(
	t *testing.T, root, sessionID string, lines []string, mtime time.Time,
) string {
	t.Helper()

	sessionDir := filepath.Join(root, "session-state", sessionID)
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	eventsPath := filepath.Join(sessionDir, "events.jsonl")
	require.NoError(t, os.WriteFile(
		eventsPath,
		[]byte(strings.Join(lines, "\n")+"\n"),
		0o644,
	))
	require.NoError(t, os.Chtimes(eventsPath, mtime, mtime))
	return eventsPath
}

// Copilot appends execution events after the assistant tool-call message.
// A later full parse must replace the stored message so those retroactively
// paired events update the existing tool call rather than only adding the
// newly emitted tool-result ordinal.
func TestSyncCopilotLateExecutionEventsUpdateStoredToolCall(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCopilot: {root},
		},
		Machine: "local",
	})

	base := time.Date(2026, time.July, 24, 11, 56, 0, 0, time.UTC)
	sessionID := "late-tool-events"
	lines := []string{
		`{"type":"session.start","data":{"sessionId":"late-tool-events"},"timestamp":"2026-07-24T11:56:20.000Z"}`,
		`{"type":"user.message","data":{"content":"Finish the task"},"timestamp":"2026-07-24T11:56:21.000Z"}`,
		`{"type":"assistant.message","data":{"toolRequests":[{"toolCallId":"call_example","name":"task_complete","arguments":"{}"}]},"timestamp":"2026-07-24T11:56:24.189Z"}`,
	}
	eventsPath := writeCopilotSyncFixture(
		t, root, sessionID, lines, base,
	)

	stats := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced)

	storedID := "copilot:" + sessionID
	msgs, err := testDB.GetAllMessages(context.Background(), storedID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Empty(t, msgs[1].ToolCalls[0].ResultEvents)

	lines = append(lines,
		`{"type":"tool.execution_start","data":{"toolCallId":"call_example"},"timestamp":"2026-07-24T11:56:24.198Z"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"call_example","success":true,"result":"done"},"timestamp":"2026-07-24T11:56:27.923Z"}`,
	)
	require.NoError(t, os.WriteFile(
		eventsPath,
		[]byte(strings.Join(lines, "\n")+"\n"),
		0o644,
	))
	later := base.Add(time.Minute)
	require.NoError(t, os.Chtimes(eventsPath, later, later))

	stats = engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced)

	msgs, err = testDB.GetAllMessages(context.Background(), storedID)
	require.NoError(t, err)
	require.Len(t, msgs, 2,
		"the paired result carrier must not become an extra stored message")
	require.Len(t, msgs[1].ToolCalls, 1)
	events := msgs[1].ToolCalls[0].ResultEvents
	require.Len(t, events, 2)
	assert.Equal(t, "started", events[0].Status)
	assert.Equal(t, "2026-07-24T11:56:24.198Z", events[0].Timestamp)
	assert.Equal(t, "completed", events[1].Status)
	assert.Equal(t, "2026-07-24T11:56:27.923Z", events[1].Timestamp)
}
