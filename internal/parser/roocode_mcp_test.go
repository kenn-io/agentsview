package parser

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRooCodeSessionMCPResponsePairing(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-mcp-pair")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-mcp-pair",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "MCP pairing test",
		TokensIn:  10,
		TokensOut: 5,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// Sequence: user task, MCP tool call, mcp_server_response
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Search for documentation",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "use_mcp_server",
			Text:      `{"tool":"brave-search","serverName":"brave","query":"React docs"}`,
		},
		{
			Timestamp: 1688836865000,
			Type:      "say",
			Say:       "mcp_server_request_started",
			Text:      "",
		},
		{
			Timestamp: 1688836870000,
			Type:      "say",
			Say:       "mcp_server_response",
			Text:      "Found React documentation at react.dev",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, msgs, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// user task + MCP tool call = 2 (mcp_server_response is paired,
	// mcp_server_request_started is metadata/skipped)
	assert.Equal(t, 2, sess.MessageCount)

	// Verify the MCP tool call has a ResultEvent with the response.
	require.Len(t, msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	assert.Equal(t, "brave-search", tc.ToolName)
	assert.Equal(t, "MCP", tc.Category)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "completed", tc.ResultEvents[0].Status)
	assert.Equal(t, "Found React documentation at react.dev",
		tc.ResultEvents[0].Content)
}

func TestParseRooCodeSessionMCPResponseNoPending(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-mcp-nopending")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-mcp-nopending",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "MCP no pending test",
		TokensIn:  10,
		TokensOut: 5,
		Workspace: "/Users/test/project",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// MCP response with no preceding MCP tool call — should be
	// a standalone system message.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Hello",
		},
		{
			Timestamp: 1688836860000,
			Type:      "say",
			Say:       "mcp_server_response",
			Text:      "Orphaned MCP response",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, msgs, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// user task + MCP response system msg = 2
	assert.Equal(t, 2, sess.MessageCount)

	// Response should be a standalone system message.
	assert.Equal(t, RoleSystem, msgs[1].Role)
	assert.True(t, msgs[1].IsSystem)
	assert.Equal(t, "Orphaned MCP response", msgs[1].Content)
}

func TestParseRooCodeSessionEmptyMCPResponsePairsCompleted(t *testing.T) {
	tmpDir := t.TempDir()
	taskDir := filepath.Join(tmpDir, "tasks", "test-task-mcp-empty")
	require.NoError(t, os.MkdirAll(taskDir, 0755))

	historyItem := rooCodeHistoryItem{
		ID:        "test-task-mcp-empty",
		Number:    1,
		Timestamp: 1688836851000,
		Task:      "Empty MCP response test",
		Workspace: "/Users/test/project",
		Status:    "completed",
	}
	historyJSON, err := json.Marshal(historyItem)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		historyJSON, 0644,
	))

	// An MCP tool whose response body is empty must still resolve
	// the pending use_mcp_server call instead of leaving it pending.
	messages := []rooCodeMessage{
		{
			Timestamp: 1688836851000,
			Type:      "say",
			Say:       "text",
			Text:      "Trigger the webhook",
		},
		{
			Timestamp: 1688836860000,
			Type:      "ask",
			Ask:       "use_mcp_server",
			Text:      `{"tool":"trigger","serverName":"hooks"}`,
		},
		{
			Timestamp: 1688836870000,
			Type:      "say",
			Say:       "mcp_server_response",
			Text:      "",
		},
	}
	messagesJSON, err := json.Marshal(messages)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		messagesJSON, 0644,
	))

	sess, msgs, err := parseRooCodeSession(taskDir, "", "")
	require.NoError(t, err)

	// user task + MCP tool call = 2; no standalone message for the
	// empty response.
	assert.Equal(t, 2, sess.MessageCount)

	require.Len(t, msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "completed", tc.ResultEvents[0].Status)
	assert.Equal(t, "", tc.ResultEvents[0].Content)
	assert.True(t,
		tc.ResultEvents[0].Timestamp.Equal(time.UnixMilli(1688836870000)),
		"result event should carry the mcp_server_response timestamp")

	// The resolved tool call must not read as orphaned.
	assert.Equal(t, TerminationClean, sess.TerminationStatus)
}

func TestRooCodeIsMetadataSayIncludesMCP(t *testing.T) {
	assert.True(t, rooCodeIsMetadataSay("mcp_server_request_started"),
		"mcp_server_request_started should be treated as metadata")
}

func TestParseRooCodeSessionCanonicalMCPPayloads(t *testing.T) {
	tests := []struct {
		name         string
		payload      string
		wantToolName string
	}{
		{
			name: "use_mcp_tool names the invoked tool",
			payload: `{"type":"use_mcp_tool","serverName":"weather",` +
				`"toolName":"get_forecast","arguments":"{\"city\":\"Berlin\"}"}`,
			wantToolName: "get_forecast",
		},
		{
			name: "access_mcp_resource keeps the type as name",
			payload: `{"type":"access_mcp_resource","serverName":"docs",` +
				`"uri":"docs://readme"}`,
			wantToolName: "access_mcp_resource",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			taskDir := filepath.Join(tmpDir, "tasks", "test-task-mcp-canonical")
			require.NoError(t, os.MkdirAll(taskDir, 0755))

			historyItem := rooCodeHistoryItem{
				ID:        "test-task-mcp-canonical",
				Number:    1,
				Timestamp: 1688836851000,
				Task:      tt.name,
				Workspace: "/Users/test/project",
			}
			historyJSON, err := json.Marshal(historyItem)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(
				filepath.Join(taskDir, "history_item.json"),
				historyJSON, 0644,
			))

			// Canonical Roo/Cline use_mcp_server payloads carry a
			// "type" discriminator, not a "tool" field. The call must
			// still be extracted and pair with its response.
			messages := []rooCodeMessage{
				{
					Timestamp: 1688836851000,
					Type:      "say",
					Say:       "text",
					Text:      "Use the MCP server",
				},
				{
					Timestamp: 1688836860000,
					Type:      "ask",
					Ask:       "use_mcp_server",
					Text:      tt.payload,
				},
				{
					Timestamp: 1688836870000,
					Type:      "say",
					Say:       "mcp_server_response",
					Text:      "server says hello",
				},
			}
			messagesJSON, err := json.Marshal(messages)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(
				filepath.Join(taskDir, "ui_messages.json"),
				messagesJSON, 0644,
			))

			sess, msgs, err := parseRooCodeSession(taskDir, "", "")
			require.NoError(t, err)

			// user task + MCP tool call = 2 (response is paired).
			assert.Equal(t, 2, sess.MessageCount)

			require.Len(t, msgs[1].ToolCalls, 1)
			tc := msgs[1].ToolCalls[0]
			assert.Equal(t, tt.wantToolName, tc.ToolName)
			assert.Equal(t, "MCP", tc.Category)
			assert.Contains(t, tc.InputJSON, `"serverName"`)
			require.Len(t, tc.ResultEvents, 1)
			assert.Equal(t, "completed", tc.ResultEvents[0].Status)
			assert.Equal(t, "server says hello", tc.ResultEvents[0].Content)
		})
	}
}
