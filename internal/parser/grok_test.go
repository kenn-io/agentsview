package parser

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/money"
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
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
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
	assert := assert.New(t)
	require := require.New(t)

	result := parseGrokGolden(t, "current")
	assert.Equal("Review parser compatibility", result.Session.FirstMessage)
	assert.Equal(2, result.Session.UserMessageCount)
	require.Len(result.Messages, 6)
	assert.Equal(RoleUser, result.Messages[0].Role)
	assert.Equal("Review parser compatibility", result.Messages[0].Content)
	assert.Equal(RoleAssistant, result.Messages[1].Role)
	assert.Contains(result.Messages[1].Content, "Grok Build persistence")
	require.Len(result.Messages[1].ToolCalls, 1)
	assert.Equal("ws_1", result.Messages[1].ToolCalls[0].ToolUseID)
	assert.Equal("web_search", result.Messages[1].ToolCalls[0].ToolName)
	assert.Equal("Inspect both formats", result.Messages[2].ThinkingText)
	assert.Equal(RoleUser, result.Messages[4].Role)
	assert.Equal("also keep interjections", result.Messages[4].Content)
}

func TestGrokProviderGoldenCurrentMetadata(t *testing.T) {
	assert := assert.New(t)

	result := parseGrokGolden(t, "current")
	session := result.Session
	assert.Equal("/workspace/grok-worktrees/parser-audit", session.Cwd)
	assert.Equal("agentsview", session.Project)
	assert.Equal(
		"grok:019f5000-0000-7000-8000-000000000000",
		session.ParentSessionID,
	)
	assert.Equal(RelFork, session.RelationshipType)
	assert.False(session.HasPeakContextTokens)
	assert.Zero(session.PeakContextTokens)
}

func TestGrokProviderGoldenLegacyTranscript(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	result := parseGrokGolden(t, "legacy")
	assert.Equal(TranscriptFidelityFull, result.Session.TranscriptFidelity)
	assert.Equal("Review parser compatibility", result.Session.FirstMessage)
	require.Len(result.Messages, 4)
	assert.Equal(RoleUser, result.Messages[0].Role)
	assert.Equal("Review parser compatibility", result.Messages[0].Content)
	require.Len(result.Messages[1].ToolCalls, 1)
	assert.Equal("call_1", result.Messages[1].ToolCalls[0].ToolUseID)
	assert.Equal("grok-4.5", result.Messages[1].Model)
	assert.Equal("Check the old format", result.Messages[1].ThinkingText)
	require.Len(result.Messages[2].ToolResults, 1)
	assert.Equal("call_1", result.Messages[2].ToolResults[0].ToolUseID)
}

func TestGrokTimestampLookingPrompt(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "cwd-key", "grok-timestamp")
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "summary.json"), `{
		"info":{"id":"grok-timestamp","cwd":"/workspace/agentsview"},
		"created_at":"2026-07-02T15:11:00Z",
		"updated_at":"2026-07-02T15:12:00Z"
	}`)
	literal := "<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC-4)</timestamp> review this prompt"
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "chat_history.jsonl"),
		`{"type":"user","content":"`+literal+`"}`+"\n")

	result, err := ParseGrokSummary(
		filepath.Join(sessionDir, "summary.json"), "agentsview", "test-machine",
	)
	require.NoError(t, err)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, literal, result.Messages[0].Content)
}

func TestGrokProviderParsesMixedTranscriptFormats(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	outcome, err := provider.Parse(
		t.Context(),
		ParseRequest{Source: sources[0]},
	)
	require.NoError(err)
	require.Len(outcome.Results, 1)
	messages := outcome.Results[0].Result.Messages
	require.Len(messages, 2)
	assert.Equal("legacy question", messages[0].Content)
	assert.Equal("current answer", messages[1].Content)
}

func TestParseGrokChatHistoryUnknownTypeFallsBackToLegacyRole(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "chat_history.jsonl")
	writeGrokFixtureFile(
		t,
		path,
		`{"type":"future_metadata","role":"user","content":"legacy question"}`+"\n",
	)

	messages, malformed, err := parseGrokChatHistory(path)
	require.NoError(err)
	assert.Zero(malformed)
	require.Len(messages, 1)
	assert.Equal(RoleUser, messages[0].Role)
	assert.Equal("legacy question", messages[0].Content)
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
			assert := assert.New(t)
			require := require.New(t)

			path := filepath.Join(t.TempDir(), "chat_history.jsonl")
			writeGrokFixtureFile(t, path, strings.Join(tt.history, "\n")+"\n")
			messages, malformed, err := parseGrokChatHistory(path)
			require.NoError(err)
			assert.Zero(malformed)
			require.Len(messages, tt.wantCount)
			assistant := messages[len(messages)-1]
			assert.Equal(RoleAssistant, assistant.Role)
			assert.Equal(tt.wantThink, assistant.ThinkingText)
			assert.True(assistant.HasThinking)
			if tt.name == "legacy raw output reasoning" {
				require.Len(messages[0].ToolCalls, 1)
				assert.Equal("ws_raw", messages[0].ToolCalls[0].ToolUseID)
				assert.Contains(messages[0].Content, "raw query")
			}
		})
	}
}

