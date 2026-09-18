package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func parseWorkBuddyTestSession(
	t testing.TB,
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()
	return parseWorkBuddySession(path, project, machine)
}

func discoverWorkBuddyTestSessions(t testing.TB, root string) []DiscoveredFile {
	t.Helper()
	provider, ok := NewProvider(AgentWorkBuddy, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)

	files := make([]DiscoveredFile, 0, len(sources))
	for _, source := range sources {
		files = append(files, DiscoveredFile{
			Path:    source.DisplayPath,
			Project: source.ProjectHint,
			Agent:   source.Provider,
		})
	}
	return files
}

func findWorkBuddyTestSourceFile(t testing.TB, root, rawID string) string {
	t.Helper()
	provider, ok := NewProvider(AgentWorkBuddy, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(
		t.Context(),
		FindSourceRequest{RawSessionID: rawID},
	)
	require.NoError(t, err)
	if !found {
		return ""
	}
	return source.DisplayPath
}

func TestDiscoverWorkBuddySessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	mainPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	subPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	toolPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "tool-results", "tool_123.txt")
	for _, path := range []string{mainPath, subPath, toolPath} {
		require.NoError(os.MkdirAll(filepath.Dir(path), 0o755), "MkdirAll(%q)", path)
		require.NoError(os.WriteFile(path, []byte("{}\n"), 0o644), "WriteFile(%q)", path)
	}

	files := discoverWorkBuddyTestSessions(t, root)
	require.Len(files, 2)
	assert.Equal(mainPath, files[0].Path)
	assert.Equal("proj", files[0].Project)
	assert.Equal(AgentWorkBuddy, files[0].Agent)
	assert.Equal(subPath, files[1].Path)
	assert.Equal("proj", files[1].Project)
	assert.Equal(AgentWorkBuddy, files[1].Agent)
}

func TestParseWorkBuddySession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "cwd", "proj")
	require.NoError(os.MkdirAll(cwd, 0o755))
	path := filepath.Join(tmp, "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	content := fmt.Sprintf(`{"id":"u1","timestamp":1778749186168,"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}],"sessionId":"11111111-1111-4111-8111-111111111111","cwd":%q}
{"id":"a1","timestamp":1778749187168,"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"sessionId":"11111111-1111-4111-8111-111111111111","providerData":{"model":"gpt-5.5","usage":{"inputTokens":20,"outputTokens":4,"cacheReadInputTokens":5}}}
{"id":"fc1","timestamp":1778749188168,"type":"function_call","name":"Bash","callId":"call_1","arguments":"{\"command\":\"pwd\"}","providerData":{"model":"gpt-5.5","usage":{"inputTokens":10,"outputTokens":3,"cacheReadInputTokens":2}}}
{"id":"fr1","timestamp":1778749189168,"type":"function_call_result","name":"Bash","callId":"call_1","output":{"type":"text","text":%q}}
`, cwd, cwd)
	require.NoError(os.WriteFile(path, []byte(content), 0o644))

	sess, msgs, err := parseWorkBuddyTestSession(t, path, "proj", "local")
	require.NoError(err)
	require.NotNil(sess, "session nil")
	assert.Equal("workbuddy:11111111-1111-4111-8111-111111111111", sess.ID)
	assert.Equal("proj", sess.Project)
	assert.Equal(cwd, sess.Cwd)
	assert.Equal("hello", sess.FirstMessage)
	assert.Equal(1, sess.UserMessageCount)
	assert.True(sess.HasTotalOutputTokens)
	assert.Equal(7, sess.TotalOutputTokens)
	require.Len(msgs, 4)
	assert.Equal(int64(20), gjson.GetBytes(msgs[1].TokenUsage, "input_tokens").Int(),
		"assistant message TokenUsage = %s", string(msgs[1].TokenUsage))
	assert.Equal(RoleAssistant, msgs[2].Role)
	assert.True(msgs[2].HasToolUse)
	require.NotEmpty(msgs[2].ToolCalls)
	assert.Equal("Bash", msgs[2].ToolCalls[0].ToolName)
	assert.JSONEq(`{"command":"pwd"}`, msgs[2].ToolCalls[0].InputJSON)
	assert.Equal(int64(10), gjson.GetBytes(msgs[2].TokenUsage, "input_tokens").Int(),
		"TokenUsage = %s", string(msgs[2].TokenUsage))
	assert.Equal(RoleUser, msgs[3].Role)
	require.NotEmpty(msgs[3].ToolResults)
	assert.Equal("call_1", msgs[3].ToolResults[0].ToolUseID)
}

func TestParseWorkBuddySessionDoesNotDoubleCountOpenAICachedTokens(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"id":"a1","timestamp":1778749187168,"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"sessionId":"11111111-1111-4111-8111-111111111111","providerData":{"model":"gpt-5.5","rawUsage":{"prompt_tokens":20,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":5}}}}
`
	require.NoError(os.WriteFile(path, []byte(content), 0o644))

	sess, msgs, err := parseWorkBuddyTestSession(t, path, "proj", "local")
	require.NoError(err)
	require.NotNil(sess, "session nil")
	require.Len(msgs, 1)
	assert.True(msgs[0].HasContextTokens)
	assert.Equal(20, msgs[0].ContextTokens)
	assert.Equal(int64(15), gjson.GetBytes(msgs[0].TokenUsage, "input_tokens").Int(),
		"input_tokens; usage=%s", string(msgs[0].TokenUsage))
	assert.Equal(int64(5), gjson.GetBytes(msgs[0].TokenUsage, "cache_read_input_tokens").Int(),
		"cache_read_input_tokens; usage=%s", string(msgs[0].TokenUsage))
}

