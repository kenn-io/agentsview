package sync_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		fmt.Sprintf(`{"type":"tool","tool":"read","callID":"call_interrupted","state":{"status":"completed","input":{"path":"cancelled.txt"},"metadata":{"interrupted":true},"time":{"start":%d,"end":%d}}}`, base+31000, base+31000),
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
}