func TestParseGrokChatHistoryInlineReasoningOverridesPending(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "chat_history.jsonl")
	writeGrokFixtureFile(t, path, strings.Join([]string{
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"pending"}]}`,
		`{"type":"assistant","content":"answer","reasoning":{"text":"inline"}}`,
	}, "\n")+"\n")
	messages, malformed, err := parseGrokChatHistory(path)
	require.NoError(err)
	assert.Zero(malformed)
	require.Len(messages, 1)
	assert.Equal("inline", messages[0].ThinkingText)
}

func TestParseGrokChatHistoryDeduplicatesRawBackendToolCalls(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "chat_history.jsonl")
	writeGrokFixtureFile(t, path, strings.Join([]string{
		`{"type":"backend_tool_call","kind":{"tool_type":"web_search","id":"ws_same","status":"completed","action":{"type":"search","query":"same query","sources":[]}}}`,
		`{"type":"assistant","content":"answer","raw_output":[{"type":"web_search_call","id":"ws_same","status":"completed","action":{"type":"search","query":"same query","sources":[]}}]}`,
	}, "\n")+"\n")
	messages, malformed, err := parseGrokChatHistory(path)
	require.NoError(err)
	assert.Zero(malformed)
	require.Len(messages, 2)
	require.Len(messages[0].ToolCalls, 1)
	assert.Equal("ws_same", messages[0].ToolCalls[0].ToolUseID)
	assert.Equal("answer", messages[1].Content)
}

func TestParseGrokChatHistoryPreservesCodeInterpreterInput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	code := strings.Repeat("print('counterfactual')\n", 20)
	codeJSON, err := json.Marshal(code)
	require.NoError(err)
	path := filepath.Join(t.TempDir(), "chat_history.jsonl")
	writeGrokFixtureFile(
		t,
		path,
		`{"type":"backend_tool_call","kind":{"tool_type":"code_interpreter","code":`+
			string(codeJSON)+
			`,"container_id":"container_counterfactual","id":"ci_counterfactual","outputs":[{"type":"logs","logs":"finished"}],"status":"completed"}}`+"\n",
	)

	messages, malformed, err := parseGrokChatHistory(path)
	require.NoError(err)
	assert.Zero(malformed)
	require.Len(messages, 1)
	require.Len(messages[0].ToolCalls, 1)
	var input struct {
		Code string `json:"code"`
	}
	require.NoError(json.Unmarshal(
		[]byte(messages[0].ToolCalls[0].InputJSON),
		&input,
	))
	assert.Equal(code, input.Code)
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
			assert := assert.New(t)
			require := require.New(t)

			rows := append([]string{
				`{"type":"reasoning","summary":[{"type":"summary_text","text":"orphan"}]}`,
			}, tt.trailing...)
			path := filepath.Join(t.TempDir(), "chat_history.jsonl")
			writeGrokFixtureFile(t, path, strings.Join(rows, "\n")+"\n")
			messages, malformed, err := parseGrokChatHistory(path)
			require.NoError(err)
			assert.Zero(malformed)
			require.Len(messages, tt.wantCount)
			for _, message := range messages {
				assert.False(message.HasThinking)
				assert.NotContains(message.Content, "orphan")
			}
		})
	}
}

// A followed cwd- or session-directory symlink whose target cannot be
// resolved must surface incomplete streaming discovery rather than reading as
// absent: reconciliation treats a clean DiscoverEach as authoritative and
// would tombstone every session beneath the symlink.
func TestGrokProviderStreamingDiscoveryPropagatesDirectorySymlinkErrors(t *testing.T) {
	discoverEach := func(t *testing.T, root string) ([]string, error) {
		t.Helper()
		provider := newGrokTestProvider(t, root)
		discoverer, ok := provider.(StreamingDiscoverer)
		require.True(t, ok)
		var yielded []string
		err := discoverer.DiscoverEach(t.Context(), func(source SourceRef) error {
			yielded = append(yielded, source.DisplayPath)
			return nil
		})
		return yielded, err
	}
	writeHealthySession := func(t *testing.T, root string) string {
		t.Helper()
		path := grokSummaryPath(root, "cwd-key", "sess-1")
		writeGrokFixtureFile(t, path, `{"summary":"Healthy"}`)
		return path
	}

	t.Run("dangling cwd symlink", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		root := t.TempDir()
		healthy := writeHealthySession(t, root)
		target := filepath.Join(t.TempDir(), "linked-cwd")
		require.NoError(os.MkdirAll(target, 0o755))
		link := filepath.Join(root, "linked")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.NoError(os.RemoveAll(target))

		_, err := discoverEach(t, root)

		require.Error(err)
		assert.ErrorIs(err, os.ErrNotExist)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(err, &incomplete)

		require.NoError(os.Remove(link))
		yielded, err := discoverEach(t, root)
		require.NoError(err)
		assert.Equal([]string{healthy}, yielded)
	})

	t.Run("dangling session symlink", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		root := t.TempDir()
		healthy := writeHealthySession(t, root)
		target := filepath.Join(t.TempDir(), "linked-session")
		require.NoError(os.MkdirAll(target, 0o755))
		link := filepath.Join(root, "cwd-key", "sess-linked")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.NoError(os.RemoveAll(target))

		_, err := discoverEach(t, root)

		require.Error(err)
		assert.ErrorIs(err, os.ErrNotExist)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(err, &incomplete)

		require.NoError(os.Remove(link))
		yielded, err := discoverEach(t, root)
		require.NoError(err)
		assert.Equal([]string{healthy}, yielded)
	})

	t.Run("unstatable cwd symlink target", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		if runtime.GOOS == "windows" {
			t.Skip("directory read permissions are not enforced on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		root := t.TempDir()
		healthy := writeHealthySession(t, root)
		targetParent := t.TempDir()
		target := filepath.Join(targetParent, "linked-cwd")
		require.NoError(os.MkdirAll(target, 0o755))
		if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.NoError(os.Chmod(targetParent, 0o000))
		t.Cleanup(func() { _ = os.Chmod(targetParent, 0o755) })

		_, err := discoverEach(t, root)

		require.Error(err)
		assert.ErrorIs(err, os.ErrPermission)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(err, &incomplete)

		require.NoError(os.Chmod(targetParent, 0o755))
		yielded, err := discoverEach(t, root)
		require.NoError(err)
		assert.Equal([]string{healthy}, yielded)
	})
}

func TestGrokProviderSummarySource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)

	session := outcome.Results[0].Result.Session
	assert.Equal("grok:sess-1", session.ID)
	assert.Equal(AgentGrok, session.Agent)
	assert.Equal("sess-1", session.SourceSessionID)
	assert.Equal("grok-summary-v1", session.SourceVersion)
	assert.Equal("summary", session.TranscriptFidelity)
	assert.Equal("Investigate the failing Grok session import", session.FirstMessage)
	assert.Equal("Fix parser regression", session.SessionName)
	assert.Equal("agentsview", session.Project)
	assert.Equal(6, session.MessageCount)
	assert.Equal(1, session.UserMessageCount)
	assert.Equal(321, session.TotalOutputTokens)
	assert.Equal(4096, session.PeakContextTokens)
	assert.Equal(filepath.Clean(grokSummaryPath(root, "cwd-key", "sess-1")), filepath.Clean(session.File.Path))
	require.Len(outcome.Results[0].Result.Messages, 1)
	assert.Equal(RoleUser, outcome.Results[0].Result.Messages[0].Role)
	assert.Equal("Investigate the failing Grok session import", outcome.Results[0].Result.Messages[0].Content)
}

func TestGrokProviderPreservesSignalPeakOverCumulativeUsageInput(t *testing.T) {
	assert := assert.New(t)

	root := t.TempDir()
	sessionDir := filepath.Join(root, "cwd-key", "sess-1")
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "summary.json"), `{
		"summary":"Usage fixture",
		"firstPrompt":"test usage",
		"modelId":"grok-summary",
		"createdAt":"2026-07-08T10:00:00Z",
		"updatedAt":"2026-07-08T10:30:00Z"
	}`)
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "signals.json"), `{
		"tokenUsage": {
			"peakContextTokens": 4096
		}
	}`)
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "updates.jsonl"), `{"params":{"update":{"usage":{"inputTokens":8192,"outputTokens":7,"cachedReadTokens":96}}}}`)

	result, err := ParseGrokSummary(
		filepath.Join(sessionDir, "summary.json"), "cwd-key", "local",
	)
	require.NoError(t, err)
	assert.True(result.Session.HasPeakContextTokens)
	assert.Equal(4096, result.Session.PeakContextTokens)
	assert.True(result.Session.HasTotalOutputTokens)
	assert.Equal(7, result.Session.TotalOutputTokens)
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

func TestGrokProviderPerTurnUsageEvents(t *testing.T) {
	assert := assert.New(t)

	// Usage payloads are per-turn measurements, not cumulative snapshots:
	// every turn must produce its own event, and cachedReadTokens moving
	// down between turns (10 -> 13 with a lower input) must be preserved
	// as-is rather than treated as a snapshot to overwrite.
	result := parseGrokUsageFixture(t, strings.Join([]string{
		`{"params":{"update":{"usage":{"inputTokens":100,"outputTokens":20,"cachedReadTokens":10,"reasoningTokens":4}}}}`,
		`{"params":{"update":{"usage":{"inputTokens":8,"outputTokens":3,"cachedReadTokens":13,"reasoningTokens":1}}}}`,
		`not json`,
	}, "\n"))

	require.Len(t, result.UsageEvents, 2)
	first, second := result.UsageEvents[0], result.UsageEvents[1]
	assert.Equal(90, first.InputTokens)
	assert.Equal(10, first.CacheReadInputTokens)
	assert.Equal(20, first.OutputTokens)
	assert.Equal(4, first.ReasoningTokens)
	assert.Equal("session:grok:sess-1:turn-1:grok-summary", first.DedupKey)
	assert.Equal(0, second.InputTokens)
	assert.Equal(13, second.CacheReadInputTokens)
	assert.Equal(3, second.OutputTokens)
	assert.Equal(1, second.ReasoningTokens)
	assert.Equal("session:grok:sess-1:turn-2:grok-summary", second.DedupKey)
	assert.Equal(23, result.Session.TotalOutputTokens)
}

func TestGrokProviderPerTurnUsageDedupsByPromptID(t *testing.T) {
	result := parseGrokUsageFixture(t, strings.Join([]string{
		`{"timestamp":1784575476,"params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"p-aaa","usage":{"modelUsage":{"grok-4.5":{"inputTokens":1100,"outputTokens":10,"cachedReadTokens":1000}}}}}}`,
		`{"timestamp":1784575500,"params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"p-bbb","usage":{"modelUsage":{"grok-4.5":{"inputTokens":200,"outputTokens":20,"cachedReadTokens":128},"grok-4.5-build":{"inputTokens":50,"outputTokens":5,"cachedReadTokens":0}}}}}}`,
	}, "\n"))

	require.Len(t, result.UsageEvents, 3)
	keys := make([]string, 0, len(result.UsageEvents))
	for _, event := range result.UsageEvents {
		keys = append(keys, event.DedupKey)
	}
	assert.ElementsMatch(t, []string{
		"session:grok:sess-1:p-aaa:grok-4.5",
		"session:grok:sess-1:p-bbb:grok-4.5",
		"session:grok:sess-1:p-bbb:grok-4.5-build",
	}, keys)
}

