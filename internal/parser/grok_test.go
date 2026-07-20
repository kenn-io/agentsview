package parser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeGrokFixtureFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

func grokSummaryPath(root, cwdKey, sessionID string) string {
	return filepath.Join(root, cwdKey, sessionID, "summary.json")
}

func newGrokTestProvider(t *testing.T, root string) Provider {
	t.Helper()
	provider, ok := NewProvider(AgentGrok, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	return provider
}

func parseGrokGolden(t *testing.T, generation string) ParseResult {
	t.Helper()
	root := t.TempDir()
	fixtureRoot := filepath.Join("testdata", "grok-build", generation)
	require.NoError(t, os.CopyFS(root, os.DirFS(fixtureRoot)))
	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	return outcome.Results[0].Result
}

func TestGrokProviderGoldenCurrentPrefersGeneratedTitle(t *testing.T) {
	result := parseGrokGolden(t, "current")
	assert.Equal(t, "Audit Grok compatibility", result.Session.SessionName)
}

func TestGrokProviderGoldenCurrentTranscriptSemantics(t *testing.T) {
	result := parseGrokGolden(t, "current")
	assert.Equal(t, "Review parser compatibility", result.Session.FirstMessage)
	assert.Equal(t, 2, result.Session.UserMessageCount)
	require.Len(t, result.Messages, 6)
	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, "Review parser compatibility", result.Messages[0].Content)
	assert.Equal(t, RoleAssistant, result.Messages[1].Role)
	assert.Contains(t, result.Messages[1].Content, "Grok Build persistence")
	require.Len(t, result.Messages[1].ToolCalls, 1)
	assert.Equal(t, "ws_1", result.Messages[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "web_search", result.Messages[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Inspect both formats", result.Messages[2].ThinkingText)
	assert.Equal(t, RoleUser, result.Messages[4].Role)
	assert.Equal(t, "also keep interjections", result.Messages[4].Content)
}

func TestGrokProviderGoldenCurrentMetadata(t *testing.T) {
	result := parseGrokGolden(t, "current")
	session := result.Session
	assert.Equal(t, "/workspace/grok-worktrees/parser-audit", session.Cwd)
	assert.Equal(t, "agentsview", session.Project)
	assert.Equal(
		t,
		"grok:019f5000-0000-7000-8000-000000000000",
		session.ParentSessionID,
	)
	assert.Equal(t, RelFork, session.RelationshipType)
	assert.False(t, session.HasPeakContextTokens)
	assert.Zero(t, session.PeakContextTokens)
}

func TestGrokProviderGoldenLegacyTranscript(t *testing.T) {
	result := parseGrokGolden(t, "legacy")
	assert.Equal(t, TranscriptFidelityFull, result.Session.TranscriptFidelity)
	assert.Equal(t, "Review parser compatibility", result.Session.FirstMessage)
	require.Len(t, result.Messages, 4)
	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, "Review parser compatibility", result.Messages[0].Content)
	require.Len(t, result.Messages[1].ToolCalls, 1)
	assert.Equal(t, "call_1", result.Messages[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "grok-4.5", result.Messages[1].Model)
	assert.Equal(t, "Check the old format", result.Messages[1].ThinkingText)
	require.Len(t, result.Messages[2].ToolResults, 1)
	assert.Equal(t, "call_1", result.Messages[2].ToolResults[0].ToolUseID)
}

func TestGrokProviderParsesMixedTranscriptFormats(t *testing.T) {
	root := t.TempDir()
	sessionID := "mixed-formats"
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", sessionID), `{
		"info":{"id":"mixed-formats","cwd":"/workspace/agentsview"},
		"session_summary":"mixed",
		"created_at":"2026-07-18T10:00:00Z",
		"updated_at":"2026-07-18T10:01:00Z"
	}`)
	writeGrokFixtureFile(
		t,
		filepath.Join(root, "cwd-key", sessionID, "chat_history.jsonl"),
		"{\"role\":\"user\",\"content\":\"legacy question\"}\n"+
			"{\"type\":\"assistant\",\"content\":\"current answer\"}\n",
	)
	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(
		context.Background(),
		ParseRequest{Source: sources[0]},
	)
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	messages := outcome.Results[0].Result.Messages
	require.Len(t, messages, 2)
	assert.Equal(t, "legacy question", messages[0].Content)
	assert.Equal(t, "current answer", messages[1].Content)
}

func TestParseGrokChatHistoryUnknownTypeFallsBackToLegacyRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat_history.jsonl")
	writeGrokFixtureFile(
		t,
		path,
		`{"type":"future_metadata","role":"user","content":"legacy question"}`+"\n",
	)

	messages, malformed, err := parseGrokChatHistory(path)
	require.NoError(t, err)
	assert.Zero(t, malformed)
	require.Len(t, messages, 1)
	assert.Equal(t, RoleUser, messages[0].Role)
	assert.Equal(t, "legacy question", messages[0].Content)
}

func TestParseGrokChatHistoryReasoningShapes(t *testing.T) {
	tests := []struct {
		name      string
		history   []string
		wantThink string
		wantCount int
	}{
		{
			name: "standalone content array",
			history: []string{
				`{"type":"reasoning","id":"r1","content":[{"type":"reasoning_text","text":"content thought"}]}`,
				`{"type":"assistant","content":"answer"}`,
			},
			wantThink: "content thought",
			wantCount: 1,
		},
		{
			name: "legacy inline reasoning",
			history: []string{
				`{"type":"assistant","content":"answer","reasoning":{"text":"inline thought"}}`,
			},
			wantThink: "inline thought",
			wantCount: 1,
		},
		{
			name: "legacy raw output reasoning",
			history: []string{
				`{"type":"assistant","content":"answer","raw_output":[{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"raw thought"}]},{"type":"web_search_call","id":"ws_raw","status":"completed","action":{"type":"search","query":"raw query","sources":[]}}]}`,
			},
			wantThink: "raw thought",
			wantCount: 2,
		},
		{
			name: "format zero reasoning content",
			history: []string{
				`{"role":"assistant","content":"answer","reasoning_content":"v0 thought"}`,
			},
			wantThink: "v0 thought",
			wantCount: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "chat_history.jsonl")
			writeGrokFixtureFile(t, path, strings.Join(tt.history, "\n")+"\n")
			messages, malformed, err := parseGrokChatHistory(path)
			require.NoError(t, err)
			assert.Zero(t, malformed)
			require.Len(t, messages, tt.wantCount)
			assistant := messages[len(messages)-1]
			assert.Equal(t, RoleAssistant, assistant.Role)
			assert.Equal(t, tt.wantThink, assistant.ThinkingText)
			assert.True(t, assistant.HasThinking)
			if tt.name == "legacy raw output reasoning" {
				require.Len(t, messages[0].ToolCalls, 1)
				assert.Equal(t, "ws_raw", messages[0].ToolCalls[0].ToolUseID)
				assert.Contains(t, messages[0].Content, "raw query")
			}
		})
	}
}

func TestParseGrokChatHistoryInlineReasoningOverridesPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat_history.jsonl")
	writeGrokFixtureFile(t, path, strings.Join([]string{
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"pending"}]}`,
		`{"type":"assistant","content":"answer","reasoning":{"text":"inline"}}`,
	}, "\n")+"\n")
	messages, malformed, err := parseGrokChatHistory(path)
	require.NoError(t, err)
	assert.Zero(t, malformed)
	require.Len(t, messages, 1)
	assert.Equal(t, "inline", messages[0].ThinkingText)
}