func TestParseWorkBuddySessionUsesCwdProjectAndFileSessionID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tmp := t.TempDir()
	path := filepath.Join(tmp, "stored-project", "22222222-2222-4222-8222-222222222222.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	// A non-git working directory under a controlled temp root so
	// ExtractProjectFromCwd falls through to the basename and applies
	// NormalizeName (hyphen -> underscore) deterministically.
	cwd := filepath.Join(tmp, "code", "cwd-project")
	require.NoError(os.MkdirAll(cwd, 0o755))
	content := fmt.Sprintf(`{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"hello"}],"sessionId":"11111111-1111-4111-8111-111111111111","cwd":%q}
`, cwd)
	require.NoError(os.WriteFile(path, []byte(content), 0o644))

	sess, _, err := parseWorkBuddyTestSession(t, path, "stored-project", "local")
	require.NoError(err)
	assert.Equal("workbuddy:22222222-2222-4222-8222-222222222222", sess.ID)
	assert.Equal("cwd_project", sess.Project)
}

func TestParseWorkBuddySessionNormalizesWindowsCwdProject(t *testing.T) {
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "stored", "33333333-3333-4333-8333-333333333333.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"hi"}],"sessionId":"33333333-3333-4333-8333-333333333333","cwd":"C:\\Users\\alice\\projects\\report-builder"}
`
	require.NoError(os.WriteFile(path, []byte(content), 0o644))

	sess, _, err := parseWorkBuddyTestSession(t, path, "stored", "local")
	require.NoError(err)
	assert.Equal(t, "report_builder", sess.Project)
}

func TestParseWorkBuddySessionFallsBackToDiscoveredProjectWhenCwdHasNoProject(t *testing.T) {
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "discovered-proj", "44444444-4444-4444-8444-444444444444.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"hi"}],"sessionId":"44444444-4444-4444-8444-444444444444","cwd":"/"}
`
	require.NoError(os.WriteFile(path, []byte(content), 0o644))

	sess, _, err := parseWorkBuddyTestSession(t, path, "discovered-proj", "local")
	require.NoError(err)
	assert.Equal(t, "discovered-proj", sess.Project)
}

func TestParseWorkBuddySessionOmitsAbsentTokenUsageKeys(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"id":"a1","timestamp":1778749187168,"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"sessionId":"11111111-1111-4111-8111-111111111111","providerData":{"model":"gpt-5.5","usage":{"inputTokens":20}}}
`
	require.NoError(os.WriteFile(path, []byte(content), 0o644))

	_, msgs, err := parseWorkBuddyTestSession(t, path, "proj", "local")
	require.NoError(err)
	require.Len(msgs, 1)
	m := msgs[0]
	assert.False(m.HasOutputTokens, "HasOutputTokens; usage=%s", string(m.TokenUsage))
	assert.False(gjson.GetBytes(m.TokenUsage, "output_tokens").Exists(),
		"output_tokens key present; usage=%s", string(m.TokenUsage))
	assert.Equal(int64(20), gjson.GetBytes(m.TokenUsage, "input_tokens").Int(),
		"input_tokens; usage=%s", string(m.TokenUsage))
	// The DB coverage backfill re-derives presence from JSON keys, so
	// an absent output field must not be inferred as output coverage.
	_, hasOutput := InferTokenPresence(m.TokenUsage, m.ContextTokens, m.OutputTokens, false, false)
	assert.False(hasOutput, "InferTokenPresence inferred output coverage for input-only usage; usage=%s", string(m.TokenUsage))
}

func TestParseWorkBuddySubagentSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"sub task"}],"cwd":"/tmp/cwd-project"}
`
	require.NoError(os.WriteFile(path, []byte(content), 0o644))

	sess, _, err := parseWorkBuddyTestSession(t, path, "proj", "local")
	require.NoError(err)
	assert.Equal("workbuddy:11111111-1111-4111-8111-111111111111:subagent:agent-123", sess.ID)
	assert.Equal("workbuddy:11111111-1111-4111-8111-111111111111", sess.ParentSessionID)
	assert.Equal(RelSubagent, sess.RelationshipType)
}

func TestFindWorkBuddySourceFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))
	got := findWorkBuddyTestSourceFile(t, root, "workbuddy:11111111-1111-4111-8111-111111111111")
	assert.Equal(t, path, got)
}

func TestFindWorkBuddySourceFileRejectsInvalidSubagentID(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))

	got := findWorkBuddyTestSourceFile(t, root, "workbuddy:11111111-1111-4111-8111-111111111111:subagent:../agent-123")
	assert.Empty(t, got, "want empty path")
}

func TestParseWorkBuddyProjectNamedSubagentsIsNotSubagent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "subagents", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"hello"}]}
`
	require.NoError(os.WriteFile(path, []byte(content), 0o644))

	sess, _, err := parseWorkBuddyTestSession(t, path, "subagents", "local")
	require.NoError(err)
	assert.Equal("workbuddy:11111111-1111-4111-8111-111111111111", sess.ID)
	assert.Empty(sess.ParentSessionID)
	assert.Equal(RelNone, sess.RelationshipType)
}

func TestParseWorkBuddySessionDecodesObjectToolResultText(t *testing.T) {
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"id":"fr1","timestamp":1778749189168,"type":"function_call_result","name":"Bash","callId":"call_1","output":{"type":"text","text":"/tmp/proj"}}
`
	require.NoError(os.WriteFile(path, []byte(content), 0o644))

	_, msgs, err := parseWorkBuddyTestSession(t, path, "proj", "local")
	require.NoError(err)
	require.Len(msgs, 1)
	require.Len(msgs[0].ToolResults, 1)
	assert.Equal(t, "/tmp/proj", DecodeContent(msgs[0].ToolResults[0].ContentRaw),
		"ContentRaw=%s", msgs[0].ToolResults[0].ContentRaw)
}