func TestGrokProviderPerTurnUsageLastWinsOnDuplicatePromptID(t *testing.T) {
	assert := assert.New(t)

	// Retry/replay lines re-emit a turn's payload under the same
	// prompt_id. They must collapse to a single event (the DB enforces a
	// unique (session_id, source, dedup_key) index — a duplicate would
	// roll back the whole usage replace), keeping the last payload.
	result := parseGrokUsageFixture(t, strings.Join([]string{
		`{"timestamp":1784575476,"params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"p-aaa","usage":{"modelUsage":{"grok-4.5":{"inputTokens":1100,"outputTokens":10,"cachedReadTokens":1000}}}}}}`,
		`{"timestamp":1784575480,"params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"p-aaa","usage":{"modelUsage":{"grok-4.5":{"inputTokens":1200,"outputTokens":15,"cachedReadTokens":1000}}}}}}`,
	}, "\n"))

	require.Len(t, result.UsageEvents, 1)
	event := result.UsageEvents[0]
	assert.Equal("session:grok:sess-1:p-aaa:grok-4.5", event.DedupKey)
	assert.Equal(200, event.InputTokens)
	assert.Equal(15, event.OutputTokens)
	assert.Equal("2026-07-20T19:24:40Z", event.OccurredAt)
	assert.Equal(15, result.Session.TotalOutputTokens)
}