func TestParseGrokChatHistoryDeduplicatesRawBackendToolCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat_history.jsonl")
	writeGrokFixtureFile(t, path, strings.Join([]string{
		`{"type":"backend_tool_call","kind":{"tool_type":"web_search","id":"ws_same","status":"completed","action":{"type":"search","query":"same query","sources":[]}}}`,
		`{"type":"assistant","content":"answer","raw_output":[{"type":"web_search_call","id":"ws_same","status":"completed","action":{"type":"search","query":"same query","sources":[]}}]}`,
	}, "\n")+"\n")
	messages, malformed, err := parseGrokChatHistory(path)
	require.NoError(t, err)
	assert.Zero(t, malformed)
	require.Len(t, messages, 2)
	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, "ws_same", messages[0].ToolCalls[0].ToolUseID)
	assert.Equal(t, "answer", messages[1].Content)
}

func TestParseGrokChatHistoryPreservesCodeInterpreterInput(t *testing.T) {
	code := strings.Repeat("print('counterfactual')\n", 20)
	codeJSON, err := json.Marshal(code)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "chat_history.jsonl")
	writeGrokFixtureFile(
		t,
		path,
		`{"type":"backend_tool_call","kind":{"tool_type":"code_interpreter","code":`+
			string(codeJSON)+
			`,"container_id":"container_counterfactual","id":"ci_counterfactual","outputs":[{"type":"logs","logs":"finished"}],"status":"completed"}}`+"\n",
	)

	messages, malformed, err := parseGrokChatHistory(path)
	require.NoError(t, err)
	assert.Zero(t, malformed)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	var input struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(
		[]byte(messages[0].ToolCalls[0].InputJSON),
		&input,
	))
	assert.Equal(t, code, input.Code)
}

