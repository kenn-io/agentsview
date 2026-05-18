package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestParseQwenSession(t *testing.T) {
	t.Parallel()

	content := `{"uuid":"u1","parentUuid":null,"sessionId":"adc026b4-c620-43e4-8cc4-295593889d18","timestamp":"2026-05-05T11:08:38.572Z","type":"user","cwd":"/Users/alice/code/sample-project","version":"0.15.6","message":{"role":"user","parts":[{"text":"Calculate .089 * 7.85788"}]}}
{"uuid":"u2","parentUuid":"u1","sessionId":"adc026b4-c620-43e4-8cc4-295593889d18","timestamp":"2026-05-05T11:08:46.390Z","type":"system","cwd":"/Users/alice/code/sample-project","version":"0.15.6","subtype":"ui_telemetry","systemPayload":{"uiEvent":{"event.name":"qwen-code.api_response","event.timestamp":"2026-05-05T11:08:46.382Z","response_id":"chatcmpl-41i0j1qr4opc2pwih6jjn","model":"cleanunicorn/qwen3-coder-30b-a3b-instruct","status_code":200,"duration_ms":7478,"input_token_count":18009,"output_token_count":47,"cached_content_token_count":12,"thoughts_token_count":36,"total_token_count":18056,"prompt_id":"adc026b4-c620-43e4-8cc4-295593889d18########0","auth_type":"openai"}}}
{"uuid":"u3","parentUuid":"u2","sessionId":"adc026b4-c620-43e4-8cc4-295593889d18","timestamp":"2026-05-05T11:08:46.529Z","type":"assistant","cwd":"/Users/alice/code/sample-project","version":"0.15.6","model":"cleanunicorn/qwen3-coder-30b-a3b-instruct","message":{"role":"model","parts":[{"text":"The user wants me to calculate 0.089 * 7.85788. This is a simple multiplication.\n\n0.089 * 7.85788 = 0.69935132","thought":true},{"text":"0.089 × 7.85788 = **0.69935132**"}]},"usageMetadata":{"promptTokenCount":18009,"candidatesTokenCount":47,"thoughtsTokenCount":36,"totalTokenCount":18056,"cachedContentTokenCount":12}}`

	path := createTestFile(t, "qwen-session.jsonl", content)

	sess, msgs, err := ParseQwenSession(path, "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 2)

	assert.Equal(t, "qwen:adc026b4-c620-43e4-8cc4-295593889d18", sess.ID)
	assert.Equal(t, AgentQwen, sess.Agent)
	assert.Equal(t, "sample_project", sess.Project)
	assert.Equal(t, "/Users/alice/code/sample-project", sess.Cwd)
	assert.Equal(t, "Calculate .089 * 7.85788", sess.FirstMessage)
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 47, sess.TotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
	// promptTokenCount already includes cachedContentTokenCount in Qwen
	// usage; context tokens should equal the prompt count (not prompt
	// + cached, which would double-count the cached portion).
	assert.Equal(t, 18009, sess.PeakContextTokens)

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Calculate .089 * 7.85788", msgs[0].Content)

	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "0.089 × 7.85788 = **0.69935132**", msgs[1].Content)
	assert.True(t, msgs[1].HasThinking)
	assert.Contains(t, msgs[1].ThinkingText, "simple multiplication")
	assert.Equal(t, "cleanunicorn/qwen3-coder-30b-a3b-instruct", msgs[1].Model)
	assert.True(t, msgs[1].HasOutputTokens)
	assert.Equal(t, 47, msgs[1].OutputTokens)
	assert.True(t, msgs[1].HasContextTokens)
	assert.Equal(t, 18009, msgs[1].ContextTokens)
	require.NotEmpty(t, msgs[1].TokenUsage)
	// Normalized input_tokens is the uncached remainder
	// (promptTokenCount - cachedContentTokenCount).
	assert.Equal(t, int64(17997), gjson.GetBytes(msgs[1].TokenUsage, "input_tokens").Int())
	assert.Equal(t, int64(47), gjson.GetBytes(msgs[1].TokenUsage, "output_tokens").Int())
	assert.Equal(t, int64(12), gjson.GetBytes(msgs[1].TokenUsage, "cache_read_input_tokens").Int())
}

