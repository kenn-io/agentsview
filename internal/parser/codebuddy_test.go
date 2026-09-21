package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func writeCodeBuddyFixture(t *testing.T, root, workspace, id string) string {
	t.Helper()

	dir := filepath.Join(root, "history", workspace, id)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "messages"), 0o755))
	index := filepath.Join(dir, "index.json")
	require.NoError(t, os.WriteFile(index, []byte(`{"messages":[{"id":"u1"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "messages", "u1.json"), []byte(`{"role":"user","message":{"content":[{"type":"text","text":"Workspace Folder: /workspace/demo\n<user_query>hello</user_query>"}]}}`), 0o600))
	return index
}

func TestCodeBuddySourceContent(t *testing.T) {
	for _, tc := range []struct{ name, envelope, extra, content, cwd string }{
		{"multiple blocks", "Workspace Folder: /workspace/demo\n", `{"sourceContentBlocks":[{"text":"first"},{"text":"second"}]}`, "first\nsecond", "/workspace/demo"},
		{"blank source fallback", "<user_query>hello</user_query>", `{"sourceContentBlocks":[{"text":"  "}]}`, "hello", ""},
		{"cwd without newline", "Workspace Folder: /workspace/demo", `{}`, "Workspace Folder: /workspace/demo", "/workspace/demo"},
		{"inline closing tag", "<user_info>Workspace Folder: /workspace/demo</user_info>\n<user_query>hello</user_query>", `{}`, "hello", "/workspace/demo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := gjson.Parse(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, tc.envelope))
			content, cwd := extractCodeBuddyUserContent(inner, gjson.Parse(tc.extra))
			assert.Equal(t, tc.content, content)
			assert.Equal(t, tc.cwd, cwd)
		})
	}
}

func TestCodeBuddyDiscoveryParseFingerprint(t *testing.T) {
	root := t.TempDir()
	index := writeCodeBuddyFixture(t, root, "ws_hash", "conv_1")
	message := filepath.Join(filepath.Dir(index), "messages", "u1.json")
	later := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(message, later, later))
	set := newCodeBuddySourceSet([]string{root})
	sources, err := set.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fp, err := set.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	results, _, err := codeBuddyParseFile(t.Context(), index, ParseRequest{Source: sources[0], Fingerprint: fp})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "demo", results[0].Session.Project)
	assert.Equal(t, fp.Size, results[0].Session.File.Size)
	assert.Equal(t, fp.MTimeNS, results[0].Session.File.Mtime)
	assert.Equal(t, fp.Hash, results[0].Session.File.Hash)
	unchanged, err := set.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	assert.Equal(t, fp, unchanged)
	require.NoError(t, os.WriteFile(message, []byte(`{"role":"user","message":{"content":[{"type":"text","text":"updated"}]}}`), 0o600))
	updated, err := set.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	assert.NotEqual(t, fp.Hash, updated.Hash)
	sess, _, err := parseCodeBuddySession(index, sources[0].ProjectHint, "test")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "ws_hash", sess.Project)
}

func TestCodeBuddyChangedPaths(t *testing.T) {
	for _, unrelated := range []int{1, 100} {
		t.Run(strconv.Itoa(unrelated), func(t *testing.T) {
			root := t.TempDir()
			index := writeCodeBuddyFixture(t, root, "ws_target", "conv_1")
			index2 := writeCodeBuddyFixture(t, root, "ws_target", "conv_2")
			for i := range unrelated {
				writeCodeBuddyFixture(t, root, fmt.Sprintf("ws_other_%d", i), "conv_other")
			}
			set := newCodeBuddySourceSet([]string{root})
			// The event path must never discover unrelated workspaces. Count
			// classification calls so archive growth cannot hide a full scan.
			visits := 0
			set.options.IncludePath = func(root, path string) bool {
				visits++
				assert.Equal(t, "ws_target", filepath.Base(filepath.Dir(filepath.Dir(path))))
				return isCodeBuddySourcePath(root, path)
			}
			wsIndex := filepath.Join(filepath.Dir(filepath.Dir(index)), "index.json")
			for _, event := range []string{"write", "remove"} {
				if event == "write" {
					require.NoError(t, os.WriteFile(wsIndex, []byte(`{"conversations":[]}`), 0o600))
				} else {
					require.NoError(t, os.Remove(wsIndex))
				}
				visits = 0
				found, err := set.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: wsIndex, WatchRoot: root, EventKind: event})
				require.NoError(t, err)
				require.Len(t, found, 2)
				assert.ElementsMatch(t, []string{index, index2}, []string{found[0].DisplayPath, found[1].DisplayPath})
				assert.Equal(t, 4, visits)
			}
			message := filepath.Join(filepath.Dir(index), "messages", "u1.json")
			for _, event := range []string{"write", "remove"} {
				if event == "remove" {
					require.NoError(t, os.Remove(message))
				}
				visits = 0
				found, err := set.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: message, EventKind: event})
				require.NoError(t, err)
				require.Len(t, found, 1)
				assert.Equal(t, index, found[0].DisplayPath)
				assert.Equal(t, 2, visits)
			}
			outside := writeCodeBuddyFixture(t, t.TempDir(), "ws_outside", "conv_1")
			found, err := set.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: outside})
			require.NoError(t, err)
			assert.Empty(t, found)
			found, err = set.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: index, WatchRoot: t.TempDir()})
			require.NoError(t, err)
			assert.Empty(t, found)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = set.SourcesForChangedPath(ctx, ChangedPathRequest{Path: wsIndex})
			assert.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestCodeBuddyThinkingAndUsageOnly(t *testing.T) {
	for _, content := range []string{
		`[{"type":"reasoning","text":"inspect carefully"}]`,
		`[{"type":"thinking","thinking":"inspect carefully"}]`,
		`[]`,
	} {
		root := t.TempDir()
		index := writeCodeBuddyFixture(t, root, "ws_test", "conv_1")
		message := fmt.Sprintf(`{"role":"assistant","message":{"content":%s},"extra":{"statsSnapshot":{"thinkingTokens":12}}}`, content)
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(index), "messages", "u1.json"), []byte(message), 0o600))
		sess, msgs, err := parseCodeBuddySession(index, "", "test")
		require.NoError(t, err)
		require.NotNil(t, sess)
		require.Len(t, msgs, 1)
		assert.Equal(t, int64(12), gjson.GetBytes(msgs[0].TokenUsage, "reasoning_tokens").Int())
		assert.Equal(t, content != `[]`, msgs[0].HasThinking)
		if content != `[]` {
			assert.Equal(t, "inspect carefully", msgs[0].ThinkingText)
		}
		assert.False(t, msgs[0].HasContextTokens)
		assert.False(t, msgs[0].HasOutputTokens)
	}
}

func TestCodeBuddyUsagePresence(t *testing.T) {
	for _, tc := range []struct {
		extra         string
		input, output bool
	}{
		{`{}`, false, false},
		{`{"lastStepInputTokens":null,"lastStepOutputTokens":-1}`, false, false},
		{`{"lastStepInputTokens":0,"lastStepOutputTokens":0}`, true, true},
		{`{"lastStepCachedInputTokens":80}`, false, false},
	} {
		var msg ParsedMessage
		applyCodeBuddyUsage(&msg, gjson.Parse(tc.extra))
		assert.Equal(t, tc.input, msg.HasContextTokens)
		assert.Equal(t, tc.output, msg.HasOutputTokens)
	}
}

func TestCodeBuddyMalformedMessages(t *testing.T) {
	root := t.TempDir()
	index := writeCodeBuddyFixture(t, root, "ws_test", "conv_1")
	require.NoError(t, os.WriteFile(index, []byte(`{"messages":[{"id":"u1"},{"id":"missing"},{"id":"bad"},{"id":"../outside"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(index), "messages", "bad.json"), []byte(`{`), 0o600))
	sess, msgs, err := parseCodeBuddySession(index, "", "test")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Len(t, msgs, 1)
	assert.Equal(t, 3, sess.MalformedLines)
	require.NoError(t, os.WriteFile(index, []byte(`{`), 0o600))
	_, _, err = parseCodeBuddySession(index, "", "test")
	assert.Error(t, err)
}

func TestCodeBuddyUsageCacheEdges(t *testing.T) {
	for _, tc := range []struct{ input, cache, uncached int }{
		{100, 80, 20}, {100, 0, 100}, {0, 0, 0}, {20, 80, 0},
	} {
		var msg ParsedMessage
		applyCodeBuddyUsage(&msg, gjson.Parse(fmt.Sprintf(`{"lastStepInputTokens":%d,"lastStepCachedInputTokens":%d,"lastStepOutputTokens":5,"statsSnapshot":{"thinkingTokens":2}}`, tc.input, tc.cache)))
		assert.Equal(t, int64(tc.uncached), gjson.GetBytes(msg.TokenUsage, "input_tokens").Int())
		assert.Equal(t, int64(tc.cache), gjson.GetBytes(msg.TokenUsage, "cache_read_input_tokens").Int())
		assert.Equal(t, int64(2), gjson.GetBytes(msg.TokenUsage, "reasoning_tokens").Int())
		assert.Equal(t, tc.input, msg.ContextTokens)
		assert.Equal(t, 5, msg.OutputTokens)
	}
}

func TestCodeBuddyToolResultFallback(t *testing.T) {
	for _, tc := range []struct{ inner, extra, expected string }{
		{`{"content":[{"type":"tool-result","toolCallId":"call_1","result":{"result":{"stdout":"done"}}}]}`, `{}`, `"done"`},
		{`{"content":[]}`, `{"toolStatus":{"call_1":{"result":{"result":{"stdout":"done"}}}}}`, `"done"`},
		{`{"content":[{"type":"tool-result","toolCallId":"call_1","result":{"error":"failed"}}]}`, `{}`, `"{\"error\":\"failed\"}"`},
	} {
		results := extractCodeBuddyToolResults(gjson.Parse(tc.inner), gjson.Parse(tc.extra))
		require.Len(t, results, 1)
		assert.Equal(t, "call_1", results[0].ToolUseID)
		assert.Equal(t, tc.expected, results[0].ContentRaw)
	}
	assert.Empty(t, extractCodeBuddyToolResults(gjson.Parse(`{"content":[{"type":"tool-result"}]}`), gjson.Parse(`{}`)))
}

func TestDiscoverCodeBuddySessions(t *testing.T) {
	root := t.TempDir()
	historyDir := filepath.Join(root, "history", "ws_123")

	// Workspace index (should NOT be classified as a session)
	require.NoError(t, os.MkdirAll(historyDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(historyDir, "index.json"), []byte(`{"conversations":[]}`), 0o644))

	// Session 1
	s1Dir := filepath.Join(historyDir, "conv_abc")
	require.NoError(t, os.MkdirAll(filepath.Join(s1Dir, "messages"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(s1Dir, "index.json"), []byte(`{"messages":[]}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(s1Dir, "messages", "m1.json"), []byte(`{}`), 0o644))

	// Session 2
	s2Dir := filepath.Join(historyDir, "conv_def")
	require.NoError(t, os.MkdirAll(s2Dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(s2Dir, "index.json"), []byte(`{"messages":[]}`), 0o644))

	// Non-matching file
	require.NoError(t, os.WriteFile(filepath.Join(historyDir, "other.json"), []byte(`{}`), 0o644))

	sourceSet := newCodeBuddySourceSet([]string{root})
	discovered, err := sourceSet.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 2)

	assert.Equal(t, filepath.Join(s1Dir, "index.json"), discovered[0].DisplayPath)
	assert.Equal(t, "ws_123", discovered[0].ProjectHint)

	assert.Equal(t, filepath.Join(s2Dir, "index.json"), discovered[1].DisplayPath)
	assert.Equal(t, "ws_123", discovered[1].ProjectHint)
}

func TestParseCodeBuddySession(t *testing.T) {
	root := t.TempDir()
	historyDir := filepath.Join(root, "history", "ws_test")
	require.NoError(t, os.MkdirAll(historyDir, 0o755))

	// Workspace index with title and model metadata
	wsIndexContent := `{
  "conversations": [
    {
      "id": "conv_001",
      "name": "Build Go Parser",
      "createdAt": "2026-09-17T02:00:00.000Z",
      "selectedModelId": "deepseek-v4.1-flash"
    }
  ]
}`
	require.NoError(t, os.WriteFile(filepath.Join(historyDir, "index.json"), []byte(wsIndexContent), 0o644))

	sessionDir := filepath.Join(historyDir, "conv_001")
	messagesDir := filepath.Join(sessionDir, "messages")
	require.NoError(t, os.MkdirAll(messagesDir, 0o755))

	sessionIndexContent := `{
  "messages": [
    {"id": "msg_u1", "role": "user", "type": "text"},
    {"id": "msg_a1", "role": "assistant", "type": "text"},
    {"id": "msg_t1", "role": "tool", "type": "text"},
    {"id": "msg_a2", "role": "assistant", "type": "text"}
  ]
}`
	indexPath := filepath.Join(sessionDir, "index.json")
	require.NoError(t, os.WriteFile(indexPath, []byte(sessionIndexContent), 0o644))

	// User message
	userMsgContent := `{
  "id": "msg_u1",
  "role": "user",
  "createdAt": "2026-09-17T02:00:01.000Z",
  "message": "{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"<user_info>\\nWorkspace Folder: /workspace/demo\\n</user_info>\\n<user_query>\\nPlease inspect the code.\\n</user_query>\"}]}",
  "extra": "{}"
}`
	require.NoError(t, os.WriteFile(filepath.Join(messagesDir, "msg_u1.json"), []byte(userMsgContent), 0o644))

	// Assistant message with tool call
	asstMsg1Content := `{
  "id": "msg_a1",
  "role": "assistant",
  "createdAt": "2026-09-17T02:00:05.000Z",
  "message": "{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"Let me check.\"},{\"type\":\"tool-call\",\"toolCallId\":\"call_999\",\"toolName\":\"execute_command\",\"args\":{\"command\":\"ls -la\"}}]}",
  "extra": "{\"modelId\":\"deepseek-v4.1-flash\",\"lastStepInputTokens\":100,\"lastStepOutputTokens\":25,\"lastStepCachedInputTokens\":80}"
}`
	require.NoError(t, os.WriteFile(filepath.Join(messagesDir, "msg_a1.json"), []byte(asstMsg1Content), 0o644))

	// Tool result message
	toolMsgContent := `{
  "id": "msg_t1",
  "role": "tool",
  "createdAt": "2026-09-17T02:00:06.000Z",
  "message": "{\"role\":\"tool\",\"content\":[{\"type\":\"tool-result\",\"toolCallId\":\"call_999\",\"result\":{\"result\":{\"stdout\":\"file1.txt\\nfile2.txt\"}}}]}",
  "extra": "{}"
}`
	require.NoError(t, os.WriteFile(filepath.Join(messagesDir, "msg_t1.json"), []byte(toolMsgContent), 0o644))

	// Final assistant message
	asstMsg2Content := `{
  "id": "msg_a2",
  "role": "assistant",
  "createdAt": "2026-09-17T02:00:10.000Z",
  "message": "{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"All files inspected successfully.\"}]}",
  "extra": "{\"modelId\":\"deepseek-v4.1-flash\",\"lastStepInputTokens\":150,\"lastStepOutputTokens\":30,\"lastStepCachedInputTokens\":100}"
}`
	require.NoError(t, os.WriteFile(filepath.Join(messagesDir, "msg_a2.json"), []byte(asstMsg2Content), 0o644))

	sess, msgs, err := parseCodeBuddySession(indexPath, "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Equal(t, "codebuddy:conv_001", sess.ID)
	assert.Equal(t, "Build Go Parser", sess.SessionName)
	assert.Equal(t, "Please inspect the code.", sess.FirstMessage)
	assert.Equal(t, "/workspace/demo", sess.Cwd)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Len(t, msgs, 4)

	// Verify User Message
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Please inspect the code.", msgs[0].Content)

	// Verify Assistant Tool Call
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "call_999", msgs[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "execute_command", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, `{"command":"ls -la"}`, msgs[1].ToolCalls[0].InputJSON)
	assert.Equal(t, "deepseek-v4.1-flash", msgs[1].Model)

	// Verify Token Usage calculation (input = 100 - 80 = 20, cached = 80, output = 25)
	assert.Equal(t, 25, msgs[1].OutputTokens)
	assert.Equal(t, 100, msgs[1].ContextTokens)
	assert.Equal(t, int64(20), gjson.GetBytes(msgs[1].TokenUsage, "input_tokens").Int())
	assert.Equal(t, int64(80), gjson.GetBytes(msgs[1].TokenUsage, "cache_read_input_tokens").Int())
	assert.Equal(t, int64(25), gjson.GetBytes(msgs[1].TokenUsage, "output_tokens").Int())

	// Verify Tool Result
	assert.Equal(t, RoleUser, msgs[2].Role)
	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "call_999", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal(t, "\"file1.txt\\nfile2.txt\"", msgs[2].ToolResults[0].ContentRaw)

	// Verify Final Assistant Message
	assert.Equal(t, RoleAssistant, msgs[3].Role)
	assert.Equal(t, "All files inspected successfully.", msgs[3].Content)
}

func TestCodeBuddyPathHelpers(t *testing.T) {
	validPath := filepath.Join("C:", "data", "history", "ws_123", "conv_abc", "index.json")
	assert.True(t, isCodeBuddySourcePath("C:\\data", validPath))

	// Invalid: not index.json
	assert.False(t, isCodeBuddySourcePath("C:\\data", filepath.Join("C:", "data", "history", "ws_123", "conv_abc", "other.json")))

	// Invalid: workspace level index.json
	assert.False(t, isCodeBuddySourcePath("C:\\data", filepath.Join("C:", "data", "history", "ws_123", "index.json")))

	// Project hint & session ID
	assert.Equal(t, "ws_123", codeBuddyProjectHintFromPath("C:\\data", validPath))
	assert.Equal(t, "conv_abc", codeBuddySessionIDFromPath("C:\\data", validPath))

	// Lookup ID
	assert.True(t, isCodeBuddyLookupID("codebuddy:conv_abc"))
	assert.True(t, isCodeBuddyLookupID("conv_abc"))
	assert.False(t, isCodeBuddyLookupID("codebuddy:invalid/slash"))
}

func TestCodeBuddyCompanionFiles(t *testing.T) {
	root := t.TempDir()
	historyDir := filepath.Join(root, "history", "ws_test")
	sessionDir := filepath.Join(historyDir, "conv_001")
	messagesDir := filepath.Join(sessionDir, "messages")
	require.NoError(t, os.MkdirAll(messagesDir, 0o755))

	wsIndex := filepath.Join(historyDir, "index.json")
	require.NoError(t, os.WriteFile(wsIndex, []byte(`{}`), 0o644))

	indexPath := filepath.Join(sessionDir, "index.json")
	require.NoError(t, os.WriteFile(indexPath, []byte(`{}`), 0o644))

	msg1 := filepath.Join(messagesDir, "m1.json")
	require.NoError(t, os.WriteFile(msg1, []byte(`{}`), 0o644))

	companions := codeBuddyCompanionFiles(indexPath)
	assert.Contains(t, companions, wsIndex)
	assert.Contains(t, companions, msg1)

	// Companion transcript mapping
	transcript, ok := codeBuddyCompanionTranscript(msg1)
	assert.True(t, ok)
	assert.Equal(t, indexPath, transcript)

	// Non-message file
	_, ok = codeBuddyCompanionTranscript(wsIndex)
	assert.False(t, ok)
}