func TestParseGrokChatHistoryDropsOrphanReasoning(t *testing.T) {
	tests := []struct {
		name      string
		trailing  []string
		wantCount int
	}{
		{name: "at eof", wantCount: 0},
		{
			name: "before user",
			trailing: []string{
				`{"type":"user","content":"new question"}`,
				`{"type":"assistant","content":"answer"}`,
			},
			wantCount: 2,
		},
		{
			name: "before tool result",
			trailing: []string{
				`{"type":"tool_result","tool_call_id":"call_1","content":"result"}`,
				`{"type":"assistant","content":"answer"}`,
			},
			wantCount: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := append([]string{
				`{"type":"reasoning","summary":[{"type":"summary_text","text":"orphan"}]}`,
			}, tt.trailing...)
			path := filepath.Join(t.TempDir(), "chat_history.jsonl")
			writeGrokFixtureFile(t, path, strings.Join(rows, "\n")+"\n")
			messages, malformed, err := parseGrokChatHistory(path)
			require.NoError(t, err)
			assert.Zero(t, malformed)
			require.Len(t, messages, tt.wantCount)
			for _, message := range messages {
				assert.False(t, message.HasThinking)
				assert.NotContains(t, message.Content, "orphan")
			}
		})
	}
}

func TestGrokProviderSummarySource(t *testing.T) {
	root := t.TempDir()
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", "sess-1"), `{
			"summary": "Fix parser regression",
			"firstPrompt": "Investigate the failing Grok session import",
			"modelId": "grok-code-fast",
			"createdAt": "2026-07-08T10:00:00Z",
			"updatedAt": "2026-07-08T10:30:00Z",
			"lastActiveAt": "2026-07-08T10:31:00Z",
			"hostname": "devbox",
			"numMessages": 6,
			"worktreeLabel": "agentsview"
		}`)
	writeGrokFixtureFile(t, filepath.Join(root, "cwd-key", "sess-1", "signals.json"), `{
			"tokenUsage": {
				"totalOutputTokens": 321,
				"peakContextTokens": 4096
			}
		}`)
	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	session := outcome.Results[0].Result.Session
	assert.Equal(t, "grok:sess-1", session.ID)
	assert.Equal(t, AgentGrok, session.Agent)
	assert.Equal(t, "sess-1", session.SourceSessionID)
	assert.Equal(t, "grok-summary-v1", session.SourceVersion)
	assert.Equal(t, "summary", session.TranscriptFidelity)
	assert.Equal(t, "Investigate the failing Grok session import", session.FirstMessage)
	assert.Equal(t, "Fix parser regression", session.SessionName)
	assert.Equal(t, "agentsview", session.Project)
	assert.Equal(t, 6, session.MessageCount)
	assert.Equal(t, 1, session.UserMessageCount)
	assert.Equal(t, 321, session.TotalOutputTokens)
	assert.Equal(t, 4096, session.PeakContextTokens)
	assert.Equal(t, filepath.Clean(grokSummaryPath(root, "cwd-key", "sess-1")), filepath.Clean(session.File.Path))
	require.Len(t, outcome.Results[0].Result.Messages, 1)
	assert.Equal(t, RoleUser, outcome.Results[0].Result.Messages[0].Role)
	assert.Equal(t, "Investigate the failing Grok session import", outcome.Results[0].Result.Messages[0].Content)
}

func parseGrokUsageFixture(t *testing.T, updates string) ParseResult {
	t.Helper()
	root := t.TempDir()
	sessionDir := filepath.Join(root, "cwd-key", "sess-1")
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "summary.json"), `{
		"summary":"Usage fixture",
		"firstPrompt":"test usage",
		"modelId":"grok-summary",
		"createdAt":"2026-07-08T10:00:00Z",
		"updatedAt":"2026-07-08T10:30:00Z"
	}`)
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "updates.jsonl"), updates)
	result, err := ParseGrokSummary(
		filepath.Join(sessionDir, "summary.json"), "cwd-key", "local",
	)
	require.NoError(t, err)
	return result
}

func parseGrokUsageFixtureWithSummary(
	t *testing.T, summaryJSON, updates string,
) ParseResult {
	t.Helper()
	root := t.TempDir()
	sessionDir := filepath.Join(root, "cwd-key", "sess-1")
	writeGrokFixtureFile(
		t, filepath.Join(sessionDir, "summary.json"), summaryJSON,
	)
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "updates.jsonl"), updates)
	result, err := ParseGrokSummary(
		filepath.Join(sessionDir, "summary.json"), "cwd-key", "local",
	)
	require.NoError(t, err)
	return result
}

func TestGrokProviderLatestUsageSnapshot(t *testing.T) {
	result := parseGrokUsageFixture(t, strings.Join([]string{
		`{"params":{"update":{"usage":{"inputTokens":100,"outputTokens":20,"cachedReadTokens":10,"reasoningTokens":4}}}}`,
		`{"params":{"update":{"usage":{"inputTokens":8,"outputTokens":3,"cachedReadTokens":13,"reasoningTokens":1}}}}`,
		`not json`,
	}, "\n"))

	require.Len(t, result.UsageEvents, 1)
	event := result.UsageEvents[0]
	assert.Equal(t, 0, event.InputTokens)
	assert.Equal(t, 13, event.CacheReadInputTokens)
	assert.Equal(t, 3, event.OutputTokens)
	assert.Equal(t, 1, event.ReasoningTokens)
	assert.Equal(t, "grok-summary", event.Model)
	assert.Equal(t, "session:grok:sess-1:grok-summary", event.DedupKey)
}

func TestGrokProviderUpdatesWithoutUsageEmitNoEvents(t *testing.T) {
	result := parseGrokUsageFixture(t, strings.Join([]string{
		`{"type":"unrelated"}`,
		`not json`,
	}, "\n"))

	assert.Empty(t, result.UsageEvents)
	assert.Equal(t, "Usage fixture", result.Session.SessionName)
}

func TestGrokProviderRetainsUsageAcrossUnrelatedJSON(t *testing.T) {
	result := parseGrokUsageFixture(t, strings.Join([]string{
		`{"params":{"update":{"usage":{"inputTokens":100,"outputTokens":20,"cachedReadTokens":10,"reasoningTokens":4}}}}`,
		`{"type":"unrelated","payload":{"ok":true}}`,
	}, "\n"))

	require.Len(t, result.UsageEvents, 1)
	event := result.UsageEvents[0]
	assert.Equal(t, 90, event.InputTokens)
	assert.Equal(t, 10, event.CacheReadInputTokens)
	assert.Equal(t, 20, event.OutputTokens)
	assert.Equal(t, 4, event.ReasoningTokens)
	assert.Equal(t, "grok-summary", event.Model)
}

func TestGrokProviderUpdatesUsageByModel(t *testing.T) {
	result := parseGrokUsageFixture(t, `{"params":{"update":{"usage":{"inputTokens":999,"outputTokens":999,"modelUsage":{"grok-a":{"inputTokens":10,"outputTokens":2},"grok-b":{"inputTokens":20,"outputTokens":3}}}}}}`)

	require.Len(t, result.UsageEvents, 2)
	models := map[string]ParsedUsageEvent{}
	for _, event := range result.UsageEvents {
		models[event.Model] = event
	}
	assert.Equal(t, 2, models["grok-a"].OutputTokens)
	assert.Equal(t, 3, models["grok-b"].OutputTokens)
	assert.NotContains(t, models, "grok-summary")
}

func TestGrokProviderUpdatesUsageTopLevelFallback(t *testing.T) {
	result := parseGrokUsageFixture(t, `{"params":{"update":{"usage":{"inputTokens":12,"outputTokens":4,"modelUsage":{}}}}}`)

	require.Len(t, result.UsageEvents, 1)
	assert.Equal(t, "grok-summary", result.UsageEvents[0].Model)
	assert.Equal(t, 12, result.UsageEvents[0].InputTokens)
}

func TestGrokProviderUpdatesUsageTopLevelFallbackWithoutSummaryModel(t *testing.T) {
	result := parseGrokUsageFixtureWithSummary(t, `{
		"summary":"Usage fixture",
		"firstPrompt":"test usage",
		"createdAt":"2026-07-08T10:00:00Z",
		"updatedAt":"2026-07-08T10:30:00Z"
	}`, `{"params":{"update":{"usage":{"inputTokens":12,"outputTokens":4,"modelUsage":{}}}}}`)

	require.Len(t, result.UsageEvents, 1)
	assert.Equal(t, "grok-summary", result.UsageEvents[0].Model)
	assert.Equal(t, 12, result.UsageEvents[0].InputTokens)
}

func TestGrokProviderCurrentBuildSummarySchema(t *testing.T) {
	root := t.TempDir()
	cwdKey := "%2FUsers%2Fdev%2Frepos%2Fwp-devops"
	sessionID := "019f542b-45b0-7720-8184-e790ac116d20"
	writeGrokFixtureFile(t, grokSummaryPath(root, cwdKey, sessionID), `{
			"info": {
				"id": "019f542b-45b0-7720-8184-e790ac116d20",
				"cwd": "/Users/dev/repos/wp-devops"
			},
			"session_summary": "\u5ba1\u67e5\u4ed3\u5e93\u4ee3\u7801",
			"generated_title": "\u5ba1\u67e5\u4ed3\u5e93\u4ee3\u7801",
			"created_at": "2026-07-12T02:32:29.874617Z",
			"updated_at": "2026-07-12T04:29:01.847426Z",
			"last_active_at": "2026-07-12T04:09:52.574304Z",
			"num_messages": 927,
			"num_chat_messages": 104,
			"current_model_id": "grok-4.5",
			"git_root_dir": "/Users/dev/repos/wp-devops/",
			"head_branch": "refactor/shared-proxy-csv-utils",
			"agent_name": "grok-build-plan"
		}`)
	writeGrokFixtureFile(t, filepath.Join(root, cwdKey, sessionID, "signals.json"), `{
			"userMessageCount": 7,
			"assistantMessageCount": 48,
			"contextTokensUsed": 106663,
			"contextWindowTokens": 200000,
			"primaryModelId": "grok-4.5"
		}`)

	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, cwdKey, sources[0].ProjectHint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	session := outcome.Results[0].Result.Session
	assert.Equal(t, "grok:"+sessionID, session.ID)
	assert.Equal(t, AgentGrok, session.Agent)
	assert.Equal(t, sessionID, session.SourceSessionID)
	assert.Equal(t, "summary", session.TranscriptFidelity)
	assert.Equal(t, "\u5ba1\u67e5\u4ed3\u5e93\u4ee3\u7801", session.SessionName)
	assert.Equal(t, "\u5ba1\u67e5\u4ed3\u5e93\u4ee3\u7801", session.FirstMessage)
	assert.Equal(t, "wp_devops", session.Project)
	assert.Equal(t, "/Users/dev/repos/wp-devops", session.Cwd)
	assert.Equal(t, "refactor/shared-proxy-csv-utils", session.GitBranch)
	// Without chat_history.jsonl, counts fall back to summary/signals.
	// Prefer num_chat_messages (104) over the broader num_messages (927),
	// which includes non-chat events and would inflate analytics filters.
	assert.Equal(t, TranscriptFidelitySummary, session.TranscriptFidelity)
	assert.Equal(t, "grok-summary-v1", session.SourceVersion)
	assert.Equal(t, 104, session.MessageCount)
	assert.Equal(t, 7, session.UserMessageCount)
	assert.Zero(t, session.PeakContextTokens)
	assert.False(t, session.HasPeakContextTokens)
	assert.False(t, session.HasTotalOutputTokens)
	require.Len(t, outcome.Results[0].Result.Messages, 1)
	assert.Equal(t, RoleUser, outcome.Results[0].Result.Messages[0].Role)
	assert.Equal(t, "\u5ba1\u67e5\u4ed3\u5e93\u4ee3\u7801", outcome.Results[0].Result.Messages[0].Content)
}

func TestGrokProviderParsesChatHistoryTranscript(t *testing.T) {
	root := t.TempDir()
	cwdKey := "%2FUsers%2Fdev%2Frepos%2Fwp-devops"
	sessionID := "019f5483-db23-74c1-9d35-7df33f1c3ddc"
	writeGrokFixtureFile(t, grokSummaryPath(root, cwdKey, sessionID), `{
			"info": {"id": "019f5483-db23-74c1-9d35-7df33f1c3ddc", "cwd": "/Users/dev/repos/wp-devops"},
			"session_summary": "review branch",
			"created_at": "2026-07-12T04:09:15.439384Z",
			"updated_at": "2026-07-12T05:23:48.317854Z",
			"last_active_at": "2026-07-12T05:23:48.317854Z",
			"num_messages": 864,
			"git_root_dir": "/Users/dev/repos/wp-devops/",
			"head_branch": "refactor/shared-proxy-csv-utils"
		}`)
	writeGrokFixtureFile(t, filepath.Join(root, cwdKey, sessionID, "chat_history.jsonl"), strings.Join([]string{
		`{"type":"system","content":"You are Grok"}`,
		`{"type":"user","content":[{"type":"text","text":"<user_info>\nOS Version: macos\n</user_info>"}]}`,
		`{"type":"user","content":[{"type":"text","text":"<user_query>review branch vs main</user_query>"}]}`,
		`{"type":"reasoning","id":"","summary":[{"type":"summary_text","text":"Need to review the branch."}]}`,
		`{"type":"assistant","content":"Loading review skill.","tool_calls":[{"id":"call-1","name":"read_file","arguments":"{\"target_file\":\"SKILL.md\"}"}],"model_id":"grok-4.5"}`,
		`{"type":"tool_result","tool_call_id":"call-1","content":"skill body"}`,
		`{"type":"assistant","content":"Review complete.","model_id":"grok-4.5"}`,
		`{"type":"user","content":[{"type":"text","text":"<user_query>fix the issues</user_query>"}]}`,
	}, "\n")+"\n")

	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	result := outcome.Results[0].Result
	session := result.Session
	assert.Equal(t, TranscriptFidelityFull, session.TranscriptFidelity)
	assert.Equal(t, "grok-chat-v1", session.SourceVersion)
	assert.Equal(t, "review branch vs main", session.FirstMessage)
	assert.Equal(t, 2, session.UserMessageCount)
	// user, assistant(+thinking+tool), tool_result, assistant, user
	require.Len(t, result.Messages, 5)

	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, "review branch vs main", result.Messages[0].Content)

	assert.Equal(t, RoleAssistant, result.Messages[1].Role)
	assert.True(t, result.Messages[1].HasThinking)
	assert.Equal(t, "Need to review the branch.", result.Messages[1].ThinkingText)
	assert.True(t, result.Messages[1].HasToolUse)
	require.Len(t, result.Messages[1].ToolCalls, 1)
	assert.Equal(t, "call-1", result.Messages[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "read_file", result.Messages[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Read", result.Messages[1].ToolCalls[0].Category)
	assert.JSONEq(t, `{"target_file":"SKILL.md"}`, result.Messages[1].ToolCalls[0].InputJSON)
	assert.Equal(t, "grok-4.5", result.Messages[1].Model)

	assert.Equal(t, RoleUser, result.Messages[2].Role)
	require.Len(t, result.Messages[2].ToolResults, 1)
	assert.Equal(t, "call-1", result.Messages[2].ToolResults[0].ToolUseID)
	assert.Equal(t, 10, result.Messages[2].ToolResults[0].ContentLength)

	assert.Equal(t, RoleAssistant, result.Messages[3].Role)
	assert.Equal(t, "Review complete.", result.Messages[3].Content)

	assert.Equal(t, RoleUser, result.Messages[4].Role)
	assert.Equal(t, "fix the issues", result.Messages[4].Content)
}

func TestGrokProviderUnwrapsOpenAIStyleToolArguments(t *testing.T) {
	// OpenAI-style tool calls nest name/arguments under "function" and encode
	// arguments as a JSON string. InputJSON must be the decoded object so
	// path extraction and skill inference can read fields.
	root := t.TempDir()
	sessionID := "sess-openai-tools"
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", sessionID), `{
		"info": {"id": "sess-openai-tools", "cwd": "/tmp/proj"},
		"session_summary": "tool args",
		"created_at": "2026-07-12T04:00:00Z",
		"updated_at": "2026-07-12T04:01:00Z"
	}`)
	writeGrokFixtureFile(t, filepath.Join(root, "cwd-key", sessionID, "chat_history.jsonl"), strings.Join([]string{
		`{"type":"user","content":"read the skill"}`,
		`{"type":"assistant","content":"","tool_calls":[{"id":"call-fn","type":"function","function":{"name":"read_file","arguments":"{\"target_file\":\"SKILL.md\",\"offset\":10}"}}],"model_id":"grok-4.5"}`,
	}, "\n")+"\n")

	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	assert.Equal(t, "call-fn", tc.ToolUseID)
	assert.Equal(t, "read_file", tc.ToolName)
	assert.Equal(t, "Read", tc.Category)
	assert.JSONEq(t, `{"target_file":"SKILL.md","offset":10}`, tc.InputJSON)
	// Must not retain the outer JSON-string quotes.
	assert.NotContains(t, tc.InputJSON, `\"target_file\"`)
	assert.False(t, strings.HasPrefix(strings.TrimSpace(tc.InputJSON), `"`))
}

