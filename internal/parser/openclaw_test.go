package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeOpenClawTestFile creates a test JSONL file inside an
// agent directory structure: <root>/<agentId>/sessions/<name>.jsonl.
// Returns the full path to the file and the root agents directory.
func writeOpenClawTestFile(
	t *testing.T, agentID string, lines ...string,
) (path, agentsDir string) {
	t.Helper()
	root := t.TempDir()
	sessDir := filepath.Join(root, agentID, "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0755))
	path = filepath.Join(sessDir, "test-session.jsonl")
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	content := b.String()
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path, root
}

func parseOpenClawSessionForTest(
	t *testing.T,
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{
		Roots:   []string{filepath.Dir(filepath.Dir(filepath.Dir(path)))},
		Machine: machine,
	})
	require.True(t, ok)
	claw, ok := provider.(*openClawProvider)
	require.True(t, ok)
	return claw.parseSession(path, project, machine)
}

func discoverOpenClawSessionsForTest(t *testing.T, root string) []SourceRef {
	t.Helper()
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	return sources
}

func findOpenClawSourceForTest(t *testing.T, root, rawID string) string {
	t.Helper()
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: rawID,
	})
	require.NoError(t, err)
	if !found {
		return ""
	}
	return source.DisplayPath
}

func TestParseOpenClawSession_Basic(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"abc-123","timestamp":"2026-02-25T10:00:00Z","cwd":"/home/user/project"}`,
		`{"type":"model_change","id":"mc1","timestamp":"2026-02-25T10:00:00Z","provider":"anthropic","modelId":"claude-sonnet-4-6"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Hello, how are you?"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
		`{"type":"message","id":"m2","timestamp":"2026-02-25T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"I'm doing well, thanks!"}],"timestamp":"2026-02-25T10:00:02Z"}}`,
	)

	sess, msgs, err := parseOpenClawSessionForTest(t, path, "", "test-machine")
	require.NoError(t, err)
	require.NotNil(t, sess, "expected session, got nil")

	assert.Equal(t, "openclaw:main:abc-123", sess.ID, "expected ID openclaw:main:abc-123, got %s")
	assert.Equal(t, AgentOpenClaw, sess.Agent, "expected agent openclaw, got %s")
	assert.Equal(t, "test-machine", sess.Machine, "expected machine test-machine, got %s")
	assert.Equal(t, "project", sess.Project, "expected project 'project', got %s")
	assert.Equal(t, "Hello, how are you?", sess.FirstMessage, "expected first message 'Hello, how are you?', got %s")
	require.Equal(t, 2, len(msgs), "expected 2 messages, got %d")
	assert.Equal(t, RoleUser, msgs[0].Role, "expected first role user, got %s")
	assert.Equal(t, RoleAssistant, msgs[1].Role, "expected second role assistant, got %s")
	assert.Equal(t, 1, sess.UserMessageCount, "expected 1 user message, got %d")
}

func TestParseOpenClawSession_Thinking(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"think-123","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Think about this"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
		`{"type":"message","id":"m2","timestamp":"2026-02-25T10:00:02Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Let me consider..."},{"type":"text","text":"Here is my response."}],"timestamp":"2026-02-25T10:00:02Z"}}`,
	)

	_, msgs, err := parseOpenClawSessionForTest(t, path, "", "test")
	require.NoError(t, err)
	require.Equal(t, 2, len(msgs), "expected 2 messages, got %d")
	assert.True(t, msgs[1].HasThinking, "expected HasThinking=true for assistant message")
}

