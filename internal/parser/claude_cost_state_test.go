package parser

import (
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// claudeCostStateLine is a cost-state record in the shape Claude Code 2.1
// writes: cumulative totals of its in-process cost tracker.
func claudeCostStateLine(totalUSD, opusUSD string, opusOutput int, unknown bool) string {
	unknownJSON := "false"
	if unknown {
		unknownJSON = "true"
	}
	return `{"type":"cost-state","sessionId":"cost-sess","totalCostUSD":` + totalUSD +
		`,"totalAPIDuration":1000,"totalDuration":2000,"startTime":1794009864110,` +
		`"modelUsage":{"claude-opus-5-5":{"inputTokens":1243,"outputTokens":` +
		strconv.Itoa(opusOutput) + `,"thinkingTokens":400,"cacheReadInputTokens":510115,` +
		`"cacheCreationInputTokens":59688,"cacheCreationEphemeral5mInputTokens":59688,` +
		`"cacheCreationEphemeral1hInputTokens":0,"webSearchRequests":1,"costUSD":` + opusUSD + `},` +
		`"claude-haiku-5-5":{"inputTokens":900,"outputTokens":30,"thinkingTokens":0,` +
		`"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"webSearchRequests":0,"costUSD":0.001}},` +
		`"hasUnknownModelCost":` + unknownJSON + `}`
}

func TestClaudeKeepsLastCostStateAsSessionTotal(t *testing.T) {
	content := testjsonl.JoinJSONL(
		`{"type":"cost-state","sessionId":"cost-sess","totalCostUSD":0,"modelUsage":{},"hasUnknownModelCost":false}`,
		testjsonl.ClaudeUserJSON("hello", "2026-10-01T10:00:00Z"),
		testjsonl.ClaudeAssistantJSON("hi", "2026-10-01T10:00:01Z"),
		claudeCostStateLine("0.25", "0.249", 5000, false),
		claudeCostStateLine("0.5787550000000001", "0.577755", 8616, true),
	)
	sess, _ := runClaudeParserTest(t, "cost-sess.jsonl", content)

	total := sess.SessionTotal
	require.NotNil(t, total, "the last cost-state record is the session total")
	assert.Equal(t, ClaudeCostStateSource, total.Source)
	assert.Equal(t, SubagentScopeIncluded, total.SubagentScope)
	assert.True(t, total.Incomplete, "hasUnknownModelCost is kept")
	require.NotNil(t, total.Cost)
	assert.Equal(t, money.Money{Microdollars: 578_755}, *total.Cost)
	require.Len(t, total.Models, 2)
	haiku, opus := total.Models[0], total.Models[1]
	assert.Equal(t, "claude-haiku-5-5", haiku.Model)
	assert.Equal(t, ParsedSessionTotalModel{
		Model: "claude-opus-5-5", InputTokens: 1243, OutputTokens: 8616,
		ReasoningTokens: 400, CacheCreationInputTokens: 59688,
		CacheReadInputTokens: 510115, WebSearchRequests: 1,
		Cost: &money.Money{Microdollars: 577_755},
	}, opus)
}

func TestClaudeIgnoresEmptyCostState(t *testing.T) {
	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-10-01T10:00:00Z"),
		testjsonl.ClaudeAssistantJSON("hi", "2026-10-01T10:00:01Z"),
		`{"type":"cost-state","sessionId":"cost-sess","totalCostUSD":0,"modelUsage":{},"hasUnknownModelCost":false}`,
	)
	sess, _ := runClaudeParserTest(t, "cost-sess.jsonl", content)
	assert.Nil(t, sess.SessionTotal, "a session that made no call has no total")
}

func TestClaudeIncrementalCostStateNeedsFullParse(t *testing.T) {
	head := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-10-01T10:00:00Z"),
		testjsonl.ClaudeAssistantJSON("hi", "2026-10-01T10:00:01Z"),
	)
	path := createTestFile(t, "cost-sess.jsonl", head)
	info, err := os.Stat(path)
	require.NoError(t, err)

	appendLine := func(line string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		require.NoError(t, err)
		_, err = f.WriteString(line + "\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	appendLine(`{"type":"cost-state","sessionId":"cost-sess","totalCostUSD":0,"modelUsage":{},"hasUnknownModelCost":false}`)
	_, _, _, err = callParseClaudeSessionFrom(path, info.Size(), 2, "")
	require.NoError(t, err, "an empty cost-state record does not need a full parse")

	appendLine(claudeCostStateLine("0.25", "0.249", 5000, false))
	_, _, _, err = callParseClaudeSessionFrom(path, info.Size(), 2, "")
	assert.ErrorIs(t, err, ErrClaudeIncrementalNeedsFullParse,
		"a new total replaces the stored one, which only a full parse writes")
}

func TestClaudeKeepsTotalBeforeTrailingEmptyCostState(t *testing.T) {
	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-10-01T10:00:00Z"),
		testjsonl.ClaudeAssistantJSON("hi", "2026-10-01T10:00:01Z"),
		claudeCostStateLine("0.25", "0.249", 5000, false),
		`{"type":"cost-state","sessionId":"cost-sess","totalCostUSD":0,"modelUsage":{},"hasUnknownModelCost":false}`,
	)
	sess, _ := runClaudeParserTest(t, "cost-sess.jsonl", content)
	require.NotNil(t, sess.SessionTotal, "an empty snapshot does not erase the total")
	require.NotNil(t, sess.SessionTotal.Cost)
	assert.Equal(t, money.Money{Microdollars: 250_000}, *sess.SessionTotal.Cost)
}
