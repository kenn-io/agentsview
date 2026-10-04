package sync_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestOpenCodeDispatchTiming(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "project-a", "/workspace/project-a")

	const base = int64(1700000000000)
	oc.addSession(t, "dispatch-timing", "project-a", base, base+32000)
	oc.addMessage(t, "msg_user_wait", "dispatch-timing", "user", base)
	oc.addTextPart(t, "part_user_wait", "dispatch-timing", "msg_user_wait", "run the tool", base)
	oc.addMessage(t, "msg_assistant_wait", "dispatch-timing", "assistant", base+1000)
	oc.mustExec(t, "insert waiting tool",
		`INSERT INTO part (id, session_id, message_id, data, time_created, time_updated)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"part_wait", "dispatch-timing", "msg_assistant_wait",
		fmt.Sprintf(`{"type":"tool","tool":"read","callID":"call_wait","state":{"status":"completed","input":{"path":"wait.txt"},"time":{"start":%d,"end":%d}}}`, base+5000, base+27000),
		base+1000, base+1000)

	oc.addMessage(t, "msg_user_missing", "dispatch-timing", "user", base+28000)
	oc.addTextPart(t, "part_user_missing", "dispatch-timing", "msg_user_missing", "run another tool", base+28000)
	oc.addMessage(t, "msg_assistant_missing", "dispatch-timing", "assistant", base+29000)
	oc.mustExec(t, "insert missing-start tool",
		`INSERT INTO part (id, session_id, message_id, data, time_created, time_updated)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"part_missing", "dispatch-timing", "msg_assistant_missing",
		fmt.Sprintf(`{"type":"tool","tool":"read","callID":"call_missing","state":{"status":"completed","input":{"path":"missing.txt"},"time":{"end":%d}}}`, base+30000),
		base+29000, base+29000)

	oc.addMessage(t, "msg_user_interrupted", "dispatch-timing", "user", base+30001)
	oc.addTextPart(t, "part_user_interrupted", "dispatch-timing", "msg_user_interrupted", "run the cancelled tool", base+30001)
	oc.addMessage(t, "msg_assistant_interrupted", "dispatch-timing", "assistant", base+30500)
	oc.mustExec(t, "insert interrupted tool",
		`INSERT INTO part (id, session_id, message_id, data, time_created, time_updated)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"part_interrupted", "dispatch-timing", "msg_assistant_interrupted",
		fmt.Sprintf(`{"type":"tool","tool":"read","callID":"call_interrupted","state":{"status":"error","input":{"path":"cancelled.txt"},"error":"interrupted","metadata":{"interrupted":true},"time":{"start":%d,"end":%d}}}`, base+31000, base+31000),
		base+30500, base+30500)

	stats := env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	assert.Equal(t, 1, stats.Synced)

	messages, err := env.db.GetAllMessages(t.Context(), "opencode:dispatch-timing")
	require.NoError(t, err)
	require.Len(t, messages, 6)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			switch call.ToolUseID {
			case "call_wait":
				t.Logf("call_wait parser event_count=%d; expected duration_ms=22000", len(call.ResultEvents))
				require.Len(t, call.ResultEvents, 2)
				assert.Equal(t, "tool_execution", call.ResultEvents[0].Source)
				assert.Equal(t, "started", call.ResultEvents[0].Status)
				assert.Equal(t, "tool_execution", call.ResultEvents[1].Source)
			case "call_missing":
				assert.Empty(t, call.ResultEvents)
			case "call_interrupted":
				t.Logf("call_interrupted parser event_count=%d", len(call.ResultEvents))
				assert.Empty(t, call.ResultEvents)
			}
		}
	}

	timing, err := env.db.GetSessionTiming(t.Context(), "opencode:dispatch-timing")
	require.NoError(t, err)
	require.NotNil(t, timing)
	require.Len(t, timing.Turns, 3)
	var foundWait, foundMissing, foundInterrupted bool
	var waitDuration, missingDuration, interruptedDuration *int64
	for _, turn := range timing.Turns {
		require.Len(t, turn.Calls, 1)
		call := turn.Calls[0]
		switch call.ToolUseID {
		case "call_wait":
			foundWait = true
			require.NotNil(t, call.DurationMs)
			waitDuration = call.DurationMs
			assert.Equal(t, int64(22000), *call.DurationMs)
		case "call_missing":
			foundMissing = true
			missingDuration = call.DurationMs
			assert.Nil(t, call.DurationMs)
		case "call_interrupted":
			foundInterrupted = true
			interruptedDuration = call.DurationMs
			assert.Nil(t, call.DurationMs)
		}
	}
	t.Logf("call_wait duration_ms=%d; call_missing duration_ms=%v; call_interrupted duration_ms=%v", *waitDuration, missingDuration, interruptedDuration)
	assert.True(t, foundWait)
	assert.True(t, foundMissing)
	assert.True(t, foundInterrupted)

	// Simulate an archive written before timing pairs while the native source stays unchanged.
	sourceBefore, err := os.ReadFile(oc.path)
	require.NoError(t, err)
	require.NoError(t, env.db.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM tool_result_events WHERE session_id = ?`, "opencode:dispatch-timing")
		return err
	}))
	require.NoError(t, env.db.SetSessionDataVersion(t.Context(), "opencode:dispatch-timing", db.CurrentDataVersion()-1))
	stats = env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	assert.Equal(t, 1, stats.Synced)
	sourceAfter, err := os.ReadFile(oc.path)
	require.NoError(t, err)
	assert.Equal(t, sourceBefore, sourceAfter)
	assert.Equal(t, db.CurrentDataVersion(), env.db.GetSessionDataVersion(t.Context(), "opencode:dispatch-timing"))
	messages, err = env.db.GetAllMessages(t.Context(), "opencode:dispatch-timing")
	require.NoError(t, err)
	var eventCount int
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			eventCount += len(call.ResultEvents)
		}
	}
	assert.Equal(t, 2, eventCount)
	revision, err := env.db.TranscriptRevision(t.Context(), "opencode:dispatch-timing")
	require.NoError(t, err)
	stats = env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	assert.Zero(t, stats.Synced)
	again, err := env.db.TranscriptRevision(t.Context(), "opencode:dispatch-timing")
	require.NoError(t, err)
	assert.Equal(t, revision, again)

	orphanPath := filepath.Join(t.TempDir(), "removed-opencode.db")
	require.NoError(t, env.db.UpsertSession(t.Context(), db.Session{
		ID: "opencode:orphan", Project: "project-a", Machine: "local", Agent: "opencode",
		FilePath: &orphanPath, MessageCount: 1,
	}))
	require.NoError(t, env.db.InsertMessages(t.Context(), []db.Message{{
		SessionID: "opencode:orphan", Ordinal: 0, Role: "user", Content: "archived prompt",
	}}))
	rebuild := env.engine.ResyncAll(t.Context(), nil)
	require.False(t, rebuild.Aborted)
	orphan, err := env.db.GetSession(t.Context(), "opencode:orphan")
	require.NoError(t, err)
	require.NotNil(t, orphan)
	orphanMessages, err := env.db.GetAllMessages(t.Context(), "opencode:orphan")
	require.NoError(t, err)
	require.Len(t, orphanMessages, 1)
	assert.Equal(t, "archived prompt", orphanMessages[0].Content)
}