func TestParseOpenClawSession_ToolResult(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"tool-123","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Read a file"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
		`{"type":"message","id":"m2","timestamp":"2026-02-25T10:00:02Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"read","input":{"path":"/etc/hosts"}}],"timestamp":"2026-02-25T10:00:02Z"}}`,
		`{"type":"message","id":"m3","timestamp":"2026-02-25T10:00:03Z","message":{"role":"toolResult","toolCallId":"tu1","toolName":"read","content":[{"type":"text","text":"127.0.0.1 localhost"}],"isError":false,"timestamp":"2026-02-25T10:00:03Z"}}`,
		`{"type":"message","id":"m4","timestamp":"2026-02-25T10:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"The hosts file contains localhost."}],"timestamp":"2026-02-25T10:00:04Z"}}`,
	)

	sess, msgs, err := parseOpenClawSessionForTest(t, path, "", "test")
	require.NoError(t, err)
	require.Equal(t, 4, len(msgs), "expected 4 messages, got %d")
	// Assistant with tool_use
	assert.True(t, msgs[1].HasToolUse, "expected HasToolUse=true for tool-use message")
	require.Equal(t, 1, len(msgs[1].ToolCalls), "expected 1 tool call, got %d")
	assert.Equal(t, "read", msgs[1].ToolCalls[0].ToolName, "expected tool name 'read', got %s")
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].Category, "expected category 'Read', got %s")

	// Tool result mapped to user role
	assert.Equal(t, RoleUser, msgs[2].Role, "expected tool result as user role, got %s")
	require.Equal(t, 1, len(msgs[2].ToolResults), "expected 1 tool result, got %d")
	assert.Equal(t, "tu1", msgs[2].ToolResults[0].ToolUseID, "expected tool use ID 'tu1', got %s")
	assert.Equal(t, 4, sess.MessageCount, "expected 4 messages, got %d")

	// UserMessageCount should only count the real user message,
	// not the synthetic tool-result message.
	assert.Equal(t, 1, sess.UserMessageCount, "expected UserMessageCount 1 (tool results excluded), got %d")
}

func TestParseOpenClawSession_RealToolCallFormat(t *testing.T) {
	// Real OpenClaw output uses camelCase "toolCall" blocks for
	// assistant tool calls and "toolResult" content blocks inside
	// role:"toolResult" messages -- not the Anthropic snake_case
	// "tool_use"/"tool_result" the shared extractor originally knew.
	// Some tools (read/exec) populate only "arguments"; others (bash)
	// duplicate it under "input".
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"real-tc","timestamp":"2026-02-25T10:00:00Z","cwd":"/home/user/project"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"list files"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
		// Assistant turn that is ONLY a toolCall (input + arguments duplicated).
		`{"type":"message","id":"m2","timestamp":"2026-02-25T10:00:02Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call_1","name":"bash","input":{"command":"ls"},"arguments":{"command":"ls"}}],"timestamp":"2026-02-25T10:00:02Z"}}`,
		// Tool result as a standalone toolResult-role message carrying a toolResult content block.
		`{"type":"message","id":"m3","timestamp":"2026-02-25T10:00:03Z","message":{"role":"toolResult","toolCallId":"call_1","toolName":"bash","isError":false,"content":[{"type":"toolResult","toolCallId":"call_1","tool_use_id":"call_1","name":"bash","text":"file1\nfile2"}],"timestamp":"2026-02-25T10:00:03Z"}}`,
		// Second toolCall where args live ONLY under "arguments".
		`{"type":"message","id":"m4","timestamp":"2026-02-25T10:00:04Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call_2","name":"read","arguments":{"path":"/etc/hosts"}}],"timestamp":"2026-02-25T10:00:04Z"}}`,
	)

	sess, msgs, err := parseOpenClawSessionForTest(t, path, "", "test")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 4, len(msgs),
		"expected user, assistant(toolCall), toolResult, assistant(toolCall)")

	// The assistant turn that is only a toolCall must survive and
	// carry its parsed call rather than being dropped as empty.
	assistant := msgs[1]
	assert.Equal(t, RoleAssistant, assistant.Role)
	assert.True(t, assistant.HasToolUse, "toolCall block must set HasToolUse")
	require.Equal(t, 1, len(assistant.ToolCalls), "expected 1 parsed tool call")
	assert.Equal(t, "bash", assistant.ToolCalls[0].ToolName)
	assert.Equal(t, "call_1", assistant.ToolCalls[0].ToolUseID)
	assert.Contains(t, assistant.ToolCalls[0].InputJSON, "ls")

	// The standalone toolResult message pairs by toolCallId and
	// extracts the text from the toolResult content block.
	result := msgs[2]
	assert.Equal(t, RoleUser, result.Role)
	require.Equal(t, 1, len(result.ToolResults), "expected 1 tool result")
	assert.Equal(t, "call_1", result.ToolResults[0].ToolUseID)
	assert.Equal(t, len("file1\nfile2"), result.ToolResults[0].ContentLength,
		"toolResult content block text must be extracted")
	// ContentRaw must be stored so pairToolResults can decode it into
	// tool_calls.result_content for the UI, search, copy, and exports.
	require.NotEmpty(t, result.ToolResults[0].ContentRaw,
		"ContentRaw must be stored for downstream decoding")
	assert.Equal(t, "file1\nfile2",
		DecodeContent(result.ToolResults[0].ContentRaw),
		"stored ContentRaw must decode to the tool result text")

	// An arguments-only toolCall still yields InputJSON from "arguments".
	argsOnly := msgs[3]
	require.Equal(t, 1, len(argsOnly.ToolCalls), "expected 1 parsed tool call")
	assert.Equal(t, "read", argsOnly.ToolCalls[0].ToolName)
	assert.Contains(t, argsOnly.ToolCalls[0].InputJSON, "/etc/hosts")
}

