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
	tb testing.TB,
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	tb.Helper()
	return parseWorkBuddySession(path, project, machine)
}

func discoverWorkBuddyTestSessions(tb testing.TB, root string) []DiscoveredFile {
	tb.Helper()
	provider, ok := NewProvider(AgentWorkBuddy, ProviderConfig{Roots: []string{root}})
	require.True(tb, ok)
	sources, err := provider.Discover(tb.Context())
	require.NoError(tb, err)

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

func findWorkBuddyTestSourceFile(tb testing.TB, root, rawID string) string {
	tb.Helper()
	provider, ok := NewProvider(AgentWorkBuddy, ProviderConfig{Roots: []string{root}})
	require.True(tb, ok)
	source, found, err := provider.FindSource(
		tb.Context(),
		FindSourceRequest{RawSessionID: rawID},
	)
	require.NoError(tb, err)
	if !found {
		return ""
	}
	return source.DisplayPath
}

func TestDiscoverWorkBuddySessions(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	subPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	toolPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "tool-results", "tool_123.txt")
	for _, path := range []string{mainPath, subPath, toolPath} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "MkdirAll(%q)", path)
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644), "WriteFile(%q)", path)
	}

	files := discoverWorkBuddyTestSessions(t, root)
	require.Len(t, files, 2)
	assert.Equal(t, mainPath, files[0].Path)
	assert.Equal(t, "proj", files[0].Project)
	assert.Equal(t, AgentWorkBuddy, files[0].Agent)
	assert.Equal(t, subPath, files[1].Path)
	assert.Equal(t, "proj", files[1].Project)
	assert.Equal(t, AgentWorkBuddy, files[1].Agent)
}

