package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeKimiWireJSONL(
	t *testing.T, projHash, sessionUUID string,
	lines []string,
) string {
	t.Helper()
	dir := filepath.Join(
		t.TempDir(), projHash, sessionUUID,
	)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "wire.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	require.NoError(t,
		os.WriteFile(path, []byte(content), 0o644))
	return path
}

func writeKimiCodeWireJSONL(
	t *testing.T, workdirDir, sessionDir, agentID string,
	lines []string,
) string {
	t.Helper()
	dir := filepath.Join(
		t.TempDir(), workdirDir, sessionDir, "agents", agentID,
	)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "wire.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	require.NoError(t,
		os.WriteFile(path, []byte(content), 0o644))
	return path
}

func parseKimiSessionForTest(
	t *testing.T,
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()
	return parseKimiSession(path, project, machine)
}

func TestParseKimiSession_Basic(t *testing.T) {
	path := writeKimiWireJSONL(t,
		"abc123", "sess-uuid-1234",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Hello Kimi"}]}}}`,
			`{"timestamp": 1704067201.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Hi there!"}}}`,
			`{"timestamp": 1704067202.0, "message": {"type": "TurnEnd", "payload": {}}}`,
		},
	)

	sess, msgs, err := parseKimiSessionForTest(t,
		path, "myproject", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertSessionMeta(t, sess,
		"kimi:abc123:sess-uuid-1234",
		"myproject", AgentKimi,
	)
	assert.Equal(t, "Hello Kimi", sess.FirstMessage)
	assertMessageCount(t, sess.MessageCount, 2)
	assert.Equal(t, 1, sess.UserMessageCount)

	wantStart := time.Unix(1704067200, 0)
	assertTimestamp(t, sess.StartedAt, wantStart)
	wantEnd := time.Unix(1704067202, 0)
	assertTimestamp(t, sess.EndedAt, wantEnd)

	require.Equal(t, 2, len(msgs))
	assertMessage(t, msgs[0], RoleUser, "Hello Kimi")
	assertMessage(t, msgs[1], RoleAssistant, "Hi there!")
	assert.Equal(t, 0, msgs[0].Ordinal)
	assert.Equal(t, 1, msgs[1].Ordinal)
}