func TestGrokProviderPerTurnUsageDatesByTurnTimestamp(t *testing.T) {
	assert := assert.New(t)

	// A session spanning several days must bucket each turn's usage on
	// the day the turn happened, not the session's end date.
	result := parseGrokUsageFixture(t, strings.Join([]string{
		`{"timestamp":1779735600,"params":{"update":{"usage":{"inputTokens":10,"outputTokens":1}}}}`,
		`{"timestamp":1779822000,"params":{"update":{"usage":{"inputTokens":20,"outputTokens":2}}}}`,
		`{"params":{"update":{"usage":{"inputTokens":30,"outputTokens":3}}}}`,
	}, "\n"))

	require.Len(t, result.UsageEvents, 3)
	assert.Equal("2026-05-25T19:00:00Z", result.UsageEvents[0].OccurredAt)
	assert.Equal("2026-05-26T19:00:00Z", result.UsageEvents[1].OccurredAt)
	// No per-turn timestamp: falls back to the session window (updatedAt).
	assert.Equal("2026-07-08T10:30:00Z", result.UsageEvents[2].OccurredAt)
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
	assert := assert.New(t)

	result := parseGrokUsageFixture(t, strings.Join([]string{
		`{"params":{"update":{"usage":{"inputTokens":100,"outputTokens":20,"cachedReadTokens":10,"reasoningTokens":4}}}}`,
		`{"type":"unrelated","payload":{"ok":true}}`,
	}, "\n"))

	require.Len(t, result.UsageEvents, 1)
	event := result.UsageEvents[0]
	assert.Equal(90, event.InputTokens)
	assert.Equal(10, event.CacheReadInputTokens)
	assert.Equal(20, event.OutputTokens)
	assert.Equal(4, event.ReasoningTokens)
	assert.Equal("grok-summary", event.Model)
}

func TestGrokProviderUpdatesUsageByModel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	result := parseGrokUsageFixture(t, `{"params":{"update":{"usage":{"inputTokens":999,"outputTokens":999,"costUsdTicks":9999999999,"modelUsage":{"grok-a":{"inputTokens":10,"outputTokens":2,"costUsdTicks":10000000000},"grok-b":{"inputTokens":20,"outputTokens":3,"costUsdTicks":2500000000}}}}}}`)

	require.Len(result.UsageEvents, 2)
	models := map[string]ParsedUsageEvent{}
	for _, event := range result.UsageEvents {
		models[event.Model] = event
	}
	assert.Equal(2, models["grok-a"].OutputTokens)
	assert.Equal(3, models["grok-b"].OutputTokens)
	require.NotNil(models["grok-a"].Cost)
	require.NotNil(models["grok-b"].Cost)
	assert.Equal(money.Money{Microdollars: 1_000_000}, *models["grok-a"].Cost)
	assert.Equal(money.Money{Microdollars: 250_000}, *models["grok-b"].Cost)
	assert.NotContains(models, "grok-summary")
}

func TestGrokProviderUpdatesUsageTopLevelFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	result := parseGrokUsageFixture(t, `{"params":{"update":{"usage":{"inputTokens":12,"outputTokens":4,"costUsdTicks":424128000,"modelUsage":{}}}}}`)

	require.Len(result.UsageEvents, 1)
	assert.Equal("grok-summary", result.UsageEvents[0].Model)
	assert.Equal(12, result.UsageEvents[0].InputTokens)
	require.NotNil(result.UsageEvents[0].Cost)
	assert.Equal(money.Money{Microdollars: 42_413},
		*result.UsageEvents[0].Cost,
	)
}

func TestGrokProviderUpdatesUsageTopLevelFallbackWithoutSummaryModel(t *testing.T) {
	assert := assert.New(t)

	result := parseGrokUsageFixtureWithSummary(t, `{
		"summary":"Usage fixture",
		"firstPrompt":"test usage",
		"createdAt":"2026-07-08T10:00:00Z",
		"updatedAt":"2026-07-08T10:30:00Z"
	}`, `{"params":{"update":{"usage":{"inputTokens":12,"outputTokens":4,"modelUsage":{}}}}}`)

	require.Len(t, result.UsageEvents, 1)
	assert.Equal("grok-summary", result.UsageEvents[0].Model)
	assert.Equal(12, result.UsageEvents[0].InputTokens)
	assert.Nil(result.UsageEvents[0].Cost)
}

func TestGrokProviderUpdatesUsageReportedZeroCost(t *testing.T) {
	result := parseGrokUsageFixture(t, `{"params":{"update":{"usage":{"inputTokens":12,"outputTokens":4,"costUsdTicks":0,"modelUsage":{}}}}}`)

	require.Len(t, result.UsageEvents, 1)
	require.NotNil(t, result.UsageEvents[0].Cost)
	assert.Equal(t, money.Money{}, *result.UsageEvents[0].Cost)
}

func TestGrokUsageCostRejectsNegativeCharge(t *testing.T) {
	_, err := grokUsageCost(gjson.Parse(`{"costUsdTicks":-1}`))
	require.Error(t, err)
	assert.ErrorIs(t, err, money.ErrNegative)
}