func TestGrokProviderKeepsUserPromptBesideMetadata(t *testing.T) {
	// Mixed context-injection + real prompt must keep the prompt instead of
	// dropping the whole user turn when any meta marker is present.
	root := t.TempDir()
	sessionID := "sess-mixed-meta"
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", sessionID), `{
		"info": {"id": "sess-mixed-meta", "cwd": "/tmp/proj"},
		"session_summary": "mixed meta",
		"created_at": "2026-07-12T04:00:00Z",
		"updated_at": "2026-07-12T04:01:00Z"
	}`)
	mixed := `<user_info>
OS Version: macos
</user_info>
<git_status>
## main
</git_status>

Please fix the flaky test in grok_test.go`
	// Escape for JSON string embedding.
	mixedJSON, err := json.Marshal(mixed)
	require.NoError(t, err)
	writeGrokFixtureFile(t, filepath.Join(root, "cwd-key", sessionID, "chat_history.jsonl"), strings.Join([]string{
		`{"type":"user","content":` + string(mixedJSON) + `}`,
		`{"type":"user","content":[{"type":"text","text":"<system-reminder>skills loaded</system-reminder>\n\nrun the suite"}]}`,
		`{"type":"user","content":[{"type":"text","text":"<user_info>\nOS Version: macos\n</user_info>"}]}`,
	}, "\n")+"\n")

	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	result := outcome.Results[0].Result
	// Meta-only third message is dropped; two real prompts remain.
	require.Len(t, result.Messages, 2)
	assert.Equal(t, 2, result.Session.UserMessageCount)
	assert.Equal(t, "Please fix the flaky test in grok_test.go", result.Messages[0].Content)
	assert.Equal(t, "run the suite", result.Messages[1].Content)
	assert.Equal(t, "Please fix the flaky test in grok_test.go", result.Session.FirstMessage)
}

func TestGrokProviderFindSource(t *testing.T) {
	root := t.TempDir()
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", "sess-1"), `{
		"summary": "Find source",
		"firstPrompt": "Locate the Grok source",
		"createdAt": "2026-07-08T10:00:00Z"
	}`)

	provider := newGrokTestProvider(t, root)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "sess-1",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, filepath.Clean(grokSummaryPath(root, "cwd-key", "sess-1")), filepath.Clean(source.FingerprintKey))

	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "missing",
	})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestGrokProviderFirstPromptKeepsSessionVisibleWithoutNumMessages(t *testing.T) {
	root := t.TempDir()
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", "sess-1"), `{
		"summary": "Find source",
		"firstPrompt": "Locate the Grok source",
		"createdAt": "2026-07-08T10:00:00Z"
	}`)

	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	session := outcome.Results[0].Result.Session
	assert.Equal(t, 1, session.MessageCount)
	assert.Equal(t, 1, session.UserMessageCount)
	require.Len(t, outcome.Results[0].Result.Messages, 1)
	assert.Equal(t, RoleUser, outcome.Results[0].Result.Messages[0].Role)
	assert.Equal(t, "Locate the Grok source", outcome.Results[0].Result.Messages[0].Content)
}

func TestGrokProviderFingerprintTracksParsedFiles(t *testing.T) {
	root := t.TempDir()
	summary := grokSummaryPath(root, "cwd-key", "sess-1")
	signals := filepath.Join(root, "cwd-key", "sess-1", "signals.json")
	updates := filepath.Join(root, "cwd-key", "sess-1", "updates.jsonl")
	chat := filepath.Join(root, "cwd-key", "sess-1", "chat_history.jsonl")
	unrelated := filepath.Join(root, "cwd-key", "sess-1", "notes.txt")
	writeGrokFixtureFile(t, summary, `{"summary":"Fingerprint","firstPrompt":"hello","createdAt":"2026-07-08T10:00:00Z"}`)
	writeGrokFixtureFile(t, signals, `{"tokenUsage":{"totalOutputTokens":1}}`)
	writeGrokFixtureFile(t, updates, "{}\n")
	writeGrokFixtureFile(t, chat, "{}\n")
	writeGrokFixtureFile(t, unrelated, "ignored")

	provider := newGrokTestProvider(t, root)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "sess-1",
	})
	require.NoError(t, err)
	require.True(t, ok)

	base, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	writeGrokFixtureFile(t, summary, `{"summary":"Fingerprint changed","firstPrompt":"hello","createdAt":"2026-07-08T10:00:00Z"}`)
	afterSummary, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, base.Hash, afterSummary.Hash)

	writeGrokFixtureFile(t, signals, `{"tokenUsage":{"totalOutputTokens":2}}`)
	afterSignals, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterSummary.Hash, afterSignals.Hash)

	writeGrokFixtureFile(t, chat, "{\"message\":1}\n")
	afterChat, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterSignals.Hash, afterChat.Hash)

	writeGrokFixtureFile(t, updates, "{\"delta\":1}\n")
	afterUpdates, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterChat.Hash, afterUpdates.Hash)

	writeGrokFixtureFile(t, unrelated, "still ignored")
	afterUnrelated, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, afterUpdates.Hash, afterUnrelated.Hash)
}

func TestGrokProviderChangedPathTracksParsedFiles(t *testing.T) {
	root := t.TempDir()
	summary := grokSummaryPath(root, "cwd-key", "sess-1")
	writeGrokFixtureFile(t, summary, `{"summary":"Changed path","firstPrompt":"hello","createdAt":"2026-07-08T10:00:00Z"}`)
	provider := newGrokTestProvider(t, root)

	for _, name := range []string{"summary.json", "signals.json", "chat_history.jsonl"} {
		changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
			Path: filepath.Join(root, "cwd-key", "sess-1", name),
		})
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, filepath.Clean(summary), filepath.Clean(changed[0].FingerprintKey))
	}

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: filepath.Join(root, "cwd-key", "sess-1", "updates.jsonl"),
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, filepath.Clean(summary), filepath.Clean(changed[0].FingerprintKey))
}

func TestGrokProviderWatchPlanIncludesUpdates(t *testing.T) {
	root := t.TempDir()
	provider := newGrokTestProvider(t, root)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.ElementsMatch(t,
		[]string{"summary.json", "signals.json", "chat_history.jsonl", "updates.jsonl"},
		plan.Roots[0].IncludeGlobs,
	)
}

func TestGrokProviderArtifactBoundaries(t *testing.T) {
	root := t.TempDir()
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", "sess-1"), `{
		"summary": "Valid",
		"firstPrompt": "valid prompt",
		"createdAt": "2026-07-08T10:00:00Z"
	}`)
	writeGrokFixtureFile(t, filepath.Join(root, "cwd-key", "not.a.grok.session", "summary.json"), `{
		"summary": "Ignored",
		"firstPrompt": "ignored",
		"createdAt": "2026-07-08T10:00:00Z"
	}`)
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", "sess-bad"), `{not json`)

	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path: filepath.Join(root, "cwd-key", "not.a.grok.session", "signals.json"),
		},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "sess-bad",
	})
	require.NoError(t, err)
	require.True(t, ok)
	_, err = provider.Parse(context.Background(), ParseRequest{Source: source})
	require.Error(t, err)
}

func TestGrokProviderRegistry(t *testing.T) {
	def, ok := AgentByType(AgentGrok)
	require.True(t, ok)
	assert.Equal(t, "GROK_DIR", def.EnvVar)
	assert.Equal(t, "grok_dirs", def.ConfigKey)
	assert.Equal(t, "grok:", def.IDPrefix)
	assert.Equal(t, []string{".grok/sessions"}, def.DefaultDirs)

	factory, ok := ProviderFactoryByType(AgentGrok)
	require.True(t, ok)
	assert.Equal(t, AgentGrok, factory.Definition().Type)

	mode, ok := ProviderMigrationModes()[AgentGrok]
	require.True(t, ok)
	assert.Equal(t, ProviderMigrationProviderAuthoritative, mode)
	assert.Equal(t, CapabilitySupported,
		factory.Capabilities().Content.AggregateUsageEvents)
}