func TestParseWorkBuddySession(t *testing.T) {
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "cwd", "proj")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	path := filepath.Join(tmp, "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := fmt.Sprintf(`{"id":"u1","timestamp":1778749186168,"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}],"sessionId":"11111111-1111-4111-8111-111111111111","cwd":%q}
{"id":"a1","timestamp":1778749187168,"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"sessionId":"11111111-1111-4111-8111-111111111111","providerData":{"model":"gpt-5.5","usage":{"inputTokens":20,"outputTokens":4,"cacheReadInputTokens":5}}}
{"id":"fc1","timestamp":1778749188168,"type":"function_call","name":"Bash","callId":"call_1","arguments":"{\"command\":\"pwd\"}","providerData":{"model":"gpt-5.5","usage":{"inputTokens":10,"outputTokens":3,"cacheReadInputTokens":2}}}
{"id":"fr1","timestamp":1778749189168,"type":"function_call_result","name":"Bash","callId":"call_1","output":{"type":"text","text":%q}}
`, cwd, cwd)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	sess, msgs, err := parseWorkBuddyTestSession(t, path, "proj", "local")
	require.NoError(t, err)
	require.NotNil(t, sess, "session nil")
	assert.Equal(t, "workbuddy:11111111-1111-4111-8111-111111111111", sess.ID)
	assert.Equal(t, "proj", sess.Project)
	assert.Equal(t, cwd, sess.Cwd)
	assert.Equal(t, "hello", sess.FirstMessage)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 7, sess.TotalOutputTokens)
	require.Len(t, msgs, 4)
	assert.Equal(t, int64(20), gjson.GetBytes(msgs[1].TokenUsage, "input_tokens").Int(),
		"assistant message TokenUsage = %s", string(msgs[1].TokenUsage))
	assert.Equal(t, RoleAssistant, msgs[2].Role)
	assert.True(t, msgs[2].HasToolUse)
	require.NotEmpty(t, msgs[2].ToolCalls)
	assert.Equal(t, "Bash", msgs[2].ToolCalls[0].ToolName)
	assert.Equal(t, `{"command":"pwd"}`, msgs[2].ToolCalls[0].InputJSON)
	assert.Equal(t, int64(10), gjson.GetBytes(msgs[2].TokenUsage, "input_tokens").Int(),
		"TokenUsage = %s", string(msgs[2].TokenUsage))
	assert.Equal(t, RoleUser, msgs[3].Role)
	require.NotEmpty(t, msgs[3].ToolResults)
	assert.Equal(t, "call_1", msgs[3].ToolResults[0].ToolUseID)
}

func TestParseWorkBuddySessionAITitle(t *testing.T) {
	t.Run("provider_title", func(t *testing.T) {
		root := t.TempDir()
		sessionID := "11111111-1111-4111-8111-111111111111"
		path := filepath.Join(root, "proj", sessionID+".jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		content := `{"type":"message","role":"user","content":[{"type":"input_text","text":"<system-reminder data-role=\"user-context\">\nSynthetic context\n</system-reminder>\nReview build logs"}]}
{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will review the logs."}]}
{"type":"ai-title","aiTitle":"Review build logs","sessionId":"11111111-1111-4111-8111-111111111111"}
`
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

		provider, ok := NewProvider(AgentWorkBuddy, ProviderConfig{
			Roots:   []string{root},
			Machine: "local",
		})
		require.True(t, ok)
		sources, err := provider.Discover(t.Context())
		require.NoError(t, err)
		require.Len(t, sources, 1)
		fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
		require.NoError(t, err)
		outcome, err := provider.Parse(t.Context(), ParseRequest{
			Source:      sources[0],
			Fingerprint: fingerprint,
		})
		require.NoError(t, err)
		require.True(t, outcome.ResultSetComplete)
		require.Len(t, outcome.Results, 1)

		result := outcome.Results[0].Result
		assert.Equal(t, "Review build logs", result.Session.SessionName)
		assert.Equal(t,
			"<system-reminder data-role=\"user-context\"> Synthetic context </system-reminder> Review build logs",
			result.Session.FirstMessage,
		)
		require.Len(t, result.Messages, 2)
		assert.Equal(t, "<system-reminder data-role=\"user-context\">\nSynthetic context\n</system-reminder>\nReview build logs", result.Messages[0].Content)
		assert.Equal(t, "I will review the logs.", result.Messages[1].Content)
	})

	tests := []struct {
		name          string
		jsonl         string
		wantTitle     string
		wantFirst     string
		wantMalformed int
		wantSession   bool
		wantMessages  int
		wantCwd       string
		wantProject   string
		wantTimestamp int64
	}{
		{
			name: "last_nonblank",
			jsonl: `{"type":"ai-title","aiTitle":"  Before messages  ","timestamp":1778749186168,"cwd":"C:\\Users\\alice\\projects\\title-test"}
{"type":"message","role":"user","content":[{"type":"input_text","text":"fallback"}]}
{"type":"ai-title","aiTitle":"\t \n"}
{"type":"message","role":"assistant","content":[{"type":"output_text","text":"body"}]}
{"type":"ai-title","aiTitle":"  Final title \n"}
{"type":"ai-title","aiTitle":"  "}
`,
			wantTitle:     "Final title",
			wantFirst:     "fallback",
			wantMalformed: 0,
			wantSession:   true,
			wantMessages:  2,
			wantCwd:       "C:\\Users\\alice\\projects\\title-test",
			wantProject:   "title_test",
			wantTimestamp: 1778749186168,
		},
		{
			name: "unusable_values_and_adjacent_records",
			jsonl: `{"type":"message","role":"user","aiTitle":"message title","content":[{"type":"input_text","text":"fallback"}]}
{"type":"ai-title-extra","aiTitle":"wrong"}
{"type":"ai-title","aiTitle":null}
{"type":"ai-title","aiTitle":42}
{"type":"ai-title","aiTitle":{}}
{"type":"ai-title","aiTitle":""}
{"type":"ai-title","aiTitle":" \t "}
{"type":"message","role":"assistant","content":[{"type":"output_text","text":"literal ai-title"}]}
`,
			wantFirst:     "fallback",
			wantMalformed: 0,
			wantSession:   true,
			wantMessages:  2,
		},
		{
			name: "malformed_and_later_title",
			jsonl: `{"type":"message","role":"user","content":[{"type":"input_text","text":"fallback"}]}
not-json
` + string([]byte{0xff}) + `
42
{"type":"ai-title","aiTitle":"Recovered"}
`,
			wantTitle:     "Recovered",
			wantFirst:     "fallback",
			wantMalformed: 2,
			wantSession:   true,
			wantMessages:  1,
		},
		{
			name:        "title_only",
			jsonl:       `{"type":"ai-title","aiTitle":"Only title"}`,
			wantSession: false,
		},
		{
			name:        "empty",
			wantSession: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte(tt.jsonl), 0o644))

			sess, msgs, err := parseWorkBuddyTestSession(t, path, "proj", "local")
			require.NoError(t, err)
			if !tt.wantSession {
				assert.Nil(t, sess)
				assert.Empty(t, msgs)
				return
			}

			require.NotNil(t, sess)
			assert.Equal(t, tt.wantTitle, sess.SessionName)
			assert.Equal(t, tt.wantFirst, sess.FirstMessage)
			assert.Equal(t, tt.wantMalformed, sess.MalformedLines)
			assert.Len(t, msgs, tt.wantMessages)
			if tt.wantCwd != "" {
				assert.Equal(t, tt.wantCwd, sess.Cwd)
			}
			if tt.wantProject != "" {
				assert.Equal(t, tt.wantProject, sess.Project)
			}
			if tt.wantTimestamp != 0 {
				assert.Equal(t, tt.wantTimestamp, sess.StartedAt.UnixMilli())
			}
		})
	}

	t.Run("preserves_transcript", func(t *testing.T) {
		base := `{"type":"message","role":"user","content":[{"type":"input_text","text":"第一行\nsecond line"}],"cwd":"/tmp/cwd-project"}
{"type":"message","role":"assistant","content":[{"type":"output_text","text":"réponse"}],"providerData":{"model":"gpt-5.5","usage":{"inputTokens":20,"outputTokens":4,"cacheReadInputTokens":5}}}
{"type":"function_call","name":"Bash","callId":"call_1","arguments":"{\"command\":\"pwd\"}"}
{"type":"function_call_result","callId":"call_1","output":{"type":"text","text":"結果"}}
`
		root := t.TempDir()
		basePath := filepath.Join(root, "base", "11111111-1111-4111-8111-111111111111.jsonl")
		titledPath := filepath.Join(root, "titled", "11111111-1111-4111-8111-111111111111.jsonl")
		for path, content := range map[string]string{
			basePath:   base,
			titledPath: base + `{"type":"ai-title","aiTitle":"  多言語 review  "}` + "\n",
		} {
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		}

		baseSession, baseMessages, err := parseWorkBuddyTestSession(t, basePath, "proj", "local")
		require.NoError(t, err)
		titledSession, titledMessages, err := parseWorkBuddyTestSession(t, titledPath, "proj", "local")
		require.NoError(t, err)
		require.NotNil(t, baseSession)
		require.NotNil(t, titledSession)
		assert.Empty(t, baseSession.SessionName)
		assert.Equal(t, "多言語 review", titledSession.SessionName)
		assert.Equal(t, baseSession.FirstMessage, titledSession.FirstMessage)
		assert.Equal(t, baseSession.StartedAt, titledSession.StartedAt)
		assert.Equal(t, baseSession.EndedAt, titledSession.EndedAt)
		assert.Equal(t, baseSession.MessageCount, titledSession.MessageCount)
		assert.Equal(t, baseSession.UserMessageCount, titledSession.UserMessageCount)
		assert.Equal(t, baseMessages, titledMessages)
	})

	t.Run("missing_file", func(t *testing.T) {
		_, _, err := parseWorkBuddyTestSession(
			t,
			filepath.Join(t.TempDir(), "missing.jsonl"),
			"proj",
			"local",
		)
		require.Error(t, err)
		assert.ErrorContains(t, err, "stat")
	})
}

func TestParseWorkBuddySessionAITitleReparse(t *testing.T) {
	root := t.TempDir()
	sessionID := "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(root, "proj", sessionID+".jsonl")
	writeSourceFile(t, path, workBuddyProviderFixture("hello"))

	provider, ok := NewProvider(AgentWorkBuddy, ProviderConfig{
		Roots:   []string{root},
		Machine: "local",
	})
	require.True(t, ok)
	parse := func() ParseResult {
		sources, err := provider.Discover(t.Context())
		require.NoError(t, err)
		require.Len(t, sources, 1)
		fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
		require.NoError(t, err)
		outcome, err := provider.Parse(t.Context(), ParseRequest{
			Source:      sources[0],
			Fingerprint: fingerprint,
		})
		require.NoError(t, err)
		require.True(t, outcome.ResultSetComplete)
		require.Len(t, outcome.Results, 1)
		return outcome.Results[0].Result
	}

	initial := parse()
	assert.Empty(t, initial.Session.SessionName)
	assert.Len(t, initial.Messages, 3)

	withTitle := workBuddyProviderFixture("hello") +
		`{"type":"ai-title","aiTitle":"  Appended title  "}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(withTitle), 0o644))
	appended := parse()
	assert.Equal(t, "Appended title", appended.Session.SessionName)
	assert.Len(t, appended.Messages, 3)

	require.NoError(t, os.WriteFile(path, []byte(workBuddyProviderFixture("replacement")), 0o644))
	cleared := parse()
	assert.Empty(t, cleared.Session.SessionName)
	assert.Equal(t, "replacement", cleared.Session.FirstMessage)
	assert.Len(t, cleared.Messages, 3)
}