func TestParseKimiSession_ThinkingAndToolUse(t *testing.T) {
	path := writeKimiWireJSONL(t,
		"proj1", "sess1",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Read the file"}]}}}`,
			`{"timestamp": 1704067201.0, "message": {"type": "ContentPart", "payload": {"type": "think", "think": "Let me plan.", "encrypted": null}}}`,
			`{"timestamp": 1704067202.0, "message": {"type": "ToolCall", "payload": {"type": "function", "id": "tool_1", "function": {"name": "Glob", "arguments": "{\"pattern\": \"*.go\"}"}, "extras": null}}}`,
			`{"timestamp": 1704067203.0, "message": {"type": "ToolResult", "payload": {"tool_call_id": "tool_1", "return_value": {"is_error": false, "output": "main.go\nutil.go"}}}}`,
			`{"timestamp": 1704067204.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Found the files."}}}`,
			`{"timestamp": 1704067205.0, "message": {"type": "TurnEnd", "payload": {}}}`,
		},
	)

	sess, msgs, err := parseKimiSessionForTest(t,
		path, "testproj", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "Read the file", sess.FirstMessage)

	// user, assistant(thinking+tool), tool_result(user), assistant(text)
	require.Equal(t, 4, len(msgs))

	// First message: user
	assert.Equal(t, RoleUser, msgs[0].Role)

	// Second: assistant with thinking + tool call
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasThinking)
	assert.True(t, msgs[1].HasToolUse)
	assert.Contains(t, msgs[1].Content, "[Thinking]")
	assert.Contains(t, msgs[1].Content, "Let me plan.")
	assert.Contains(t, msgs[1].Content, "[Glob:")
	require.Equal(t, 1, len(msgs[1].ToolCalls))
	assert.Equal(t, "Glob", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Glob", msgs[1].ToolCalls[0].Category)
	assert.Equal(t, "tool_1", msgs[1].ToolCalls[0].ToolUseID)

	// Third: tool result (user role)
	assert.Equal(t, RoleUser, msgs[2].Role)
	require.Equal(t, 1, len(msgs[2].ToolResults))
	assert.Equal(t, "tool_1", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal(t, "main.go\nutil.go",
		DecodeContent(msgs[2].ToolResults[0].ContentRaw))

	// Fourth: assistant text continuation
	assert.Equal(t, RoleAssistant, msgs[3].Role)
	assert.Contains(t, msgs[3].Content, "Found the files.")
}

func TestParseKimiSession_Empty(t *testing.T) {
	path := writeKimiWireJSONL(t,
		"proj2", "sess2",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
		},
	)

	sess, msgs, err := parseKimiSessionForTest(t,
		path, "testproj", "local",
	)
	require.NoError(t, err)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func TestParseKimiSession_ErrorToolResult(t *testing.T) {
	path := writeKimiWireJSONL(t,
		"proj3", "sess3",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Do something"}]}}}`,
			`{"timestamp": 1704067201.0, "message": {"type": "ToolCall", "payload": {"type": "function", "id": "tool_err", "function": {"name": "Bash", "arguments": "{\"command\": \"exit 1\"}"}, "extras": null}}}`,
			`{"timestamp": 1704067202.0, "message": {"type": "ToolResult", "payload": {"tool_call_id": "tool_err", "return_value": {"is_error": true, "output": ""}}}}`,
			`{"timestamp": 1704067203.0, "message": {"type": "TurnEnd", "payload": {}}}`,
		},
	)

	sess, msgs, err := parseKimiSessionForTest(t,
		path, "testproj", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// user, assistant(tool call), tool_result(error)
	require.Equal(t, 3, len(msgs))
	assert.Equal(t, "[error]",
		DecodeContent(msgs[2].ToolResults[0].ContentRaw))
}

func TestParseKimiSession_ArrayToolResult(t *testing.T) {
	path := writeKimiWireJSONL(t,
		"proj-arr", "sess-arr",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Query logs"}]}}}`,
			`{"timestamp": 1704067201.0, "message": {"type": "ToolCall", "payload": {"type": "function", "id": "tool_arr", "function": {"name": "Bash", "arguments": "{\"command\": \"echo hi\"}"}, "extras": null}}}`,
			`{"timestamp": 1704067202.0, "message": {"type": "ToolResult", "payload": {"tool_call_id": "tool_arr", "return_value": {"is_error": false, "output": [{"type": "text", "text": "line one"}, {"type": "text", "text": "line two"}]}}}}`,
			`{"timestamp": 1704067203.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Done."}}}`,
			`{"timestamp": 1704067204.0, "message": {"type": "TurnEnd", "payload": {}}}`,
		},
	)

	sess, msgs, err := parseKimiSessionForTest(t,
		path, "testproj", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// user, assistant(tool call), tool_result(array output), assistant(text)
	require.Equal(t, 4, len(msgs))
	require.Equal(t, 1, len(msgs[2].ToolResults))
	assert.Equal(t, "line one\nline two",
		DecodeContent(msgs[2].ToolResults[0].ContentRaw))
	assert.Equal(t, len("line one\nline two"),
		msgs[2].ToolResults[0].ContentLength)
}

func TestParseKimiSession_MultipleStatusUpdates(t *testing.T) {
	path := writeKimiWireJSONL(t,
		"proj-multi", "sess-multi",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Hello"}]}}}`,
			`{"timestamp": 1704067201.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Hi"}}}`,
			`{"timestamp": 1704067201.5, "message": {"type": "StatusUpdate", "payload": {"context_tokens": 5000, "token_usage": {"output": 100}}}}`,
			`{"timestamp": 1704067202.0, "message": {"type": "ToolCall", "payload": {"type": "function", "id": "t1", "function": {"name": "Glob", "arguments": "{}"}, "extras": null}}}`,
			`{"timestamp": 1704067202.5, "message": {"type": "StatusUpdate", "payload": {"context_tokens": 8000, "token_usage": {"output": 50}}}}`,
			`{"timestamp": 1704067203.0, "message": {"type": "ToolResult", "payload": {"tool_call_id": "t1", "return_value": {"is_error": false, "output": "a.go"}}}}`,
			`{"timestamp": 1704067203.5, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Found it."}}}`,
			`{"timestamp": 1704067204.0, "message": {"type": "StatusUpdate", "payload": {"context_tokens": 6000, "token_usage": {"output": 75}}}}`,
			`{"timestamp": 1704067205.0, "message": {"type": "TurnEnd", "payload": {}}}`,
		},
	)

	sess, _, err := parseKimiSessionForTest(t,
		path, "testproj", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 225, sess.TotalOutputTokens)
	assert.Equal(t, 8000, sess.PeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
}

func TestParseKimiSession_StatusUpdate(t *testing.T) {
	path := writeKimiWireJSONL(t,
		"proj4", "sess4",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Hello"}]}}}`,
			`{"timestamp": 1704067201.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Hi"}}}`,
			`{"timestamp": 1704067201.5, "message": {"type": "StatusUpdate", "payload": {"context_tokens": 5000, "max_context_tokens": 262144, "token_usage": {"output": 42}}}}`,
			`{"timestamp": 1704067202.0, "message": {"type": "TurnEnd", "payload": {}}}`,
		},
	)

	sess, _, err := parseKimiSessionForTest(t,
		path, "testproj", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 42, sess.TotalOutputTokens)
	assert.Equal(t, 5000, sess.PeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
}

func TestParseKimiSession_ZeroValuedStatusUpdatePreservesCoverage(t *testing.T) {
	path := writeKimiWireJSONL(t,
		"proj-zero", "sess-zero",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Hello"}]}}}`,
			`{"timestamp": 1704067201.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Hi"}}}`,
			`{"timestamp": 1704067201.5, "message": {"type": "StatusUpdate", "payload": {"context_tokens": 0, "token_usage": {"output": 0}}}}`,
			`{"timestamp": 1704067202.0, "message": {"type": "TurnEnd", "payload": {}}}`,
		},
	)

	sess, _, err := parseKimiSessionForTest(t,
		path, "testproj", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 0, sess.TotalOutputTokens)
	assert.Equal(t, 0, sess.PeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
}

func TestParseKimiSession_NoProject(t *testing.T) {
	path := writeKimiWireJSONL(t,
		"proj5", "sess5",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Hello"}]}}}`,
			`{"timestamp": 1704067201.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Hi"}}}`,
			`{"timestamp": 1704067202.0, "message": {"type": "TurnEnd", "payload": {}}}`,
		},
	)

	sess, _, err := parseKimiSessionForTest(t, path, "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "kimi", sess.Project)
}

func TestParseKimiSession_MessageTimestamps(t *testing.T) {
	path := writeKimiWireJSONL(t,
		"proj-ts", "sess-ts",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Hello"}]}}}`,
			`{"timestamp": 1704067201.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Hi there!"}}}`,
			`{"timestamp": 1704067202.0, "message": {"type": "ToolCall", "payload": {"type": "function", "id": "t1", "function": {"name": "Bash", "arguments": "{\"command\": \"ls\"}"}, "extras": null}}}`,
			`{"timestamp": 1704067203.0, "message": {"type": "ToolResult", "payload": {"tool_call_id": "t1", "return_value": {"is_error": false, "output": "file.go"}}}}`,
			`{"timestamp": 1704067204.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Done."}}}`,
			`{"timestamp": 1704067205.0, "message": {"type": "TurnEnd", "payload": {}}}`,
		},
	)

	_, msgs, err := parseKimiSessionForTest(t,
		path, "testproj", "local",
	)
	require.NoError(t, err)
	require.Equal(t, 4, len(msgs))

	// User message gets timestamp from TurnBegin record.
	assertTimestamp(t, msgs[0].Timestamp,
		time.Unix(1704067200, 0))

	// Assistant (flushed by ToolResult) gets first content
	// timestamp (ContentPart at :01, not ToolCall at :02).
	assertTimestamp(t, msgs[1].Timestamp,
		time.Unix(1704067201, 0))

	// Tool result gets timestamp from ToolResult record.
	assertTimestamp(t, msgs[2].Timestamp,
		time.Unix(1704067203, 0))

	// Final assistant gets timestamp from its ContentPart.
	assertTimestamp(t, msgs[3].Timestamp,
		time.Unix(1704067204, 0))
}