// TestParseQwenSession_CoalescesToolCallOnlyAssistants verifies that a
// run of assistant entries whose parts are only [thought, functionCall]
// (no user-facing text) is merged into the next text-bearing assistant
// entry, so MessageCount reflects user-visible turns rather than every
// tool-call iteration. Models the Session 9 pattern (`50510b6f`).
func TestParseQwenSession_CoalescesToolCallOnlyAssistants(t *testing.T) {
	t.Parallel()

	content := `{"uuid":"u1","sessionId":"sess-coalesce","timestamp":"2026-05-15T10:26:54.209Z","type":"user","cwd":"/work","message":{"role":"user","parts":[{"text":"Find the bug"}]}}
{"uuid":"u2","sessionId":"sess-coalesce","timestamp":"2026-05-15T10:26:55.000Z","type":"assistant","model":"qwen","message":{"role":"model","parts":[{"text":"Looking at the code first.","thought":true},{"functionCall":{"id":"c1","name":"read_file","args":{"path":"a.go"}}}]},"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":10,"cachedContentTokenCount":0}}
{"uuid":"u3","sessionId":"sess-coalesce","timestamp":"2026-05-15T10:26:56.000Z","type":"assistant","model":"qwen","message":{"role":"model","parts":[{"text":"Now searching.","thought":true},{"functionCall":{"id":"c2","name":"grep","args":{"q":"bug"}}}]},"usageMetadata":{"promptTokenCount":200,"candidatesTokenCount":20,"cachedContentTokenCount":50}}
{"uuid":"u4","sessionId":"sess-coalesce","timestamp":"2026-05-15T10:26:57.000Z","type":"assistant","model":"qwen","message":{"role":"model","parts":[{"text":"Putting it together.","thought":true},{"text":"The bug is on line 42."}]},"usageMetadata":{"promptTokenCount":300,"candidatesTokenCount":15,"cachedContentTokenCount":80}}`

	path := createTestFile(t, "coalesce.jsonl", content)

	sess, msgs, err := ParseQwenSession(path, "", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2, "expected user + single coalesced assistant turn")
	require.Equal(t, 2, sess.MessageCount)
	require.Equal(t, 1, sess.UserMessageCount)

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Find the bug", msgs[0].Content)

	a := msgs[1]
	assert.Equal(t, RoleAssistant, a.Role)
	assert.Equal(t, "The bug is on line 42.", a.Content)
	assert.True(t, a.HasThinking)
	assert.Contains(t, a.ThinkingText, "Looking at the code first.")
	assert.Contains(t, a.ThinkingText, "Now searching.")
	assert.Contains(t, a.ThinkingText, "Putting it together.")

	// Output tokens sum across coalesced entries; context tokens take
	// the peak promptTokenCount (which already includes cached) so we
	// avoid double-counting the cached portion.
	assert.True(t, a.HasOutputTokens)
	assert.Equal(t, 45, a.OutputTokens, "10 + 20 + 15")
	assert.True(t, a.HasContextTokens)
	assert.Equal(t, 300, a.ContextTokens, "max(prompt) across coalesced entries")

	// Normalized input/cache usage sums across iterations so a turn
	// with multiple tool-call API calls reports the total tokens that
	// were billed, not just the peak call's contribution:
	//   input_tokens = (100-0) + (200-50) + (300-80) = 470
	//   cache_read_input_tokens = 0 + 50 + 80 = 130
	require.NotEmpty(t, a.TokenUsage)
	assert.Equal(t, int64(470),
		gjson.GetBytes(a.TokenUsage, "input_tokens").Int())
	assert.Equal(t, int64(45),
		gjson.GetBytes(a.TokenUsage, "output_tokens").Int())
	assert.Equal(t, int64(130),
		gjson.GetBytes(a.TokenUsage, "cache_read_input_tokens").Int())

	// Tool calls from each iteration aggregate onto the coalesced turn.
	require.True(t, a.HasToolUse)
	require.Len(t, a.ToolCalls, 2)
	assert.Equal(t, "read_file", a.ToolCalls[0].ToolName)
	assert.Equal(t, "c1", a.ToolCalls[0].ToolUseID)
	assert.Equal(t, "grep", a.ToolCalls[1].ToolName)
	assert.Equal(t, "c2", a.ToolCalls[1].ToolUseID)

	// Session-level totals stay consistent with summed/maxed message values.
	assert.Equal(t, 45, sess.TotalOutputTokens)
	assert.Equal(t, 300, sess.PeakContextTokens)

	// Timestamp tracks the final (text-bearing) entry in the coalesced run.
	assert.Equal(t, "2026-05-15T10:26:57Z", a.Timestamp.UTC().Format("2006-01-02T15:04:05Z"))
}

