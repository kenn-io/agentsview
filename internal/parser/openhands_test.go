package parser

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDiscoverAndFindOpenHandsSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessionID := "086c7ecf-6cb7-46b6-9fbc-b900358d1247"
	dirName := "086c7ecf6cb746b69fbcb900358d1247"
	sessionDir := filepath.Join(root, dirName)

	require.NoError(os.MkdirAll(
		filepath.Join(sessionDir, "events"), 0o755,
	))
	require.NoError(os.WriteFile(
		filepath.Join(sessionDir, "base_state.json"),
		[]byte(`{"id":"`+sessionID+`"}`),
		0o644,
	))

	provider, ok := NewProvider(AgentOpenHands, ProviderConfig{
		Roots: []string{root},
	})
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(sessionDir, sources[0].DisplayPath)
	assert.Equal(AgentOpenHands, sources[0].Provider)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: sessionID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sessionDir, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: dirName,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sessionDir, found.DisplayPath)
}

func TestParseOpenHandsSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	projectDir := filepath.Join(root, "demo-repo")
	require.NoError(os.MkdirAll(projectDir, 0o755))

	sessionID := "086c7ecf-6cb7-46b6-9fbc-b900358d1247"
	sessionDir := filepath.Join(
		root, "086c7ecf6cb746b69fbcb900358d1247",
	)
	eventsDir := filepath.Join(sessionDir, "events")
	require.NoError(os.MkdirAll(eventsDir, 0o755))

	baseState := `{
		"id":"` + sessionID + `",
		"agent":{"llm":{"model":"litellm_proxy/claude-sonnet-4-6"}}
	}`
	require.NoError(os.WriteFile(
		filepath.Join(sessionDir, "base_state.json"),
		[]byte(baseState), 0o644,
	))

	projectDirJSON, err := json.Marshal(projectDir)
	require.NoError(err)

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
		require.NoError(os.WriteFile(
			filepath.Join(eventsDir, name),
			[]byte(content), 0o644,
		))
	}

	provider, ok := NewProvider(AgentOpenHands, ProviderConfig{
		Roots:   []string{root},
		Machine: "local",
	})
	require.True(ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: sessionDir,
	})
	require.NoError(err)
	require.True(found)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)

	sess := &outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages
	require.NotNil(sess)
	require.Len(msgs, 4)

	assert.Equal("openhands:"+sessionID, sess.ID)
	assert.Equal(AgentOpenHands, sess.Agent)
	assert.Equal("demo_repo", sess.Project)
	assert.Equal(projectDir, sess.Cwd)
	assert.Equal("Help me debug the server", sess.FirstMessage)
	assert.Equal(4, sess.MessageCount)
	assert.Equal(1, sess.UserMessageCount)
	assert.Equal(sessionDir, sess.File.Path)
	assert.NotEmpty(sess.File.Hash)
	assert.NotZero(sess.File.Mtime)
	assert.Greater(sess.File.Size, int64(0))

	assert.Equal(RoleUser, msgs[0].Role)
	assert.Equal("Help me debug the server", msgs[0].Content)

	assert.Equal(RoleAssistant, msgs[1].Role)
	assert.True(msgs[1].HasThinking)
	assert.True(msgs[1].HasToolUse)
	assert.Equal("litellm_proxy/claude-sonnet-4-6", msgs[1].Model)
	require.Len(msgs[1].ToolCalls, 1)
	assert.Equal("terminal", msgs[1].ToolCalls[0].ToolName)
	assert.Equal("Bash", msgs[1].ToolCalls[0].Category)
	assert.Equal("toolu_123", msgs[1].ToolCalls[0].ToolUseID)
	assert.Contains(msgs[1].Content, "[Bash: Inspect latest server logs]")

	assert.Equal(RoleUser, msgs[2].Role)
	require.Len(msgs[2].ToolResults, 1)
	assert.Equal("toolu_123", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal(
		"panic: boom",
		DecodeContent(msgs[2].ToolResults[0].ContentRaw),
	)

	assert.Equal(RoleAssistant, msgs[3].Role)
	assert.True(msgs[3].HasThinking)
	assert.Equal("litellm_proxy/claude-sonnet-4-6", msgs[3].Model)
	assert.Contains(msgs[3].Content, "The panic happens during startup.")
}

func TestParseOpenHandsObservationWithoutToolCallIsMarkedToolOutput(t *testing.T) {
	assert := assert.New(t)

	event := gjson.Parse(`{
		"id":"e9",
		"source":"environment",
		"observation":{
			"content":[{"type":"text","text":"token=abc123"}],
			"kind":"TerminalObservation"
		},
		"kind":"ObservationEvent"
	}`)

	msg, ok, _ := parseOpenHandsObservationEvent(event, 3, time.Time{})

	require.True(t, ok)
	assert.Equal(RoleUser, msg.Role)
	assert.Equal("token=abc123", msg.Content)
	assert.Equal(SourceSubtypeToolResult, msg.SourceSubtype,
		"an observation with no tool call to pair with is still tool output")
}
