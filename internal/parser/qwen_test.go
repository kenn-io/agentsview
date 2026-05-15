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
	assert.Equal(t, 18021, sess.PeakContextTokens)

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
	assert.Equal(t, 18021, msgs[1].ContextTokens)
	require.NotEmpty(t, msgs[1].TokenUsage)
	assert.Equal(t, int64(18009), gjson.GetBytes(msgs[1].TokenUsage, "input_tokens").Int())
	assert.Equal(t, int64(47), gjson.GetBytes(msgs[1].TokenUsage, "output_tokens").Int())
	assert.Equal(t, int64(12), gjson.GetBytes(msgs[1].TokenUsage, "cache_read_input_tokens").Int())
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