func TestGrokProviderCurrentBuildSummarySchema(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(cwdKey, sources[0].ProjectHint)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)

	session := outcome.Results[0].Result.Session
	assert.Equal("grok:"+sessionID, session.ID)
	assert.Equal(AgentGrok, session.Agent)
	assert.Equal(sessionID, session.SourceSessionID)
	assert.Equal("summary", session.TranscriptFidelity)
	assert.Equal("\u5ba1\u67e5\u4ed3\u5e93\u4ee3\u7801", session.SessionName)
	assert.Equal("\u5ba1\u67e5\u4ed3\u5e93\u4ee3\u7801", session.FirstMessage)
	assert.Equal("wp_devops", session.Project)
	assert.Equal("/Users/dev/repos/wp-devops", session.Cwd)
	assert.Equal("refactor/shared-proxy-csv-utils", session.GitBranch)
	// Without chat_history.jsonl, counts fall back to summary/signals.
	// Prefer num_chat_messages (104) over the broader num_messages (927),
	// which includes non-chat events and would inflate analytics filters.
	assert.Equal(TranscriptFidelitySummary, session.TranscriptFidelity)
	assert.Equal("grok-summary-v1", session.SourceVersion)
	assert.Equal(104, session.MessageCount)
	assert.Equal(7, session.UserMessageCount)
	assert.Zero(session.PeakContextTokens)
	assert.False(session.HasPeakContextTokens)
	assert.False(session.HasTotalOutputTokens)
	require.Len(outcome.Results[0].Result.Messages, 1)
	assert.Equal(RoleUser, outcome.Results[0].Result.Messages[0].Role)
	assert.Equal("\u5ba1\u67e5\u4ed3\u5e93\u4ee3\u7801", outcome.Results[0].Result.Messages[0].Content)
}

func TestGrokProviderParsesChatHistoryTranscript(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)

	result := outcome.Results[0].Result
	session := result.Session
	assert.Equal(TranscriptFidelityFull, session.TranscriptFidelity)
	assert.Equal("grok-chat-v1", session.SourceVersion)
	assert.Equal("review branch vs main", session.FirstMessage)
	assert.Equal(2, session.UserMessageCount)
	// user, assistant(+thinking+tool), tool_result, assistant, user
	require.Len(result.Messages, 5)

	assert.Equal(RoleUser, result.Messages[0].Role)
	assert.Equal("review branch vs main", result.Messages[0].Content)

	assert.Equal(RoleAssistant, result.Messages[1].Role)
	assert.True(result.Messages[1].HasThinking)
	assert.Equal("Need to review the branch.", result.Messages[1].ThinkingText)
	assert.True(result.Messages[1].HasToolUse)
	require.Len(result.Messages[1].ToolCalls, 1)
	assert.Equal("call-1", result.Messages[1].ToolCalls[0].ToolUseID)
	assert.Equal("read_file", result.Messages[1].ToolCalls[0].ToolName)
	assert.Equal("Read", result.Messages[1].ToolCalls[0].Category)
	assert.JSONEq(`{"target_file":"SKILL.md"}`, result.Messages[1].ToolCalls[0].InputJSON)
	assert.Equal("grok-4.5", result.Messages[1].Model)

	assert.Equal(RoleUser, result.Messages[2].Role)
	require.Len(result.Messages[2].ToolResults, 1)
	assert.Equal("call-1", result.Messages[2].ToolResults[0].ToolUseID)
	assert.Equal(10, result.Messages[2].ToolResults[0].ContentLength)

	assert.Equal(RoleAssistant, result.Messages[3].Role)
	assert.Equal("Review complete.", result.Messages[3].Content)

	assert.Equal(RoleUser, result.Messages[4].Role)
	assert.Equal("fix the issues", result.Messages[4].Content)
}

func TestGrokProviderEnrichesTranscriptWithUpdateTimestamps(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "workspace-key", "session-timing")
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "summary.json"), `{
		"info":{"id":"session-timing","cwd":"/workspace/sample-project"},
		"session_summary":"sample conversation",
		"created_at":"2023-11-14T22:13:20Z",
		"updated_at":"2023-11-14T22:15:20Z"
	}`)
	writeGrokFixtureFile(
		t,
		filepath.Join(sessionDir, "chat_history.jsonl"),
		strings.Join([]string{
			`{"type":"user","content":"first request"}`,
			`{"type":"assistant","content":"first response"}`,
			`{"type":"user","content":"follow-up request"}`,
			`{"type":"assistant","content":"second response"}`,
		}, "\n")+"\n",
	)
	writeGrokFixtureFile(
		t,
		filepath.Join(sessionDir, "updates.jsonl"),
		strings.Join([]string{
			`{"timestamp":1700000000,"method":"session/update","params":{"sessionId":"session-timing","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"first request"}}}}`,
			`{"timestamp":1700000010,"method":"session/update","params":{"sessionId":"session-timing","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"first response"}}}}`,
			`{"timestamp":1700000060,"method":"session/update","params":{"sessionId":"session-timing","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"follow-up request"}}}}`,
			`{"timestamp":1700000075,"method":"session/update","params":{"sessionId":"session-timing","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"second response"}}}}`,
		}, "\n")+"\n",
	)

	result, err := ParseGrokSummary(
		filepath.Join(sessionDir, "summary.json"), "sample-project", "test-machine",
	)
	require.NoError(t, err)
	require.Len(t, result.Messages, 4)
	assert.Equal(t, []time.Time{
		time.Unix(1_700_000_000, 0).UTC(),
		time.Unix(1_700_000_010, 0).UTC(),
		time.Unix(1_700_000_060, 0).UTC(),
		time.Unix(1_700_000_075, 0).UTC(),
	}, []time.Time{
		result.Messages[0].Timestamp,
		result.Messages[1].Timestamp,
		result.Messages[2].Timestamp,
		result.Messages[3].Timestamp,
	})
}