func TestParseOpenClawSession_OrphanToolResult(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"orphan-tr","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hello"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
		`{"type":"message","id":"m2","timestamp":"2026-02-25T10:00:02Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"read","input":{}}],"timestamp":"2026-02-25T10:00:02Z"}}`,
		// toolResult with empty toolCallId — should be dropped
		`{"type":"message","id":"m3","timestamp":"2026-02-25T10:00:03Z","message":{"role":"toolResult","toolCallId":"","toolName":"read","content":[{"type":"text","text":"orphan result"}],"isError":false,"timestamp":"2026-02-25T10:00:03Z"}}`,
		`{"type":"message","id":"m4","timestamp":"2026-02-25T10:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"timestamp":"2026-02-25T10:00:04Z"}}`,
	)

	sess, msgs, err := parseOpenClawSessionForTest(t, path, "", "test")
	require.NoError(t, err)
	// 3 messages: user, assistant (tool_use), assistant (text).
	// The orphan toolResult is skipped entirely.
	require.Equal(t, 3, len(msgs), "expected 3 messages, got %d")
	assert.Equal(t, 3, sess.MessageCount, "MessageCount = %d, want 3")
	assert.Equal(t, 1, sess.UserMessageCount, "UserMessageCount = %d, want 1")
	for _, m := range msgs {
		assert.False(t, m.Role == RoleUser && m.Content == "", "blank user message leaked through")
	}
}

func TestParseOpenClawSession_EmptyFile(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"empty","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
	)

	sess, _, err := parseOpenClawSessionForTest(t, path, "", "test")
	require.NoError(t, err)
	assert.Nil(t, sess, "expected nil session for file with no messages")
}

