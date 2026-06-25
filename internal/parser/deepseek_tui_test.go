package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeepSeekTUIProviderDiscoversSessions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "session_b.json"), []byte(`{}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "session_a.json"), []byte(`{}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "latest.json"), []byte(`{}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "offline_queue.json"), []byte(`{}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte(`ignore`), 0o644))
	checkpointDir := filepath.Join(root, "checkpoints")
	require.NoError(t, os.MkdirAll(checkpointDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(checkpointDir, "nested.json"), []byte(`{}`), 0o644))

	provider, ok := NewProvider(AgentDeepSeekTUI, ProviderConfig{
		Roots:   []string{root},
		Machine: "local",
	})
	require.True(t, ok)
	files, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, files, 2)
	assert.Equal(t, filepath.Join(root, "session_a.json"), files[0].DisplayPath)
	assert.Equal(t, AgentDeepSeekTUI, files[0].Provider)
	assert.Equal(t, filepath.Join(root, "session_b.json"), files[1].DisplayPath)
	assert.Equal(t, AgentDeepSeekTUI, files[1].Provider)
}

func TestDeepSeekTUIProviderFindsSourceFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "session_123.json")
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o644))

	provider, ok := NewProvider(AgentDeepSeekTUI, ProviderConfig{
		Roots:   []string{root},
		Machine: "local",
	})
	require.True(t, ok)

	found, ok, err := provider.FindSource(
		context.Background(),
		FindSourceRequest{RawSessionID: "session_123"},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, path, found.DisplayPath)

	_, ok, err = provider.FindSource(
		context.Background(),
		FindSourceRequest{RawSessionID: "missing"},
	)
	require.NoError(t, err)
	assert.False(t, ok)

	_, ok, err = provider.FindSource(
		context.Background(),
		FindSourceRequest{RawSessionID: "../session_123"},
	)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestDeepSeekTUIProviderParsesBasicSession(t *testing.T) {
	t.Parallel()

	content := `{
  "schema_version": 1,
  "metadata": {
    "id": "session_123",
    "title": "Investigate DeepSeek TUI",
    "created_at": "2026-06-01T10:00:00Z",
    "updated_at": "2026-06-01T10:02:00Z",
    "model": "deepseek-chat",
    "workspace": "/Users/alice/code/sample-project",
    "total_tokens": 999
  },
  "messages": [
    {"role": "user", "content": "Inspect server logs", "timestamp": "2026-06-01T10:00:05Z"},
    {"role": "assistant", "content": [{"type": "text", "text": "The server failed during startup."}], "timestamp": "2026-06-01T10:00:10Z"}
  ]
}`
	path := createTestFile(t, "session_123.json", content)

	sess, msgs, err := parseDeepSeekTUITestSession(t, path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 2)

	assert.Equal(t, "deepseek-tui:session_123", sess.ID)
	assert.Equal(t, AgentDeepSeekTUI, sess.Agent)
	assert.Equal(t, "local", sess.Machine)
	assert.Equal(t, "sample_project", sess.Project)
	assert.Equal(t, "/Users/alice/code/sample-project", sess.Cwd)
	assert.Equal(t, "Investigate DeepSeek TUI", sess.SessionName)
	assert.Equal(t, "Inspect server logs", sess.FirstMessage)
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.False(t, sess.HasTotalOutputTokens)
	assert.False(t, sess.HasPeakContextTokens)
	assert.Equal(t, "2026-06-01T10:00:00Z", sess.StartedAt.Format("2006-01-02T15:04:05Z"))
	assert.Equal(t, "2026-06-01T10:02:00Z", sess.EndedAt.Format("2006-01-02T15:04:05Z"))

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Inspect server logs", msgs[0].Content)
	assert.Equal(t, "deepseek-chat", msgs[0].Model)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "The server failed during startup.", msgs[1].Content)
}