func TestParseKimiSession_EmptyFragmentTimestamp(t *testing.T) {
	t.Run("cross-turn", func(t *testing.T) {
		// Empty ContentPart followed by TurnEnd, then a real
		// turn. The second turn must use its own timestamp.
		path := writeKimiWireJSONL(t,
			"proj-empty", "sess-empty",
			[]string{
				`{"type": "metadata", "protocol_version": "1.3"}`,
				`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Hello"}]}}}`,
				`{"timestamp": 1704067201.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": ""}}}`,
				`{"timestamp": 1704067202.0, "message": {"type": "TurnEnd", "payload": {}}}`,
				`{"timestamp": 1704067210.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Again"}]}}}`,
				`{"timestamp": 1704067211.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Hi!"}}}`,
				`{"timestamp": 1704067212.0, "message": {"type": "TurnEnd", "payload": {}}}`,
			},
		)

		_, msgs, err := parseKimiSessionForTest(t,
			path, "testproj", "local",
		)
		require.NoError(t, err)
		require.Equal(t, 3, len(msgs))
		assertTimestamp(t, msgs[2].Timestamp,
			time.Unix(1704067211, 0))
	})

	t.Run("intra-turn", func(t *testing.T) {
		// Empty fragment then real content in the same turn.
		// Timestamp must reflect the real content, not the
		// empty fragment.
		path := writeKimiWireJSONL(t,
			"proj-intra", "sess-intra",
			[]string{
				`{"type": "metadata", "protocol_version": "1.3"}`,
				`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Hello"}]}}}`,
				`{"timestamp": 1704067201.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": ""}}}`,
				`{"timestamp": 1704067205.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Real content"}}}`,
				`{"timestamp": 1704067206.0, "message": {"type": "TurnEnd", "payload": {}}}`,
			},
		)

		_, msgs, err := parseKimiSessionForTest(t,
			path, "testproj", "local",
		)
		require.NoError(t, err)
		require.Equal(t, 2, len(msgs))
		assertTimestamp(t, msgs[1].Timestamp,
			time.Unix(1704067205, 0))
	})
}