func TestParseWorkBuddySessionDoesNotDoubleCountOpenAICachedTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"id":"a1","timestamp":1778749187168,"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"sessionId":"11111111-1111-4111-8111-111111111111","providerData":{"model":"gpt-5.5","rawUsage":{"prompt_tokens":20,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":5}}}}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	sess, msgs, err := parseWorkBuddyTestSession(t, path, "proj", "local")
	require.NoError(t, err)
	require.NotNil(t, sess, "session nil")
	require.Len(t, msgs, 1)
	assert.True(t, msgs[0].HasContextTokens)
	assert.Equal(t, 20, msgs[0].ContextTokens)
	assert.Equal(t, int64(15), gjson.GetBytes(msgs[0].TokenUsage, "input_tokens").Int(),
		"input_tokens; usage=%s", string(msgs[0].TokenUsage))
	assert.Equal(t, int64(5), gjson.GetBytes(msgs[0].TokenUsage, "cache_read_input_tokens").Int(),
		"cache_read_input_tokens; usage=%s", string(msgs[0].TokenUsage))
}

func TestParseWorkBuddySessionUsesCwdProjectAndFileSessionID(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "stored-project", "22222222-2222-4222-8222-222222222222.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	// A non-git working directory under a controlled temp root so
	// ExtractProjectFromCwd falls through to the basename and applies
	// NormalizeName (hyphen -> underscore) deterministically.
	cwd := filepath.Join(tmp, "code", "cwd-project")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	content := fmt.Sprintf(`{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"hello"}],"sessionId":"11111111-1111-4111-8111-111111111111","cwd":%q}
`, cwd)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	sess, _, err := parseWorkBuddyTestSession(t, path, "stored-project", "local")
	require.NoError(t, err)
	assert.Equal(t, "workbuddy:22222222-2222-4222-8222-222222222222", sess.ID)
	assert.Equal(t, "cwd_project", sess.Project)
}