func TestParseOpenClawSession_AssistantUsage(t *testing.T) {
	// Synthetic fixture covering the OpenClaw assistant-turn usage
	// shape: per-message provider/model and a usage block with
	// short-name token counts plus a nested cost object.
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"usage-1","timestamp":"2026-04-30T12:00:00Z","cwd":"/home/user/proj"}`,
		`{"type":"model_change","id":"mc1","timestamp":"2026-04-30T12:00:00Z","provider":"anthropic","modelId":"claude-sonnet-4-6"}`,
		`{"type":"message","id":"u1","timestamp":"2026-04-30T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"do a thing"}],"timestamp":"2026-04-30T12:00:01Z"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-30T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"timestamp":"2026-04-30T12:00:02Z","provider":"anthropic","model":"claude-sonnet-4-6","usage":{"input":3,"output":91,"cacheRead":0,"cacheWrite":9612,"totalTokens":9706,"cost":{"input":0.000009,"output":0.001365,"cacheRead":0,"cacheWrite":0.036045,"total":0.037419}}}}`,
	)

	sess, msgs, err := parseOpenClawSessionForTest(t, path, "", "test")
	require.NoError(t, err)
	require.Equal(t, 2, len(msgs), "expected 2 messages, got %d")

	a := msgs[1]
	require.Equal(t, RoleAssistant, a.Role, "expected assistant role, got %s")
	assert.Equal(t, "claude-sonnet-4-6", a.Model, "Model = %q, want claude-sonnet-4-6")
	assert.Equal(t, 91, a.OutputTokens, "OutputTokens = %d, want 91")
	assert.True(t, a.HasOutputTokens, "HasOutputTokens = false, want true")
	// ContextTokens = input + cacheRead + cacheWrite.
	assert.Equal(t, 9615, a.ContextTokens, "ContextTokens = %d, want 9615")
	assert.True(t, a.HasContextTokens, "HasContextTokens = false, want true")
	// TokenUsage must be normalized to Anthropic-style keys so
	// downstream usage aggregation (internal/db/usage.go) can
	// read input_tokens/output_tokens/cache_*_input_tokens.
	require.False(t, len(a.TokenUsage) == 0, "TokenUsage empty, want normalized JSON")
	tu := string(a.TokenUsage)
	for _, want := range []string{
		`"input_tokens":3`,
		`"output_tokens":91`,
		`"cache_read_input_tokens":0`,
		`"cache_creation_input_tokens":9612`,
	} {
		assert.Contains(t, tu, want)
	}

	// Session-level rollup must reflect the per-message totals.
	assert.True(t, sess.HasTotalOutputTokens, "sess.HasTotalOutputTokens = false, want true")
	assert.Equal(t, 91, sess.TotalOutputTokens, "TotalOutputTokens")
	assert.True(t, sess.HasPeakContextTokens, "sess.HasPeakContextTokens = false, want true")
	assert.Equal(t, 9615, sess.PeakContextTokens, "PeakContextTokens")
}

func TestParseOpenClawSession_AssistantUsageWithoutCost(t *testing.T) {
	// Older sessions may carry a usage block without the nested
	// cost object. Token extraction must still succeed and not
	// crash on the missing field.
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"usage-2","timestamp":"2026-04-30T12:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","timestamp":"2026-04-30T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":"2026-04-30T12:00:01Z"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-30T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"hi back"}],"timestamp":"2026-04-30T12:00:02Z","provider":"anthropic","model":"claude-haiku-4-5","usage":{"input":42,"output":17,"cacheRead":0,"cacheWrite":0,"totalTokens":59}}}`,
	)

	sess, msgs, err := parseOpenClawSessionForTest(t, path, "", "test")
	require.NoError(t, err)
	require.Equal(t, 2, len(msgs), "expected 2 messages, got %d")

	a := msgs[1]
	assert.Equal(t, "claude-haiku-4-5", a.Model, "Model = %q, want claude-haiku-4-5")
	assert.Equal(t, 17, a.OutputTokens, "OutputTokens = %d, want 17")
	assert.Equal(t, 42, a.ContextTokens, "ContextTokens = %d, want 42")
	assert.False(t, len(a.TokenUsage) == 0, "TokenUsage empty, want normalized JSON")
	assert.Equal(t, 17, sess.TotalOutputTokens, "TotalOutputTokens")
}

func TestParseOpenClawSession_PartialUsage(t *testing.T) {
	// Partial usage block: only output is present in the source.
	// applyOpenClawAssistantUsage normalizes to a 4-key JSON, but
	// HasContextTokens must still be false. TokenPresence() must
	// trust the parser's explicit flags rather than inferring from
	// the always-populated normalized keys.
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"partial","timestamp":"2026-04-30T12:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","timestamp":"2026-04-30T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":"2026-04-30T12:00:01Z"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-30T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"reply"}],"timestamp":"2026-04-30T12:00:02Z","provider":"anthropic","model":"claude-haiku-4-5","usage":{"output":17}}}`,
	)

	_, msgs, err := parseOpenClawSessionForTest(t, path, "", "test")
	require.NoError(t, err)
	require.Equal(t, 2, len(msgs), "expected 2 messages, got %d")

	a := msgs[1]
	assert.False(t, a.HasContextTokens, "HasContextTokens = true, want false")
	assert.True(t, a.HasOutputTokens, "HasOutputTokens = false, want true")

	hasCtx, hasOut := a.TokenPresence()
	assert.False(t, hasCtx,
		"TokenPresence ctx = true, want false (parser flags must take precedence over JSON keys)")
	assert.True(t, hasOut, "TokenPresence out = false, want true")
}