// TestParseQwenSession_TrailingToolCallOnlyAssistants verifies that a
// trailing run of tool-call-only assistant entries (no text follow-up
// before EOF) is emitted as a single coalesced assistant message rather
// than dropped or counted N times.
func TestParseQwenSession_TrailingToolCallOnlyAssistants(t *testing.T) {
	t.Parallel()

	content := `{"uuid":"u1","sessionId":"sess-trail","timestamp":"2026-05-15T10:00:00.000Z","type":"user","cwd":"/work","message":{"role":"user","parts":[{"text":"Run tests"}]}}
{"uuid":"u2","sessionId":"sess-trail","timestamp":"2026-05-15T10:00:01.000Z","type":"assistant","model":"qwen","message":{"role":"model","parts":[{"text":"Starting test run.","thought":true},{"functionCall":{"id":"c1","name":"shell","args":{"cmd":"go test"}}}]},"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":5,"cachedContentTokenCount":0}}
{"uuid":"u3","sessionId":"sess-trail","timestamp":"2026-05-15T10:00:02.000Z","type":"assistant","model":"qwen","message":{"role":"model","parts":[{"text":"Re-running with verbose.","thought":true},{"functionCall":{"id":"c2","name":"shell","args":{"cmd":"go test -v"}}}]},"usageMetadata":{"promptTokenCount":80,"candidatesTokenCount":7,"cachedContentTokenCount":10}}`

	path := createTestFile(t, "trail.jsonl", content)

	sess, msgs, err := ParseQwenSession(path, "", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2, "trailing tool-call run should coalesce into one assistant message")
	require.Equal(t, 2, sess.MessageCount)
	require.Equal(t, 1, sess.UserMessageCount)

	a := msgs[1]
	assert.Equal(t, RoleAssistant, a.Role)
	assert.Empty(t, a.Content, "no text-bearing entry means content is empty")
	assert.True(t, a.HasThinking)
	assert.Contains(t, a.ThinkingText, "Starting test run.")
	assert.Contains(t, a.ThinkingText, "Re-running with verbose.")
	assert.Equal(t, 12, a.OutputTokens, "5 + 7")
	assert.Equal(t, 80, a.ContextTokens, "max(prompt) across coalesced entries")
	// Summed normalized input/cache usage:
	//   input_tokens = (50-0) + (80-10) = 120
	//   cache_read_input_tokens = 0 + 10 = 10
	require.NotEmpty(t, a.TokenUsage)
	assert.Equal(t, int64(120),
		gjson.GetBytes(a.TokenUsage, "input_tokens").Int())
	assert.Equal(t, int64(10),
		gjson.GetBytes(a.TokenUsage, "cache_read_input_tokens").Int())
	require.True(t, a.HasToolUse)
	require.Len(t, a.ToolCalls, 2)
	assert.Equal(t, "shell", a.ToolCalls[0].ToolName)
	assert.Equal(t, "c1", a.ToolCalls[0].ToolUseID)
	assert.Equal(t, "shell", a.ToolCalls[1].ToolName)
	assert.Equal(t, "c2", a.ToolCalls[1].ToolUseID)
}