func TestParseKimiSession_MissingFile(t *testing.T) {
	_, _, err := parseKimiSessionForTest(t,
		"/nonexistent/wire.jsonl", "proj", "local",
	)
	assert.Error(t, err)
}

func TestParseKimiSession_FirstMessageTruncation(t *testing.T) {
	longText := strings.Repeat("a", 400)
	path := writeKimiWireJSONL(t,
		"proj6", "sess6",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "` + longText + `"}]}}}`,
			`{"timestamp": 1704067201.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Ok"}}}`,
			`{"timestamp": 1704067202.0, "message": {"type": "TurnEnd", "payload": {}}}`,
		},
	)

	sess, _, err := parseKimiSessionForTest(t,
		path, "testproj", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 303, len(sess.FirstMessage))
}

func TestDiscoverKimiSessions(t *testing.T) {
	dir := t.TempDir()

	projDir := filepath.Join(dir, "abc123")
	sessDir := filepath.Join(projDir, "uuid-1")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessDir, "wire.jsonl"),
		[]byte(`{"type":"metadata"}`+"\n"), 0o644,
	))

	sessDir2 := filepath.Join(projDir, "uuid-2")
	require.NoError(t, os.MkdirAll(sessDir2, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessDir2, "wire.jsonl"),
		[]byte(`{"type":"metadata"}`+"\n"), 0o644,
	))

	provider, ok := NewProvider(AgentKimi, ProviderConfig{Roots: []string{dir}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.Equal(t, []string{
		filepath.Join(sessDir, "wire.jsonl"),
		filepath.Join(sessDir2, "wire.jsonl"),
	}, sourceDisplayPaths(sources))
	assert.Equal(t, []string{"abc123", "abc123"}, sourceProjects(sources))
}

func TestDiscoverKimiSessions_Empty(t *testing.T) {
	provider, ok := NewProvider(AgentKimi, ProviderConfig{
		Roots: []string{"", "/nonexistent"},
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func TestFindKimiSourceFile(t *testing.T) {
	dir := t.TempDir()

	projDir := filepath.Join(dir, "abc123")
	sessDir := filepath.Join(projDir, "uuid-1")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	wirePath := filepath.Join(sessDir, "wire.jsonl")
	require.NoError(t, os.WriteFile(
		wirePath, []byte("{}"), 0o644,
	))

	provider, ok := NewProvider(AgentKimi, ProviderConfig{Roots: []string{dir}})
	require.True(t, ok)
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "abc123:uuid-1",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, wirePath, found.DisplayPath)

	for _, rawID := range []string{"abc123:nonexistent", "invalid"} {
		_, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
			RawSessionID: rawID,
		})
		require.NoError(t, err)
		assert.False(t, ok)
	}
	emptyProvider, ok := NewProvider(AgentKimi, ProviderConfig{})
	require.True(t, ok)
	_, ok, err = emptyProvider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "abc123:uuid-1",
	})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestDiscoverKimiSessions_NewLayout(t *testing.T) {
	dir := t.TempDir()

	workdirDir := "wd_claude-code_5534d269834e"
	sessionDir := "session_2728744d-1865-4af1-b3da-97d5bf22a979"
	sessDir := filepath.Join(dir, workdirDir, sessionDir, "agents", "main")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	wirePath := filepath.Join(sessDir, "wire.jsonl")
	require.NoError(t, os.WriteFile(
		wirePath, []byte(`{"type":"metadata"}`+"\n"), 0o644,
	))

	provider, ok := NewProvider(AgentKimi, ProviderConfig{Roots: []string{dir}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, wirePath, sources[0].DisplayPath)
	// Project is decoded from "wd_<workdir>_<hash>".
	assert.Equal(t, "claude-code", sources[0].ProjectHint)
}

func TestDiscoverKimiSessions_NewLayout_NonMainAgent(t *testing.T) {
	dir := t.TempDir()

	workdirDir := "wd_kimi-code_6dc514e1caf6"
	sessionDir := "session_c0517a58-48ee-4632-a1fd-08be3f4f9b0f"
	sessDir := filepath.Join(dir, workdirDir, sessionDir, "agents", "agent-0")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	wirePath := filepath.Join(sessDir, "wire.jsonl")
	require.NoError(t, os.WriteFile(
		wirePath, []byte(`{"type":"metadata"}`+"\n"), 0o644,
	))

	provider, ok := NewProvider(AgentKimi, ProviderConfig{Roots: []string{dir}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, wirePath, sources[0].DisplayPath)
}

func TestFindKimiSourceFile_NewLayout(t *testing.T) {
	dir := t.TempDir()

	workdirDir := "wd_pycharmprojects_a51d6966b209"
	sessionDir := "session_07673173-caad-4ad9-b8b8-29cb8fdaf66b"
	sessDir := filepath.Join(dir, workdirDir, sessionDir, "agents", "main")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	wirePath := filepath.Join(sessDir, "wire.jsonl")
	require.NoError(t, os.WriteFile(
		wirePath, []byte("{}"), 0o644,
	))

	provider, ok := NewProvider(AgentKimi, ProviderConfig{Roots: []string{dir}})
	require.True(t, ok)
	rawID := workdirDir + ":main:" + sessionDir
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: rawID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, wirePath, found.DisplayPath)

	for _, rawID := range []string{
		workdirDir + ":main:nonexistent",
		workdirDir + ":" + sessionDir,
	} {
		_, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
			RawSessionID: rawID,
		})
		require.NoError(t, err)
		assert.False(t, ok)
	}
}