func TestGrokProviderUpdateTimestampsFollowToolBoundaries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessionDir := filepath.Join(root, "workspace-key", "session-tools")
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "summary.json"), `{
		"info":{"id":"session-tools","cwd":"/workspace/sample-project"},
		"session_summary":"tool conversation",
		"created_at":"2023-11-14T22:13:20Z",
		"updated_at":"2023-11-14T22:15:20Z"
	}`)
	writeGrokFixtureFile(
		t,
		filepath.Join(sessionDir, "chat_history.jsonl"),
		strings.Join([]string{
			`{"type":"user","content":"inspect the sample"}`,
			`{"type":"assistant","content":"","tool_calls":[{"id":"call-sample","name":"read_file","arguments":"{\"target_file\":\"sample.txt\"}"},{"id":"call-reference","name":"read_file","arguments":"{\"target_file\":\"reference.txt\"}"}]}`,
			`{"type":"tool_result","tool_call_id":"call-sample","content":"example contents"}`,
			`{"type":"tool_result","tool_call_id":"call-reference","content":"reference contents"}`,
			`{"type":"assistant","content":"inspection complete"}`,
		}, "\n")+"\n",
	)
	writeGrokFixtureFile(
		t,
		filepath.Join(sessionDir, "updates.jsonl"),
		strings.Join([]string{
			`{"timestamp":1700000000,"method":"session/update","params":{"sessionId":"session-tools","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"inspect the sample"}}}}`,
			`{"timestamp":1700000010,"method":"session/update","params":{"sessionId":"session-tools","update":{"sessionUpdate":"tool_call","toolCallId":"call-sample","title":"read_file"}}}`,
			`{"timestamp":1700000012,"method":"session/update","params":{"sessionId":"session-tools","update":{"sessionUpdate":"tool_call","toolCallId":"call-reference","title":"read_file"}}}`,
			`{"timestamp":1700000020,"method":"session/update","params":{"sessionId":"session-tools","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-sample","status":"completed"}}}`,
			`{"timestamp":1700000022,"method":"session/update","params":{"sessionId":"session-tools","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-reference","status":"failed"}}}`,
			`{"timestamp":1700000030,"method":"session/update","params":{"sessionId":"session-tools","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"inspection complete"}}}}`,
		}, "\n")+"\n",
	)

	result, err := ParseGrokSummary(
		filepath.Join(sessionDir, "summary.json"), "sample-project", "test-machine",
	)
	require.NoError(err)
	require.Len(result.Messages, 5)
	assert.Equal(time.Unix(1_700_000_000, 0).UTC(), result.Messages[0].Timestamp)
	assert.Equal(time.Unix(1_700_000_010, 0).UTC(), result.Messages[1].Timestamp)
	assert.True(result.Messages[2].Timestamp.IsZero())
	assert.True(result.Messages[3].Timestamp.IsZero())
	assert.Equal(time.Unix(1_700_000_030, 0).UTC(), result.Messages[4].Timestamp)
	require.Len(result.Messages[1].ToolCalls, 2)
	for i, want := range []struct {
		started, completed int64
		status             string
	}{
		{1_700_000_010, 1_700_000_020, "completed"},
		{1_700_000_012, 1_700_000_022, "errored"},
	} {
		require.Len(result.Messages[1].ToolCalls[i].ResultEvents, 2)
		assert.Equal("started", result.Messages[1].ToolCalls[i].ResultEvents[0].Status)
		assert.Equal(time.Unix(want.started, 0).UTC(), result.Messages[1].ToolCalls[i].ResultEvents[0].Timestamp)
		assert.Equal(want.status, result.Messages[1].ToolCalls[i].ResultEvents[1].Status)
		assert.Equal(time.Unix(want.completed, 0).UTC(), result.Messages[1].ToolCalls[i].ResultEvents[1].Timestamp)
	}
}

func TestGrokProviderUpdateTimestampsRejectMismatchedToolIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessionDir := filepath.Join(root, "workspace-key", "session-tool-mismatch")
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "summary.json"), `{
		"info":{"id":"session-tool-mismatch","cwd":"/workspace/sample-project"},
		"session_summary":"tool conversation",
		"created_at":"2023-11-14T22:13:20Z",
		"updated_at":"2023-11-14T22:15:20Z"
	}`)
	writeGrokFixtureFile(
		t,
		filepath.Join(sessionDir, "chat_history.jsonl"),
		strings.Join([]string{
			`{"type":"user","content":"inspect the sample"}`,
			`{"type":"assistant","content":"","tool_calls":[{"id":"call-missing","name":"read_file","arguments":"{\"target_file\":\"missing.txt\"}"}]}`,
			`{"type":"tool_result","tool_call_id":"call-missing","content":"missing result"}`,
			`{"type":"assistant","content":"","tool_calls":[{"id":"call-current","name":"read_file","arguments":"{\"target_file\":\"current.txt\"}"}]}`,
			`{"type":"tool_result","tool_call_id":"call-current","content":"current result"}`,
		}, "\n")+"\n",
	)
	writeGrokFixtureFile(
		t,
		filepath.Join(sessionDir, "updates.jsonl"),
		strings.Join([]string{
			`{"timestamp":1700000000,"method":"session/update","params":{"sessionId":"session-tool-mismatch","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"inspect the sample"}}}}`,
			`{"timestamp":1700000010,"method":"session/update","params":{"sessionId":"session-tool-mismatch","update":{"sessionUpdate":"tool_call","toolCallId":"call-current","title":"read_file"}}}`,
			`{"timestamp":1700000020,"method":"session/update","params":{"sessionId":"session-tool-mismatch","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-current","status":"completed"}}}`,
		}, "\n")+"\n",
	)

	result, err := ParseGrokSummary(
		filepath.Join(sessionDir, "summary.json"), "sample-project", "test-machine",
	)
	require.NoError(err)
	require.Len(result.Messages, 5)
	assert.True(result.Messages[1].Timestamp.IsZero())
	assert.Equal(
		time.Unix(1_700_000_010, 0).UTC(), result.Messages[3].Timestamp,
	)
}

