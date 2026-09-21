package parser

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestReasoningEffortClaudeFullAndIncremental(t *testing.T) {
	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", tsEarly),
		`{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","uuid":"a1","requestId":"r1","effort":"high","message":{"model":"claude-test","content":[{"type":"text","text":"answer"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
	path := createTestFile(t, "reasoning-effort-claude.jsonl", initial)
	results, err := parseClaudeSession(path, "proj", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 2)
	assert.Empty(t, results[0].Messages[0].ReasoningEffort)
	assert.Equal(t, "high", results[0].Messages[1].ReasoningEffort)

	info, err := os.Stat(path)
	require.NoError(t, err)
	appended := `{"type":"assistant","timestamp":"2026-01-01T00:00:02Z","uuid":"a2","requestId":"r2","message":{"model":"claude-test","content":[{"type":"text","text":"follow up"}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	newMessages, _, _, err := callParseClaudeSessionFrom(path, info.Size(), 2, "")
	require.NoError(t, err)
	require.Len(t, newMessages, 1)
	assert.Empty(t, newMessages[0].ReasoningEffort,
		"an absent effort must not inherit the previous assistant effort")
}

func TestReasoningEffortCodexContextAndAssistantCarriers(t *testing.T) {
	turn := testjsonl.CodexTurnContextJSON("gpt-test", tsEarlyS1)
	turn = strings.Replace(turn, `"model":"gpt-test"`, `"model":"gpt-test","effort":"xhigh"`, 1)
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("effort-1", "/tmp", "user", tsEarly),
		turn,
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
		testjsonl.CodexMsgJSON("assistant", "answer", tsEarlyS5),
		testjsonl.CodexFunctionCallJSON("shell", "run command", tsLate),
	)
	_, messages := runCodexParserTest(t, "reasoning-effort-codex.jsonl", content, false)
	require.Len(t, messages, 3)
	assert.Empty(t, messages[0].ReasoningEffort)
	assert.Equal(t, "xhigh", messages[1].ReasoningEffort)
	assert.Equal(t, "xhigh", messages[2].ReasoningEffort)
}

func TestReasoningEffortCodexResetAndSeed(t *testing.T) {
	turn := testjsonl.CodexTurnContextJSON("gpt-test", tsEarlyS1)
	turn = strings.Replace(turn, `"model":"gpt-test"`, `"model":"gpt-test","effort":"high"`, 1)
	reset := testjsonl.CodexTurnContextJSON("gpt-test", tsLate)
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("effort-2", "/tmp", "user", tsEarly),
		turn,
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
		reset,
	)
	path := createTestFile(t, "reasoning-effort-seed.jsonl", initial)
	info, err := os.Stat(path)
	require.NoError(t, err)
	more := testjsonl.CodexMsgJSON("assistant", "after reset", tsLateS5) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(more)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	messages, _, _, err := parseCodexTestSessionFrom(t, path, info.Size(), 2, false)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Empty(t, messages[0].ReasoningEffort)
}