func TestParseKimiSession_NewLayoutSessionID(t *testing.T) {
	path := writeKimiCodeWireJSONL(t,
		"wd_myproject_a1b2c3d4", "session_uuid-1234", "main",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Hello Kimi Code"}]}}}`,
			`{"timestamp": 1704067201.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Hi there!"}}}`,
			`{"timestamp": 1704067202.0, "message": {"type": "TurnEnd", "payload": {}}}`,
		},
	)

	sess, msgs, err := parseKimiSessionForTest(t, path, "myproject", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertSessionMeta(t, sess,
		"kimi:wd_myproject_a1b2c3d4:main:session_uuid-1234",
		"myproject", AgentKimi,
	)
	assert.Equal(t, "Hello Kimi Code", sess.FirstMessage)
	assertMessageCount(t, sess.MessageCount, 2)
	require.Equal(t, 2, len(msgs))
	assertMessage(t, msgs[0], RoleUser, "Hello Kimi Code")
	assertMessage(t, msgs[1], RoleAssistant, "Hi there!")
}

func TestParseKimiSession_NativeKimiCodeEvents(t *testing.T) {
	path := writeKimiCodeWireJSONL(t,
		"wd_myproject_a1b2c3d4", "session_uuid-1234", "main",
		[]string{
			`{"type":"metadata","protocol_version":"1.4","created_at":1782012661212}`,
			`{"type":"config.update","modelAlias":"kimi-code/kimi-for-coding","thinkingLevel":"high","time":1782012661213}`,
			`{"type":"turn.prompt","input":[{"type":"text","text":"hello"}],"origin":{"kind":"user"},"time":1782012666987}`,
			`{"type":"context.append_message","message":{"role":"user","content":[{"type":"text","text":"hello"}],"toolCalls":[],"origin":{"kind":"user"}},"time":1782012666987}`,
			`{"type":"context.append_loop_event","event":{"type":"step.begin","uuid":"667ba233-c6d8-4939-b777-a5035ecc4215","turnId":"0","step":1},"time":1782012666989}`,
			`{"type":"context.append_loop_event","event":{"type":"content.part","uuid":"1160b8b3-c0a6-436f-91db-ef737cd736cd","turnId":"0","step":1,"stepUuid":"667ba233-c6d8-4939-b777-a5035ecc4215","part":{"type":"think","think":"The user said hello. This is a simple greeting."}},"time":1782012668557}`,
			`{"type":"context.append_loop_event","event":{"type":"content.part","uuid":"c379acc8-aaad-41bb-af8a-b478829e38f7","turnId":"0","step":1,"stepUuid":"667ba233-c6d8-4939-b777-a5035ecc4215","part":{"type":"text","text":"Hello! How can I help you today?"}},"time":1782012668558}`,
			`{"type":"context.append_loop_event","event":{"type":"step.end","uuid":"667ba233-c6d8-4939-b777-a5035ecc4215","turnId":"0","step":1,"usage":{"inputOther":1598,"output":37,"inputCacheRead":14592,"inputCacheCreation":0},"finishReason":"end_turn","llmFirstTokenLatencyMs":1169,"llmStreamDurationMs":396},"time":1782012668558}`,
			`{"type":"usage.record","model":"kimi-code/kimi-for-coding","usage":{"inputOther":1598,"output":37,"inputCacheRead":14592,"inputCacheCreation":0},"usageScope":"turn","time":1782012668558}`,
		},
	)

	sess, msgs, err := parseKimiSessionForTest(t, path, "myproject", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertSessionMeta(t, sess,
		"kimi:wd_myproject_a1b2c3d4:main:session_uuid-1234",
		"myproject", AgentKimi,
	)
	assert.Equal(t, "hello", sess.FirstMessage)
	assertMessageCount(t, sess.MessageCount, 2)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, 37, sess.TotalOutputTokens)
	assert.Equal(t, 16190, sess.PeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)

	require.Len(t, msgs, 2)
	assertMessage(t, msgs[0], RoleUser, "hello")
	assertTimestamp(t, msgs[0].Timestamp,
		time.UnixMilli(1782012666987))

	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasThinking)
	assert.Contains(t, msgs[1].Content, "[Thinking]")
	assert.Contains(t, msgs[1].Content, "simple greeting")
	assert.Contains(t, msgs[1].Content,
		"Hello! How can I help you today?")
	assert.Equal(t, "kimi-code/kimi-for-coding", msgs[1].Model)
	assert.Equal(t, "end_turn", msgs[1].StopReason)
	assert.True(t, msgs[1].HasOutputTokens)
	assert.True(t, msgs[1].HasContextTokens)
	assert.Equal(t, 37, msgs[1].OutputTokens)
	assert.Equal(t, 16190, msgs[1].ContextTokens)
	assert.JSONEq(t,
		`{"input_tokens":1598,"output_tokens":37,"cache_read_input_tokens":14592,"cache_creation_input_tokens":0}`,
		string(msgs[1].TokenUsage),
	)
	assertTimestamp(t, msgs[1].Timestamp,
		time.UnixMilli(1782012668557))
}

