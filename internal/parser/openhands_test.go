package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverAndFindOpenHandsSessions(t *testing.T) {
	root := t.TempDir()
	sessionID := "086c7ecf-6cb7-46b6-9fbc-b900358d1247"
	dirName := "086c7ecf6cb746b69fbcb900358d1247"
	sessionDir := filepath.Join(root, dirName)

	require.NoError(t, os.MkdirAll(
		filepath.Join(sessionDir, "events"), 0o755,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "base_state.json"),
		[]byte(`{"id":"`+sessionID+`"}`),
		0o644,
	))

	files := DiscoverOpenHandsSessions(root)
	require.Len(t, files, 1)
	assert.Equal(t, sessionDir, files[0].Path)
	assert.Equal(t, AgentOpenHands, files[0].Agent)

	assert.Equal(
		t, sessionDir,
		FindOpenHandsSourceFile(root, sessionID),
	)
	assert.Equal(
		t, sessionDir,
		FindOpenHandsSourceFile(root, dirName),
	)
}

func TestParseOpenHandsSession(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "demo-repo")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	sessionID := "086c7ecf-6cb7-46b6-9fbc-b900358d1247"
	sessionDir := filepath.Join(
		root, "086c7ecf6cb746b69fbcb900358d1247",
	)
	eventsDir := filepath.Join(sessionDir, "events")
	require.NoError(t, os.MkdirAll(eventsDir, 0o755))

	baseState := `{
		"id":"` + sessionID + `",
		"agent":{"llm":{"model":"litellm_proxy/claude-sonnet-4-6"}}
	}`
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "base_state.json"),
		[]byte(baseState), 0o644,
	))

	projectDirJSON, err := json.Marshal(projectDir)
	require.NoError(t, err)

	events := map[string]string{
		"event-00000-user.json": `{
			"id":"e0",
			"timestamp":"2026-04-02T15:25:40.706887",
			"source":"user",
			"llm_message":{"role":"user","content":[{"type":"text","text":"Help me debug the server"}]},
			"kind":"MessageEvent"
		}`,
		"event-00001-action.json": `{
			"id":"e1",
			"timestamp":"2026-04-02T15:25:41.706887",
			"source":"agent",
			"thought":[{"type":"text","text":"I'll inspect the logs first."}],
			"thinking_blocks":[{"type":"thinking","thinking":"Start with the failing process and collect output."}],
			"action":{"command":"tail -40 /tmp/server.log","kind":"TerminalAction"},
			"tool_name":"terminal",
			"tool_call_id":"toolu_123",
			"tool_call":{"id":"toolu_123","name":"terminal","arguments":"{\"command\":\"tail -40 /tmp/server.log\",\"summary\":\"Inspect latest server logs\"}"},
			"summary":"Inspect latest server logs",
			"kind":"ActionEvent"
		}`,
		"event-00002-observation.json": `{
			"id":"e2",
			"timestamp":"2026-04-02T15:25:42.706887",
			"source":"environment",
			"tool_name":"terminal",
			"tool_call_id":"toolu_123",
			"observation":{
				"content":[{"type":"text","text":"panic: boom"}],
				"is_error":false,
				"metadata":{"working_dir":` + string(projectDirJSON) + `},
				"kind":"TerminalObservation"
			},
			"action_id":"e1",
			"kind":"ObservationEvent"
		}`,
		"event-00003-assistant.json": `{
			"id":"e3",
			"timestamp":"2026-04-02T15:25:43.706887",
			"source":"agent",
			"llm_message":{"role":"assistant","content":[{"type":"text","text":"The panic happens during startup."}]},
			"thinking_blocks":[{"type":"thinking","thinking":"The stack trace indicates a nil config path."}],
			"kind":"MessageEvent"
		}`,
	}
	for name, content := range events {
		require.NoError(t, os.WriteFile(
			filepath.Join(eventsDir, name),
			[]byte(content), 0o644,
		))
	}

	sess, msgs, err := ParseOpenHandsSession(
		sessionDir, "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 4)

	assert.Equal(t, "openhands:"+sessionID, sess.ID)
	assert.Equal(t, AgentOpenHands, sess.Agent)
	assert.Equal(t, "demo_repo", sess.Project)
	assert.Equal(t, projectDir, sess.Cwd)
	assert.Equal(t, "Help me debug the server", sess.FirstMessage)
	assert.Equal(t, 4, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, sessionDir, sess.File.Path)
	assert.NotEmpty(t, sess.File.Hash)
	assert.NotZero(t, sess.File.Mtime)
	assert.Greater(t, sess.File.Size, int64(0))

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Help me debug the server", msgs[0].Content)

	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasThinking)
	assert.True(t, msgs[1].HasToolUse)
	assert.Equal(t, "litellm_proxy/claude-sonnet-4-6", msgs[1].Model)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "terminal", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Bash", msgs[1].ToolCalls[0].Category)
	assert.Equal(t, "toolu_123", msgs[1].ToolCalls[0].ToolUseID)
	assert.Contains(t, msgs[1].Content, "[Bash: Inspect latest server logs]")

	assert.Equal(t, RoleUser, msgs[2].Role)
	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "toolu_123", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal(
		t, "panic: boom",
		DecodeContent(msgs[2].ToolResults[0].ContentRaw),
	)

	assert.Equal(t, RoleAssistant, msgs[3].Role)
	assert.True(t, msgs[3].HasThinking)
	assert.Equal(t, "litellm_proxy/claude-sonnet-4-6", msgs[3].Model)
	assert.Contains(t, msgs[3].Content, "The panic happens during startup.")
}