// TestParseQwenSession_ToolUseRoundTrip verifies that an assistant
// turn made up of `[thought, functionCall]` parts followed by a
// tool-result user entry (parts: `[functionResponse]`) and a final
// text-bearing assistant entry is coalesced into a single assistant
// message carrying both the ToolCalls and the matching ToolResults.
// The synthetic tool-result user entry is folded into the assistant
// turn rather than counted as a user message.
func TestParseQwenSession_ToolUseRoundTrip(t *testing.T) {
	t.Parallel()

	content := `{"uuid":"u1","sessionId":"sess-tools","timestamp":"2026-05-15T10:00:00.000Z","type":"user","cwd":"/work","message":{"role":"user","parts":[{"text":"Read a.go"}]}}
{"uuid":"u2","sessionId":"sess-tools","timestamp":"2026-05-15T10:00:01.000Z","type":"assistant","model":"qwen","message":{"role":"model","parts":[{"text":"Calling read_file.","thought":true},{"functionCall":{"id":"c1","name":"read_file","args":{"path":"a.go"}}}]},"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5,"cachedContentTokenCount":10}}
{"uuid":"u3","sessionId":"sess-tools","timestamp":"2026-05-15T10:00:02.000Z","type":"user","cwd":"/work","message":{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"package main\n"}}}]}}
{"uuid":"u4","sessionId":"sess-tools","timestamp":"2026-05-15T10:00:03.000Z","type":"assistant","model":"qwen","message":{"role":"model","parts":[{"text":"That's the file."}]},"usageMetadata":{"promptTokenCount":150,"candidatesTokenCount":7,"cachedContentTokenCount":20}}`

	path := createTestFile(t, "tools.jsonl", content)

	sess, msgs, err := ParseQwenSession(path, "", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2,
		"tool-result user entry should fold into the assistant turn")
	assert.Equal(t, 1, sess.UserMessageCount,
		"only the typed user prompt counts as a user message")

	a := msgs[1]
	assert.Equal(t, RoleAssistant, a.Role)
	assert.Equal(t, "That's the file.", a.Content)
	require.True(t, a.HasToolUse)
	require.Len(t, a.ToolCalls, 1)
	assert.Equal(t, "read_file", a.ToolCalls[0].ToolName)
	assert.Equal(t, "c1", a.ToolCalls[0].ToolUseID)
	require.Len(t, a.ToolResults, 1)
	tr := a.ToolResults[0]
	assert.Equal(t, "c1", tr.ToolUseID)
	// The paired tool result must decode to the underlying output text,
	// not be left empty by mismatched object-shape handling.
	assert.Equal(t, "package main\n", DecodeContent(tr.ContentRaw))
	assert.Equal(t, len("package main\n"), tr.ContentLength)
	// Peak prompt (150) becomes ContextTokens. Normalized input/cache
	// usage sums across the two iterations so multi-call turns don't
	// drop the earlier API call's tokens:
	//   input_tokens = (100-10) + (150-20) = 220
	//   cache_read_input_tokens = 10 + 20 = 30
	assert.Equal(t, 150, a.ContextTokens)
	assert.Equal(t, int64(220), gjson.GetBytes(a.TokenUsage, "input_tokens").Int())
	assert.Equal(t, int64(30), gjson.GetBytes(a.TokenUsage, "cache_read_input_tokens").Int())
}

// TestParseQwenSession_TextWithFunctionCallCoalesces verifies that an
// assistant entry carrying both user-facing text and a functionCall is
// kept open until its matching functionResponse and closing text arrive,
// rather than flushing immediately and orphaning the tool result onto a
// phantom empty assistant message. Intermediate text from the same turn
// should also be preserved.
func TestParseQwenSession_TextWithFunctionCallCoalesces(t *testing.T) {
	t.Parallel()

	content := `{"uuid":"u1","sessionId":"sess-interleaved","timestamp":"2026-05-15T10:00:00.000Z","type":"user","cwd":"/work","message":{"role":"user","parts":[{"text":"Read a.go"}]}}
{"uuid":"u2","sessionId":"sess-interleaved","timestamp":"2026-05-15T10:00:01.000Z","type":"assistant","model":"qwen","message":{"role":"model","parts":[{"text":"Looking at the file."},{"functionCall":{"id":"c1","name":"read_file","args":{"path":"a.go"}}}]},"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5,"cachedContentTokenCount":0}}
{"uuid":"u3","sessionId":"sess-interleaved","timestamp":"2026-05-15T10:00:02.000Z","type":"user","cwd":"/work","message":{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"package main\n"}}}]}}
{"uuid":"u4","sessionId":"sess-interleaved","timestamp":"2026-05-15T10:00:03.000Z","type":"assistant","model":"qwen","message":{"role":"model","parts":[{"text":"Done."}]},"usageMetadata":{"promptTokenCount":150,"candidatesTokenCount":7,"cachedContentTokenCount":20}}`

	path := createTestFile(t, "interleaved.jsonl", content)

	sess, msgs, err := ParseQwenSession(path, "", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2,
		"interleaved text+functionCall must not inflate MessageCount")
	require.Equal(t, 2, sess.MessageCount)
	require.Equal(t, 1, sess.UserMessageCount)

	a := msgs[1]
	require.Equal(t, RoleAssistant, a.Role)
	require.True(t, a.HasToolUse)
	require.Len(t, a.ToolCalls, 1)
	assert.Equal(t, "c1", a.ToolCalls[0].ToolUseID)
	require.Len(t, a.ToolResults, 1,
		"tool result must be paired with the same assistant turn")
	assert.Equal(t, "c1", a.ToolResults[0].ToolUseID)
	// Both the intermediate "Looking at the file." lead-in and the
	// closing "Done." text belong to the same logical turn.
	assert.Contains(t, a.Content, "Looking at the file.")
	assert.Contains(t, a.Content, "Done.")
}