func TestParseKimiSession_NativeKimiCodeToolCall(t *testing.T) {
	path := writeKimiCodeWireJSONL(t,
		"wd_myproject_a1b2c3d4", "session_uuid-tool", "main",
		[]string{
			`{"type":"metadata","protocol_version":"1.4","created_at":1782012661212}`,
			`{"type":"config.update","modelAlias":"kimi-code/kimi-for-coding","time":1782012661213}`,
			`{"type":"turn.prompt","input":[{"type":"text","text":"List files"}],"origin":{"kind":"user"},"time":1782012666987}`,
			`{"type":"context.append_loop_event","event":{"type":"step.begin","uuid":"step-1","turnId":"0","step":1},"time":1782012666989}`,
			`{"type":"context.append_loop_event","event":{"type":"tool.call","uuid":"call-event","turnId":"0","step":1,"stepUuid":"step-1","toolCallId":"tool_1","name":"Bash","args":{"command":"ls","description":"List files"}},"time":1782012667000}`,
			`{"type":"context.append_loop_event","event":{"type":"step.end","uuid":"step-1","turnId":"0","step":1,"usage":{"inputOther":10,"output":3,"inputCacheRead":20,"inputCacheCreation":0},"finishReason":"tool_use"},"time":1782012667001}`,
			`{"type":"context.append_loop_event","event":{"type":"tool.result","parentUuid":"call-event","toolCallId":"tool_1","result":{"output":"main.go\nREADME.md","isError":false}},"time":1782012667002}`,
			`{"type":"context.append_loop_event","event":{"type":"step.begin","uuid":"step-2","turnId":"0","step":2},"time":1782012667003}`,
			`{"type":"context.append_loop_event","event":{"type":"content.part","uuid":"part-1","turnId":"0","step":2,"stepUuid":"step-2","part":{"type":"text","text":"Found the files."}},"time":1782012667004}`,
			`{"type":"context.append_loop_event","event":{"type":"step.end","uuid":"step-2","turnId":"0","step":2,"usage":{"inputOther":30,"output":4,"inputCacheRead":40,"inputCacheCreation":5},"finishReason":"end_turn"},"time":1782012667005}`,
		},
	)

	sess, msgs, err := parseKimiSessionForTest(t, path, "myproject", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Equal(t, 7, sess.TotalOutputTokens)
	assert.Equal(t, 75, sess.PeakContextTokens)

	require.Len(t, msgs, 4)
	assertMessage(t, msgs[0], RoleUser, "List files")
	require.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasToolUse)
	assert.Contains(t, msgs[1].Content, "[Bash: List files]")
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "tool_1", msgs[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "Bash", msgs[1].ToolCalls[0].ToolName)
	assert.JSONEq(t, `{"command":"ls","description":"List files"}`,
		msgs[1].ToolCalls[0].InputJSON)
	assert.Equal(t, "tool_use", msgs[1].StopReason)

	require.Equal(t, RoleUser, msgs[2].Role)
	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "tool_1", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal(t, "main.go\nREADME.md",
		DecodeContent(msgs[2].ToolResults[0].ContentRaw))

	assertMessage(t, msgs[3], RoleAssistant, "Found the files.")
	assert.Equal(t, "end_turn", msgs[3].StopReason)
	assert.Equal(t, 4, msgs[3].OutputTokens)
	assert.Equal(t, 75, msgs[3].ContextTokens)
}