func TestGrokProviderUpdateTimestampsSeparateParallelBackendTools(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "workspace-key", "session-parallel-tools")
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "summary.json"), `{
		"info":{"id":"session-parallel-tools","cwd":"/workspace/sample-project"},
		"session_summary":"parallel tool conversation",
		"created_at":"2023-11-14T22:13:20Z",
		"updated_at":"2023-11-14T22:15:20Z"
	}`)
	writeGrokFixtureFile(
		t,
		filepath.Join(sessionDir, "chat_history.jsonl"),
		strings.Join([]string{
			`{"type":"user","content":"compare two examples"}`,
			`{"type":"backend_tool_call","kind":{"tool_type":"web_search","id":"search-first","status":"completed","action":{"type":"search","query":"first example","sources":[]}}}`,
			`{"type":"backend_tool_call","kind":{"tool_type":"web_search","id":"search-second","status":"completed","action":{"type":"search","query":"second example","sources":[]}}}`,
			`{"type":"assistant","content":"comparison complete"}`,
		}, "\n")+"\n",
	)
	writeGrokFixtureFile(
		t,
		filepath.Join(sessionDir, "updates.jsonl"),
		strings.Join([]string{
			`{"timestamp":1700000000,"method":"session/update","params":{"sessionId":"session-parallel-tools","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"compare two examples"}}}}`,
			`{"timestamp":1700000010,"method":"session/update","params":{"sessionId":"session-parallel-tools","update":{"sessionUpdate":"tool_call","toolCallId":"search-first","title":"Web search:"}}}`,
			`{"timestamp":1700000012,"method":"session/update","params":{"sessionId":"session-parallel-tools","update":{"sessionUpdate":"tool_call","toolCallId":"search-second","title":"Web search:"}}}`,
			`{"timestamp":1700000020,"method":"session/update","params":{"sessionId":"session-parallel-tools","update":{"sessionUpdate":"tool_call_update","toolCallId":"search-first","status":"completed"}}}`,
			`{"timestamp":1700000022,"method":"session/update","params":{"sessionId":"session-parallel-tools","update":{"sessionUpdate":"tool_call_update","toolCallId":"search-second","status":"completed"}}}`,
			`{"timestamp":1700000030,"method":"session/update","params":{"sessionId":"session-parallel-tools","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"comparison complete"}}}}`,
		}, "\n")+"\n",
	)

	result, err := ParseGrokSummary(
		filepath.Join(sessionDir, "summary.json"), "sample-project", "test-machine",
	)
	require.NoError(t, err)
	require.Len(t, result.Messages, 4)
	assert.Equal(t, []time.Time{
		time.Unix(1_700_000_000, 0).UTC(),
		time.Unix(1_700_000_010, 0).UTC(),
		time.Unix(1_700_000_012, 0).UTC(),
		time.Unix(1_700_000_030, 0).UTC(),
	}, []time.Time{
		result.Messages[0].Timestamp,
		result.Messages[1].Timestamp,
		result.Messages[2].Timestamp,
		result.Messages[3].Timestamp,
	})
}

func TestGrokProviderUnwrapsOpenAIStyleToolArguments(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	require.Len(outcome.Results, 1)

	msgs := outcome.Results[0].Result.Messages
	require.Len(msgs, 2)
	require.Len(msgs[1].ToolCalls, 1)
	tc := msgs[1].ToolCalls[0]
	assert.Equal("call-fn", tc.ToolUseID)
	assert.Equal("read_file", tc.ToolName)
	assert.Equal("Read", tc.Category)
	assert.JSONEq(`{"target_file":"SKILL.md","offset":10}`, tc.InputJSON)
	// Must not retain the outer JSON-string quotes.
	assert.NotContains(tc.InputJSON, `\"target_file\"`)
	assert.False(strings.HasPrefix(strings.TrimSpace(tc.InputJSON), `"`))
}

func TestGrokProviderKeepsUserPromptBesideMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	writeGrokFixtureFile(t, filepath.Join(root, "cwd-key", sessionID, "chat_history.jsonl"), strings.Join([]string{
		`{"type":"user","content":` + string(mixedJSON) + `}`,
		`{"type":"user","content":[{"type":"text","text":"<system-reminder>skills loaded</system-reminder>\n\nrun the suite"}]}`,
		`{"type":"user","content":[{"type":"text","text":"<user_info>\nOS Version: macos\n</user_info>"}]}`,
	}, "\n")+"\n")

	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	require.Len(outcome.Results, 1)

	result := outcome.Results[0].Result
	// Meta-only third message is dropped; two real prompts remain.
	require.Len(result.Messages, 2)
	assert.Equal(2, result.Session.UserMessageCount)
	assert.Equal("Please fix the flaky test in grok_test.go", result.Messages[0].Content)
	assert.Equal("run the suite", result.Messages[1].Content)
	assert.Equal("Please fix the flaky test in grok_test.go", result.Session.FirstMessage)
}

func TestGrokProviderFindSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", "sess-1"), `{
		"summary": "Find source",
		"firstPrompt": "Locate the Grok source",
		"createdAt": "2026-07-08T10:00:00Z"
	}`)

	provider := newGrokTestProvider(t, root)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "sess-1",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(filepath.Clean(grokSummaryPath(root, "cwd-key", "sess-1")), filepath.Clean(source.FingerprintKey))

	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "missing",
	})
	require.NoError(err)
	assert.False(ok)
}

func TestGrokProviderFirstPromptKeepsSessionVisibleWithoutNumMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	writeGrokFixtureFile(t, grokSummaryPath(root, "cwd-key", "sess-1"), `{
		"summary": "Find source",
		"firstPrompt": "Locate the Grok source",
		"createdAt": "2026-07-08T10:00:00Z"
	}`)

	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)

	session := outcome.Results[0].Result.Session
	assert.Equal(1, session.MessageCount)
	assert.Equal(1, session.UserMessageCount)
	require.Len(outcome.Results[0].Result.Messages, 1)
	assert.Equal(RoleUser, outcome.Results[0].Result.Messages[0].Role)
	assert.Equal("Locate the Grok source", outcome.Results[0].Result.Messages[0].Content)
}

func TestGrokProviderPromptContextClassifiesNonInteractive(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	summary := grokSummaryPath(root, "cwd-key", "sess-1")
	writeGrokFixtureFile(t, summary, `{
		"summary":"Inspect a function",
		"firstPrompt":"Explain this function",
		"createdAt":"2026-07-08T10:00:00Z"
	}`)
	writeGrokFixtureFile(
		t,
		filepath.Join(root, "cwd-key", "sess-1", "prompt_context.json"),
		`{"is_non_interactive":true}`,
	)

	provider := newGrokTestProvider(t, root)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	require.Len(outcome.Results, 1)

	assert.Equal(t, "non-interactive", outcome.Results[0].Result.Session.SessionKind)
}

func TestGrokProviderFingerprintTracksParsedFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	summary := grokSummaryPath(root, "cwd-key", "sess-1")
	signals := filepath.Join(root, "cwd-key", "sess-1", "signals.json")
	updates := filepath.Join(root, "cwd-key", "sess-1", "updates.jsonl")
	chat := filepath.Join(root, "cwd-key", "sess-1", "chat_history.jsonl")
	promptContext := filepath.Join(root, "cwd-key", "sess-1", "prompt_context.json")
	unrelated := filepath.Join(root, "cwd-key", "sess-1", "notes.txt")
	writeGrokFixtureFile(t, summary, `{"summary":"Fingerprint","firstPrompt":"hello","createdAt":"2026-07-08T10:00:00Z"}`)
	writeGrokFixtureFile(t, signals, `{"tokenUsage":{"totalOutputTokens":1}}`)
	writeGrokFixtureFile(t, updates, "{}\n")
	writeGrokFixtureFile(t, chat, "{}\n")
	writeGrokFixtureFile(t, promptContext, `{"is_non_interactive":false}`)
	writeGrokFixtureFile(t, unrelated, "ignored")

	provider := newGrokTestProvider(t, root)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "sess-1",
	})
	require.NoError(err)
	require.True(ok)

	base, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	writeGrokFixtureFile(t, summary, `{"summary":"Fingerprint changed","firstPrompt":"hello","createdAt":"2026-07-08T10:00:00Z"}`)
	afterSummary, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(base.Hash, afterSummary.Hash)

	writeGrokFixtureFile(t, signals, `{"tokenUsage":{"totalOutputTokens":2}}`)
	afterSignals, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(afterSummary.Hash, afterSignals.Hash)

	writeGrokFixtureFile(t, chat, "{\"message\":1}\n")
	afterChat, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(afterSignals.Hash, afterChat.Hash)

	writeGrokFixtureFile(t, updates, "{\"delta\":1}\n")
	afterUpdates, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(afterChat.Hash, afterUpdates.Hash)

	writeGrokFixtureFile(t, promptContext, `{"is_non_interactive":true}`)
	afterPromptContext, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(afterUpdates.Hash, afterPromptContext.Hash)

	writeGrokFixtureFile(t, unrelated, "still ignored")
	afterUnrelated, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.Equal(afterPromptContext.Hash, afterUnrelated.Hash)
}

func TestGrokProviderChangedPathTracksParsedFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	summary := grokSummaryPath(root, "cwd-key", "sess-1")
	writeGrokFixtureFile(t, summary, `{"summary":"Changed path","firstPrompt":"hello","createdAt":"2026-07-08T10:00:00Z"}`)
	provider := newGrokTestProvider(t, root)

	for _, name := range []string{
		"summary.json",
		"signals.json",
		"chat_history.jsonl",
		"prompt_context.json",
	} {
		changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
			Path: filepath.Join(root, "cwd-key", "sess-1", name),
		})
		require.NoError(err)
		require.Len(changed, 1)
		assert.Equal(filepath.Clean(summary), filepath.Clean(changed[0].FingerprintKey))
	}

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: filepath.Join(root, "cwd-key", "sess-1", "updates.jsonl"),
	})
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(filepath.Clean(summary), filepath.Clean(changed[0].FingerprintKey))
}

func TestGrokProviderWatchPlanIncludesParsedCompanions(t *testing.T) {
	root := t.TempDir()
	provider := newGrokTestProvider(t, root)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.ElementsMatch(t,
		[]string{
			"summary.json",
			"signals.json",
			"chat_history.jsonl",
			"updates.jsonl",
			"prompt_context.json",
			"meta.json",
		},
		plan.Roots[0].IncludeGlobs,
	)
}

func TestGrokProviderArtifactBoundaries(t *testing.T) {
	require := require.New(t)

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
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 2)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path: filepath.Join(root, "cwd-key", "not.a.grok.session", "signals.json"),
		},
	)
	require.NoError(err)
	assert.Empty(t, changed)

	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "sess-bad",
	})
	require.NoError(err)
	require.True(ok)
	_, err = provider.Parse(t.Context(), ParseRequest{Source: source})
	require.Error(err)
}

func TestGrokProviderRegistry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentGrok)
	require.True(ok)
	assert.Equal("GROK_DIR", def.EnvVar)
	assert.Equal("grok_dirs", def.ConfigKey)
	assert.Equal("grok:", def.IDPrefix)
	assert.Equal([]string{".grok/sessions"}, def.DefaultDirs)

	factory, ok := ProviderFactoryByType(AgentGrok)
	require.True(ok)
	assert.Equal(AgentGrok, factory.Definition().Type)

	mode, ok := ProviderMigrationModes()[AgentGrok]
	require.True(ok)
	assert.Equal(ProviderMigrationProviderAuthoritative, mode)
	assert.Equal(CapabilitySupported,
		factory.Capabilities().Content.AggregateUsageEvents)
}
