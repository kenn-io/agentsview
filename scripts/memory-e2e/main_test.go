package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseClaudeTraceCapturesEvidenceToolsAndUsage(t *testing.T) {
	trace := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-haiku-4-5-20251001"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__agentsview__search_content","input":{"query":"ember lantern"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__agentsview__get_messages","input":{"session_id":"source-1"}}]}}`,
		`{"type":"result","result":"Use ember-lantern-731. Source source-1 ordinals 1-3.","duration_ms":1200,"total_cost_usd":0.02,"usage":{"input_tokens":5,"output_tokens":6},"modelUsage":{"claude-haiku-4-5-20251001":{"inputTokens":100,"cacheReadInputTokens":80,"cacheCreationInputTokens":15,"outputTokens":20}}}`,
	}, "\n")

	got, err := parseAgentTrace(clientClaude, []byte(trace))
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5-20251001", got.Model)
	assert.Equal(t, "Use ember-lantern-731. Source source-1 ordinals 1-3.", got.FinalAnswer)
	assert.Equal(t, []string{"search_content", "get_messages"}, got.Tools)
	assert.Equal(t, int64(100), got.InputTokens)
	assert.Equal(t, int64(80), got.CacheReadTokens)
	assert.Equal(t, int64(15), got.CacheWriteTokens)
	assert.Equal(t, int64(20), got.OutputTokens)
	assert.Equal(t, int64(1200), got.DurationMS)
	assert.InDelta(t, 0.02, got.CostUSD, 0.0001)
}

func TestParseCodexTraceCapturesEvidenceToolsAndUsage(t *testing.T) {
	trace := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-1"}`,
		`{"type":"item.completed","item":{"id":"call-1","type":"mcp_tool_call","server":"agentsview","tool":"search_content","status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"call-2","type":"mcp_tool_call","server":"agentsview","tool":"get_messages","status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"msg-1","type":"agent_message","text":"Use violet-keel-884. Source codex:source-1 ordinals 0-2."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":250,"cached_input_tokens":100,"cache_write_input_tokens":25,"output_tokens":30}}`,
	}, "\n")

	got, err := parseAgentTrace(clientCodex, []byte(trace))
	require.NoError(t, err)
	assert.Equal(t, "thread-1", got.SessionID)
	assert.Equal(t, "Use violet-keel-884. Source codex:source-1 ordinals 0-2.", got.FinalAnswer)
	assert.Equal(t, []string{"search_content", "get_messages"}, got.Tools)
	assert.Equal(t, int64(250), got.InputTokens)
	assert.Equal(t, int64(100), got.CacheReadTokens)
	assert.Equal(t, int64(25), got.CacheWriteTokens)
	assert.Equal(t, int64(30), got.OutputTokens)
}

func TestGradeHistoricalCaseRequiresSearchReadAnswerAndCitation(t *testing.T) {
	testCase := behaviorCase{
		Name:             "explicit-decision",
		ExpectedPhrases:  []string{"ember-lantern-731", "lock contention"},
		RequiresHistory:  true,
		RequiresCitation: true,
	}
	parsed := parsedTrace{
		FinalAnswer: "Use ember-lantern-731; copper caused lock contention. " +
			"Source codex:source-1 ordinals 0-3.",
		Tools: []string{"search_content", "get_messages"},
	}

	failures := gradeBehaviorRun(testCase, parsed, "codex:source-1")
	assert.Empty(t, failures)

	parsed.Tools = []string{"search_content"}
	parsed.FinalAnswer = "Use ember-lantern-731."
	failures = gradeBehaviorRun(testCase, parsed, "codex:source-1")
	assert.ElementsMatch(t, []string{
		"did not read source messages",
		"missing expected phrase: lock contention",
		"missing source session citation",
		"missing ordinal range citation",
	}, failures)
}

func TestGradeHistoricalCaseAcceptsBrowserURLAndFormattedOrdinals(t *testing.T) {
	testCase := behaviorCase{
		ExpectedPhrases:  []string{"ember-lantern-731", "lock contention"},
		RequiresHistory:  true,
		RequiresCitation: true,
	}
	parsed := parsedTrace{
		FinalAnswer: "Use ember-lantern-731 because the alternative caused lock contention and a lease-timeout. " +
			"Source: http://127.0.0.1/sessions/codex/source-1. Messages: **0–3**.",
		Tools: []string{"search_content", "get_messages"},
	}
	testCase.ExpectedPhrases = append(testCase.ExpectedPhrases, "lease timeout")

	failures := gradeBehaviorRun(testCase, parsed, "codex:source-1")
	assert.Empty(t, failures)
}

func TestGradeHistoricalCaseRejectsWrongMessageRange(t *testing.T) {
	testCase := behaviorCase{
		ExpectedPhrases:  []string{"ember-lantern-731"},
		RequiresHistory:  true,
		RequiresCitation: true,
	}
	parsed := parsedTrace{
		FinalAnswer: "Use ember-lantern-731. Source codex:source-1, messages 1-2.",
		Tools:       []string{"search_content", "get_messages"},
	}

	failures := gradeBehaviorRun(testCase, parsed, "codex:source-1")
	assert.Equal(t, []string{"citation range does not cover source evidence"}, failures)
}

func TestImplicitCaseDoesNotTellTheAgentToRecall(t *testing.T) {
	var prompt string
	for _, testCase := range behaviorCases() {
		if testCase.Name == "implicit-failure" {
			prompt = testCase.Prompt
			break
		}
	}

	require.NotEmpty(t, prompt)
	assert.Contains(t, prompt, "existing concurrency plan")
	for _, cue := range []string{"search", "history", "recall", "conversation"} {
		assert.NotContains(t, strings.ToLower(prompt), cue)
	}
}

func TestGradeCurrentContextCaseRejectsHistoricalSearch(t *testing.T) {
	testCase := behaviorCase{
		Name:            "current-context",
		ExpectedPhrases: []string{"current-saffron-512"},
		ForbidsHistory:  true,
	}
	parsed := parsedTrace{
		FinalAnswer: "current-saffron-512",
		Tools:       []string{"search_content", "get_messages"},
	}

	failures := gradeBehaviorRun(testCase, parsed, "source-1")
	assert.Equal(t, []string{"searched history for a current-context answer"}, failures)
}

func TestNormalizeMemoryToolRejectsOtherMCPServers(t *testing.T) {
	assert.Equal(t, "search_content",
		normalizeMemoryTool("mcp__agentsview__search_content", ""))
	assert.Equal(t, "get_messages",
		normalizeMemoryTool("get_messages", "agentsview"))
	assert.Empty(t, normalizeMemoryTool("search_content", "other"))
	assert.Empty(t, normalizeMemoryTool("mcp__other__search_content", ""))
}