func TestParseKimiSession_NewLayout_AgentZero(t *testing.T) {
	path := writeKimiCodeWireJSONL(t,
		"wd_myproject_a1b2c3d4", "session_uuid-5678", "agent-0",
		[]string{
			`{"type": "metadata", "protocol_version": "1.3"}`,
			`{"timestamp": 1704067200.0, "message": {"type": "TurnBegin", "payload": {"user_input": [{"type": "text", "text": "Hello subagent"}]}}}`,
			`{"timestamp": 1704067201.0, "message": {"type": "ContentPart", "payload": {"type": "text", "text": "Done."}}}`,
			`{"timestamp": 1704067202.0, "message": {"type": "TurnEnd", "payload": {}}}`,
		},
	)

	sess, _, err := parseKimiSessionForTest(t, path, "myproject", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertSessionMeta(t, sess,
		"kimi:wd_myproject_a1b2c3d4:agent-0:session_uuid-5678",
		"myproject", AgentKimi,
	)
}

func TestDiscoverKimiSessions_MixedLayouts(t *testing.T) {
	dir := t.TempDir()

	// Legacy layout.
	legacyProjDir := filepath.Join(dir, "abc123")
	legacySessDir := filepath.Join(legacyProjDir, "uuid-1")
	require.NoError(t, os.MkdirAll(legacySessDir, 0o755))
	legacyPath := filepath.Join(legacySessDir, "wire.jsonl")
	require.NoError(t, os.WriteFile(
		legacyPath, []byte(`{"type":"metadata"}`+"\n"), 0o644,
	))

	// New layout.
	workdirDir := "wd_foo_bar"
	newSessDir := filepath.Join(dir, workdirDir, "session_xyz", "agents", "main")
	require.NoError(t, os.MkdirAll(newSessDir, 0o755))
	newPath := filepath.Join(newSessDir, "wire.jsonl")
	require.NoError(t, os.WriteFile(
		newPath, []byte(`{"type":"metadata"}`+"\n"), 0o644,
	))

	provider, ok := NewProvider(AgentKimi, ProviderConfig{Roots: []string{dir}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	paths := sourceDisplayPaths(sources)
	assert.Contains(t, paths, legacyPath)
	assert.Contains(t, paths, newPath)
}

func TestKimiSessionIDFromPath(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		path := filepath.Join("/home", "user", ".kimi", "sessions", "abc123", "uuid-1", "wire.jsonl")
		assert.Equal(t, "abc123:uuid-1", kimiSessionIDFromPath(path))
	})

	t.Run("kimi-code-main", func(t *testing.T) {
		path := filepath.Join("/home", "user", ".kimi-code", "sessions", "wd_foo_a1b2", "session_uuid-1", "agents", "main", "wire.jsonl")
		assert.Equal(t, "wd_foo_a1b2:main:session_uuid-1", kimiSessionIDFromPath(path))
	})

	t.Run("kimi-code-agent-zero", func(t *testing.T) {
		path := filepath.Join("/home", "user", ".kimi-code", "sessions", "wd_foo_a1b2", "session_uuid-1", "agents", "agent-0", "wire.jsonl")
		assert.Equal(t, "wd_foo_a1b2:agent-0:session_uuid-1", kimiSessionIDFromPath(path))
	})
}

func TestDecodeKimiProjectDir(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		// .kimi-code workdir names: "wd_<workdir>_<12-hex>".
		{"wd_kimi-code_057f5c09ee3f", "kimi-code"},
		{"wd_pi-mono_77f54d0fe81a", "pi-mono"},
		// Underscores inside the workdir name are preserved.
		{"wd_figma_mermaid_plugin_2_777a2abfe3e7", "figma_mermaid_plugin_2"},
		// Uppercase/mixed-case hex is still recognized as the hash.
		{"wd_foo_ABCDEF012345", "foo"},
		// A trailing segment that is not a 12-hex hash is kept.
		{"wd_foo_bar", "foo_bar"},
		// Legacy opaque project hashes have no "wd_" prefix and
		// are returned unchanged.
		{"03cd233b1066bcc214245959059ca4c8", "03cd233b1066bcc214245959059ca4c8"},
		{"abc123", "abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, DecodeKimiProjectDir(tt.input))
		})
	}
}