func TestParseOpenClawSession_NoUsage(t *testing.T) {
	// Assistant turn without any usage block: the parser is still
	// authoritative — both presence flags must be false and stick.
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"nousage","timestamp":"2026-04-30T12:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","timestamp":"2026-04-30T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":"2026-04-30T12:00:01Z"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-04-30T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"reply"}],"timestamp":"2026-04-30T12:00:02Z"}}`,
	)

	_, msgs, err := parseOpenClawSessionForTest(t, path, "", "test")
	require.NoError(t, err)
	require.Equal(t, 2, len(msgs), "expected 2 messages, got %d")

	hasCtx, hasOut := msgs[1].TokenPresence()
	assert.False(t, hasCtx, "TokenPresence ctx")
	assert.False(t, hasOut, "TokenPresence out")
}

func TestParseOpenClawSession_Compaction(t *testing.T) {
	path, _ := writeOpenClawTestFile(t, "main",
		`{"type":"session","version":3,"id":"compact","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"compaction","id":"c1","timestamp":"2026-02-25T10:00:01Z","summary":"Previous work summary"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:02Z","message":{"role":"user","content":[{"type":"text","text":"Continue from here"}],"timestamp":"2026-02-25T10:00:02Z"}}`,
		`{"type":"message","id":"m2","timestamp":"2026-02-25T10:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"Continuing..."}],"timestamp":"2026-02-25T10:00:03Z"}}`,
	)

	sess, msgs, err := parseOpenClawSessionForTest(t, path, "", "test")
	require.NoError(t, err)
	require.False(t, sess == nil, "expected session, got nil")
	// Compaction should be skipped, only messages remain.
	assert.Equal(t, 2, len(msgs), "expected 2 messages (compaction skipped), got %d")
}