func TestDeepSeekTUIProviderParsesToolUseAndThinking(t *testing.T) {
	t.Parallel()

	content := `{
  "metadata": {"id": "session_tools", "workspace": "/repo"},
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "Read the file"}]},
    {"role": "assistant", "content": [
      {"type": "thinking", "thinking": "Need to inspect the target."},
      {"type": "tool_use", "id": "toolu_1", "name": "Read", "input": {"file_path": "main.go"}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_1", "content": "package main"}
    ]},
    {"role": "assistant", "content": [{"type": "text", "text": "It is a Go file."}]}
  ]
}`
	path := createTestFile(t, "session_tools.json", content)

	sess, msgs, err := parseDeepSeekTUITestSession(t, path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 4)

	assert.True(t, msgs[1].HasThinking)
	assert.Equal(t, "Need to inspect the target.", msgs[1].ThinkingText)
	assert.Contains(t, msgs[1].Content, "[Thinking]")
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "toolu_1", msgs[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].Category)
	assert.JSONEq(t, `{"file_path":"main.go"}`, msgs[1].ToolCalls[0].InputJSON)

	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "toolu_1", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal(t, len("package main"), msgs[2].ToolResults[0].ContentLength)
	assert.Equal(t, "package main", DecodeContent(msgs[2].ToolResults[0].ContentRaw))
}

func TestDeepSeekTUIProviderParsesObjectToolResult(t *testing.T) {
	t.Parallel()

	content := `{
  "metadata": {"id": "session_obj", "workspace": "/repo"},
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "Run it"}]},
    {"role": "assistant", "content": [
      {"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": {"command": "ls"}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_1", "content": {"output": "file1.go\nfile2.go"}}
    ]}
  ]
}`
	path := createTestFile(t, "session_obj.json", content)

	_, msgs, err := parseDeepSeekTUITestSession(t, path, "local")
	require.NoError(t, err)
	require.Len(t, msgs, 3)

	require.Len(t, msgs[2].ToolResults, 1)
	result := msgs[2].ToolResults[0]
	assert.Equal(t, len("file1.go\nfile2.go"), result.ContentLength)
	assert.Equal(t, "file1.go\nfile2.go", DecodeContent(result.ContentRaw))
}

func TestDeepSeekTUIProviderParsesEmptyObjectToolResult(t *testing.T) {
	t.Parallel()

	content := `{
  "metadata": {"id": "session_empty_obj", "workspace": "/repo"},
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "Run it"}]},
    {"role": "assistant", "content": [
      {"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": {"command": "true"}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_1", "content": {"output": ""}}
    ]}
  ]
}`
	path := createTestFile(t, "session_empty_obj.json", content)

	_, msgs, err := parseDeepSeekTUITestSession(t, path, "local")
	require.NoError(t, err)
	require.Len(t, msgs, 3)

	require.Len(t, msgs[2].ToolResults, 1)
	result := msgs[2].ToolResults[0]
	assert.Equal(t, 0, result.ContentLength)
	assert.Empty(t, DecodeContent(result.ContentRaw))
}

func TestDeepSeekTUIProviderSkipsEmptySession(t *testing.T) {
	t.Parallel()

	path := createTestFile(t, "empty_session.json", `{
  "metadata": {"id": "empty_session"},
  "messages": []
}`)

	sess, msgs, err := parseDeepSeekTUITestSession(t, path, "local")
	require.NoError(t, err)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func parseDeepSeekTUITestSession(
	t *testing.T,
	path string,
	machine string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()

	provider, ok := NewProvider(AgentDeepSeekTUI, ProviderConfig{
		Roots:   []string{filepath.Dir(path)},
		Machine: machine,
	})
	require.True(t, ok)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: SourceRef{
			Provider:       AgentDeepSeekTUI,
			Key:            path,
			DisplayPath:    path,
			FingerprintKey: path,
		},
		Machine: machine,
	})
	if err != nil || len(outcome.Results) == 0 {
		return nil, nil, err
	}
	result := outcome.Results[0].Result
	return &result.Session, result.Messages, nil
}