// TestParseQwenSession_AbortedNoAssistantResponse models the
// `fa8d5d8e` / `96282bca` pattern — user typed something, the session
// ended before any assistant entry was written. Should produce exactly
// one user message and no assistant inflation.
func TestParseQwenSession_AbortedNoAssistantResponse(t *testing.T) {
	t.Parallel()

	content := `{"uuid":"u1","sessionId":"sess-abort","timestamp":"2026-05-15T10:00:00.000Z","type":"user","cwd":"/work","message":{"role":"user","parts":[{"text":"Hello"}]}}
{"uuid":"u2","sessionId":"sess-abort","timestamp":"2026-05-15T10:00:01.000Z","type":"system","cwd":"/work","subtype":"ui_telemetry","systemPayload":{}}
{"uuid":"u3","sessionId":"sess-abort","timestamp":"2026-05-15T10:00:02.000Z","type":"system","cwd":"/work","subtype":"ui_telemetry","systemPayload":{}}`

	path := createTestFile(t, "abort.jsonl", content)

	sess, msgs, err := ParseQwenSession(path, "", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, 1, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Hello", msgs[0].Content)
	assert.False(t, sess.HasTotalOutputTokens, "no assistant entries -> no output tokens")
}

// TestParseQwenSession_ShortClean models the Session 10 pattern
// (`80a3069a`) — one user prompt and one text-bearing assistant
// response. The baseline "clean" short session that should not change
// shape under the new coalescing logic.
func TestParseQwenSession_ShortClean(t *testing.T) {
	t.Parallel()

	content := `{"uuid":"u1","sessionId":"sess-short","timestamp":"2026-05-15T10:25:42.212Z","type":"user","cwd":"/work","message":{"role":"user","parts":[{"text":"."}]}}
{"uuid":"u2","sessionId":"sess-short","timestamp":"2026-05-15T10:25:53.402Z","type":"assistant","model":"qwen","message":{"role":"model","parts":[{"text":"Ready when you are."}]},"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"cachedContentTokenCount":0}}`

	path := createTestFile(t, "short.jsonl", content)

	sess, msgs, err := ParseQwenSession(path, "", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "Ready when you are.", msgs[1].Content)
	assert.False(t, msgs[1].HasThinking)
}

func TestDiscoverQwenSessions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	projectDir := filepath.Join(root, "-Users-alice--qwen")
	chatsDir := filepath.Join(projectDir, "chats")
	require.NoError(t, os.MkdirAll(chatsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(chatsDir, "a.jsonl"), []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(chatsDir, "b.jsonl"), []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(chatsDir, "notes.txt"), []byte("ignore"), 0o644))

	files := DiscoverQwenSessions(root)
	require.Len(t, files, 2)
	assert.Equal(t, AgentQwen, files[0].Agent)
	assert.Equal(t, "qwen", files[0].Project)
	assert.Equal(t, AgentQwen, files[1].Agent)
	assert.Equal(t, "qwen", files[1].Project)
}

func TestFindQwenSourceFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	projectDir := filepath.Join(root, "-Users-alice-code-sample-project")
	chatsDir := filepath.Join(projectDir, "chats")
	require.NoError(t, os.MkdirAll(chatsDir, 0o755))

	sessionID := "adc026b4-c620-43e4-8cc4-295593889d18"
	want := filepath.Join(chatsDir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(want, []byte("{}\n"), 0o644))

	assert.Equal(t, want, FindQwenSourceFile(root, sessionID))
	assert.Empty(t, FindQwenSourceFile(root, "not-a-session-id"))
	assert.Empty(t, FindQwenSourceFile(root, "b0a4eadd-cb99-4165-94d9-64cad5a66d99"))
	assert.Empty(t, FindQwenSourceFile("", sessionID))
}