func TestParseOpenClawSession_AgentIDInSessionID(t *testing.T) {
	// Verify different agent subdirectories produce distinct
	// session IDs even when the raw session ID is the same.
	pathA, _ := writeOpenClawTestFile(t, "alpha",
		`{"type":"session","version":3,"id":"same-id","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Hello"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
	)
	pathB, _ := writeOpenClawTestFile(t, "beta",
		`{"type":"session","version":3,"id":"same-id","timestamp":"2026-02-25T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","timestamp":"2026-02-25T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Hello"}],"timestamp":"2026-02-25T10:00:01Z"}}`,
	)

	sessA, _, err := parseOpenClawSessionForTest(t, pathA, "", "test")
	require.NoError(t, err)
	sessB, _, err := parseOpenClawSessionForTest(t, pathB, "", "test")
	require.NoError(t, err)

	assert.NotEqualf(t, sessB.ID, sessA.ID,
		"expected different session IDs for different agents, both got %s", sessA.ID)
	assert.Equal(t, "openclaw:alpha:same-id", sessA.ID, "expected openclaw:alpha:same-id, got %s")
	assert.Equal(t, "openclaw:beta:same-id", sessB.ID, "expected openclaw:beta:same-id, got %s")
}

func TestIsOpenClawSessionFile(t *testing.T) {
	accepted := []string{
		"abc.jsonl",
		"abc.jsonl.deleted.2026-02-19T08-59-24.951Z",
		"abc.jsonl.reset.2026-02-17T09-39-39.691Z",
		"abc.jsonl.full.bak",
	}
	rejected := []string{
		"abc.jsonl.tmp",
		"abc.jsonl.lock",
		"abc.jsonl.partial",
		"abc.json",
		"sessions.json",
	}
	for _, name := range accepted {
		assert.Truef(t, IsOpenClawSessionFile(name),
			"expected %q to be accepted", name)
	}
	for _, name := range rejected {
		assert.Falsef(t, IsOpenClawSessionFile(name),
			"expected %q to be rejected", name)
	}
}

func TestBestOpenClawEntry_CrossSuffix(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0755))

	// reset is newer (March) than deleted (January), even though
	// "deleted" > "reset" would be wrong lexicographically within
	// the suffix family.
	older := "abc.jsonl.deleted.2026-01-15T00-00-00.000Z"
	newer := "abc.jsonl.reset.2026-03-01T00-00-00.000Z"
	for _, name := range []string{older, newer} {
		require.NoError(t, os.WriteFile(
			filepath.Join(sessDir, name), []byte("{}"), 0644,
		))
	}

	files := discoverOpenClawSessionsForTest(t, root)
	require.Equal(t, 1, len(files), "expected 1 (deduplicated), got %d")
	assert.Equal(t, newer, filepath.Base(files[0].DisplayPath), "expected %q, got %q")
}

func TestDiscoverOpenClawSessions(t *testing.T) {
	// Build a mock directory structure:
	// <root>/main/sessions/sess1.jsonl
	// <root>/main/sessions/sessions.json
	// <root>/claude/sessions/sess2.jsonl
	root := t.TempDir()

	mainSessions := filepath.Join(root, "main", "sessions")
	require.NoError(t, os.MkdirAll(mainSessions, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(mainSessions, "sess1.jsonl"), []byte("{}"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(mainSessions, "sessions.json"), []byte("{}"), 0644))

	claudeSessions := filepath.Join(root, "claude", "sessions")
	require.NoError(t, os.MkdirAll(claudeSessions, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(claudeSessions, "sess2.jsonl"), []byte("{}"), 0644))

	files := discoverOpenClawSessionsForTest(t, root)
	require.Equal(t, 2, len(files), "expected 2 session files, got %d")
	for _, f := range files {
		assert.Equal(t, AgentOpenClaw, f.Provider, "expected agent openclaw, got %s")
	}
}

func TestDiscoverOpenClawSessions_DeduplicatesArchived(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0755))

	// Active file and two archived files for the same session.
	for _, name := range []string{
		"abc.jsonl",
		"abc.jsonl.deleted.2026-02-19T08-59-24.951Z",
		"abc.jsonl.reset.2026-02-17T09-39-39.691Z",
	} {
		require.NoError(t, os.WriteFile(
			filepath.Join(sessDir, name),
			[]byte("{}"), 0644,
		))
	}

	files := discoverOpenClawSessionsForTest(t, root)
	require.Equal(t, 1, len(files), "expected 1 file (deduplicated), got %d")
	// Active file should win.
	assert.Truef(t, strings.HasSuffix(files[0].DisplayPath, "abc.jsonl"),
		"expected active .jsonl to win, got %s",
		filepath.Base(files[0].DisplayPath))
}

func TestDiscoverOpenClawSessions_ArchiveOnlyPicksNewest(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0755))

	// Two archived files, no active — newest filename wins.
	for _, name := range []string{
		"xyz.jsonl.deleted.2026-01-01T00-00-00.000Z",
		"xyz.jsonl.deleted.2026-03-01T00-00-00.000Z",
	} {
		require.NoError(t, os.WriteFile(
			filepath.Join(sessDir, name),
			[]byte("{}"), 0644,
		))
	}

	files := discoverOpenClawSessionsForTest(t, root)
	require.Equal(t, 1, len(files), "expected 1 file (deduplicated), got %d")
	want := "xyz.jsonl.deleted.2026-03-01T00-00-00.000Z"
	assert.Equal(t, want, filepath.Base(files[0].DisplayPath), "expected newest archive")
}

func TestDiscoverOpenClawSessions_DifferentSessionsNotDeduped(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0755))

	// Two different session IDs — should not be deduplicated.
	for _, name := range []string{
		"aaa.jsonl",
		"bbb.jsonl.deleted.2026-01-01T00-00-00.000Z",
	} {
		require.NoError(t, os.WriteFile(
			filepath.Join(sessDir, name),
			[]byte("{}"), 0644,
		))
	}

	files := discoverOpenClawSessionsForTest(t, root)
	require.Len(t, files, 2, "expected 2 files (different sessions)")
}

func TestFindOpenClawSourceFile(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0755))
	target := filepath.Join(sessDir, "abc-123.jsonl")
	require.NoError(t, os.WriteFile(target, []byte("{}"), 0644))

	// Raw ID is now "agentId:sessionId".
	found := findOpenClawSourceForTest(t, root, "main:abc-123")
	assert.Equal(t, target, found, "expected %s, got %s")

	// Non-existent session.
	notFound := findOpenClawSourceForTest(t, root, "main:nonexistent")
	assert.Equal(t, "", notFound, "expected empty string, got %s")

	// Non-existent agent.
	notFound2 := findOpenClawSourceForTest(t, root, "other:abc-123")
	assert.Equal(t, "", notFound2, "expected empty string, got %s")

	// Invalid format (no colon separator).
	notFound3 := findOpenClawSourceForTest(t, root, "abc-123")
	assert.Equal(t, "", notFound3, "expected empty string for bare ID, got %s")
}

func TestFindOpenClawSourceFile_ArchiveOnly(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0755))

	// Only archived files exist — no active .jsonl.
	archived := "def-456.jsonl.deleted.2026-02-19T08-59-24.951Z"
	require.NoError(t, os.WriteFile(
		filepath.Join(sessDir, archived),
		[]byte("{}"), 0644,
	))

	found := findOpenClawSourceForTest(t, root, "main:def-456")
	want := filepath.Join(sessDir, archived)
	assert.Equal(t, want, found, "expected %s, got %s")
}

func TestFindOpenClawSourceFile_PrefersActiveOverArchive(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0755))

	// Both active and archived files exist.
	active := filepath.Join(sessDir, "ghi-789.jsonl")
	require.NoError(t, os.WriteFile(active, []byte("{}"), 0644))
	archived := "ghi-789.jsonl.deleted.2026-02-19T00-00-00.000Z"
	require.NoError(t, os.WriteFile(
		filepath.Join(sessDir, archived),
		[]byte("{}"), 0644,
	))

	found := findOpenClawSourceForTest(t, root, "main:ghi-789")
	assert.Equal(t, active, found, "expected active file %s, got %s")
}

func TestFindOpenClawSourceFile_ArchiveOnlyNewest(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "main", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0755))

	// Two archived files — newest should be chosen.
	old := "jkl.jsonl.deleted.2026-01-01T00-00-00.000Z"
	newest := "jkl.jsonl.deleted.2026-03-01T00-00-00.000Z"
	for _, name := range []string{old, newest} {
		require.NoError(t, os.WriteFile(
			filepath.Join(sessDir, name),
			[]byte("{}"), 0644,
		))
	}

	found := findOpenClawSourceForTest(t, root, "main:jkl")
	want := filepath.Join(sessDir, newest)
	assert.Equal(t, want, found, "expected newest archive %s, got %s")
}