func TestParseWorkBuddySessionNormalizesWindowsCwdProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stored", "33333333-3333-4333-8333-333333333333.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"hi"}],"sessionId":"33333333-3333-4333-8333-333333333333","cwd":"C:\\Users\\alice\\projects\\report-builder"}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	sess, _, err := parseWorkBuddyTestSession(t, path, "stored", "local")
	require.NoError(t, err)
	assert.Equal(t, "report_builder", sess.Project)
}

func TestParseWorkBuddySessionFallsBackToDiscoveredProjectWhenCwdHasNoProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discovered-proj", "44444444-4444-4444-8444-444444444444.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"hi"}],"sessionId":"44444444-4444-4444-8444-444444444444","cwd":"/"}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	sess, _, err := parseWorkBuddyTestSession(t, path, "discovered-proj", "local")
	require.NoError(t, err)
	assert.Equal(t, "discovered-proj", sess.Project)
}

func TestParseWorkBuddySessionOmitsAbsentTokenUsageKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"id":"a1","timestamp":1778749187168,"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"sessionId":"11111111-1111-4111-8111-111111111111","providerData":{"model":"gpt-5.5","usage":{"inputTokens":20}}}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	_, msgs, err := parseWorkBuddyTestSession(t, path, "proj", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	m := msgs[0]
	assert.False(t, m.HasOutputTokens, "HasOutputTokens; usage=%s", string(m.TokenUsage))
	assert.False(t, gjson.GetBytes(m.TokenUsage, "output_tokens").Exists(),
		"output_tokens key present; usage=%s", string(m.TokenUsage))
	assert.Equal(t, int64(20), gjson.GetBytes(m.TokenUsage, "input_tokens").Int(),
		"input_tokens; usage=%s", string(m.TokenUsage))
	// The DB coverage backfill re-derives presence from JSON keys, so
	// an absent output field must not be inferred as output coverage.
	_, hasOutput := InferTokenPresence(m.TokenUsage, m.ContextTokens, m.OutputTokens, false, false)
	assert.False(t, hasOutput, "InferTokenPresence inferred output coverage for input-only usage; usage=%s", string(m.TokenUsage))
}

func TestParseWorkBuddySubagentSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"sub task"}],"cwd":"/tmp/cwd-project"}
{"type":"ai-title","aiTitle":"  Subagent title  ","sessionId":"99999999-9999-4999-8999-999999999999"}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	sess, _, err := parseWorkBuddyTestSession(t, path, "proj", "local")
	require.NoError(t, err)
	assert.Equal(t, "workbuddy:11111111-1111-4111-8111-111111111111:subagent:agent-123", sess.ID)
	assert.Equal(t, "workbuddy:11111111-1111-4111-8111-111111111111", sess.ParentSessionID)
	assert.Equal(t, RelSubagent, sess.RelationshipType)
	assert.Equal(t, "Subagent title", sess.SessionName)
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
	path := filepath.Join(t.TempDir(), "subagents", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"timestamp":1778749186168,"type":"message","role":"user","content":[{"text":"hello"}]}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	sess, _, err := parseWorkBuddyTestSession(t, path, "subagents", "local")
	require.NoError(t, err)
	assert.Equal(t, "workbuddy:11111111-1111-4111-8111-111111111111", sess.ID)
	assert.Empty(t, sess.ParentSessionID)
	assert.Equal(t, RelNone, sess.RelationshipType)
}

func TestParseWorkBuddySessionDecodesObjectToolResultText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"id":"fr1","timestamp":1778749189168,"type":"function_call_result","name":"Bash","callId":"call_1","output":{"type":"text","text":"/tmp/proj"}}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	_, msgs, err := parseWorkBuddyTestSession(t, path, "proj", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].ToolResults, 1)
	assert.Equal(t, "/tmp/proj", DecodeContent(msgs[0].ToolResults[0].ContentRaw),
		"ContentRaw=%s", msgs[0].ToolResults[0].ContentRaw)
}
