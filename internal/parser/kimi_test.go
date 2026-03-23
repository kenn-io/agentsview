package parser

import (
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

	sess, msgs, err := ParseKimiSession(
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

	sess, msgs, err := ParseKimiSession(
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

	sess, msgs, err := ParseKimiSession(
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

	sess, msgs, err := ParseKimiSession(
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

	sess, msgs, err := ParseKimiSession(
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

	sess, _, err := ParseKimiSession(
		path, "testproj", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 225, sess.TotalOutputTokens)
	assert.Equal(t, 8000, sess.PeakContextTokens)
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

	sess, _, err := ParseKimiSession(
		path, "testproj", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 42, sess.TotalOutputTokens)
	assert.Equal(t, 5000, sess.PeakContextTokens)
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

	sess, _, err := ParseKimiSession(path, "", "local")
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

	_, msgs, err := ParseKimiSession(
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

		_, msgs, err := ParseKimiSession(
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

		_, msgs, err := ParseKimiSession(
			path, "testproj", "local",
		)
		require.NoError(t, err)
		require.Equal(t, 2, len(msgs))
		assertTimestamp(t, msgs[1].Timestamp,
			time.Unix(1704067205, 0))
	})
}

func TestParseKimiSession_MissingFile(t *testing.T) {
	_, _, err := ParseKimiSession(
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

	sess, _, err := ParseKimiSession(
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

	files := DiscoverKimiSessions(dir)
	require.Equal(t, 2, len(files))
	assert.Equal(t, AgentKimi, files[0].Agent)
	assert.Equal(t, "abc123", files[0].Project)
}

func TestDiscoverKimiSessions_Empty(t *testing.T) {
	files := DiscoverKimiSessions("")
	assert.Nil(t, files)

	files = DiscoverKimiSessions("/nonexistent")
	assert.Nil(t, files)
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

	found := FindKimiSourceFile(dir, "abc123:uuid-1")
	assert.Equal(t, wirePath, found)

	assert.Equal(t, "",
		FindKimiSourceFile(dir, "abc123:nonexistent"))
	assert.Equal(t, "",
		FindKimiSourceFile(dir, "invalid"))
	assert.Equal(t, "",
		FindKimiSourceFile("", "abc123:uuid-1"))
}