func TestDiscoverKimiSessions_NewLayout_RejectsInvalidComponent(t *testing.T) {
	dir := t.TempDir()

	// An agent name with a character outside [A-Za-z0-9_-] cannot
	// round-trip through the ':'-delimited session ID, so that agent
	// must be skipped at discovery while a valid sibling agent in the
	// same session is still imported. A space stands in for any such
	// character (':' itself is not a portable path component on
	// Windows).
	workdirDir := "wd_foo_1234567890ab"
	sessionDir := "session_uuid-1"

	badDir := filepath.Join(dir, workdirDir, sessionDir, "agents", "sub agent")
	require.NoError(t, os.MkdirAll(badDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(badDir, "wire.jsonl"),
		[]byte(`{"type":"metadata"}`+"\n"), 0o644,
	))

	goodDir := filepath.Join(dir, workdirDir, sessionDir, "agents", "main")
	require.NoError(t, os.MkdirAll(goodDir, 0o755))
	goodPath := filepath.Join(goodDir, "wire.jsonl")
	require.NoError(t, os.WriteFile(
		goodPath, []byte(`{"type":"metadata"}`+"\n"), 0o644,
	))

	provider, ok := NewProvider(AgentKimi, ProviderConfig{Roots: []string{dir}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, goodPath, sources[0].DisplayPath)
}
