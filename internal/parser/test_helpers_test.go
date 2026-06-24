package parser

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseChatGPTExport parses a ChatGPT export through the ChatGPT import-only
// provider. It is the test harness replacement for the former
// ParseChatGPTExport free function.
func parseChatGPTExport(
	dir string,
	assets AssetResolver,
	onConversation func(ParseResult) error,
) error {
	p, ok := NewProvider(AgentChatGPT, ProviderConfig{})
	if !ok {
		return fmt.Errorf("chatgpt provider unavailable")
	}
	exporter, ok := p.(ChatGPTExportParser)
	if !ok {
		return fmt.Errorf("chatgpt provider does not support exports")
	}
	return exporter.ParseChatGPTExport(dir, assets, onConversation)
}

// parseClaudeAIExport parses a Claude.ai export through the Claude.ai
// import-only provider. It is the test harness replacement for the former
// ParseClaudeAIExport free function.
func parseClaudeAIExport(
	r io.Reader,
	onConversation func(ParseResult) error,
) error {
	p, ok := NewProvider(AgentClaudeAI, ProviderConfig{})
	if !ok {
		return fmt.Errorf("claude.ai provider unavailable")
	}
	exporter, ok := p.(ClaudeAIExportParser)
	if !ok {
		return fmt.Errorf("claude.ai provider does not support exports")
	}
	return exporter.ParseClaudeAIExport(r, onConversation)
}

// Timestamp constants for test data.
const (
	tsZero    = "2024-01-01T00:00:00Z"
	tsZeroS1  = "2024-01-01T00:00:01Z"
	tsZeroS2  = "2024-01-01T00:00:02Z"
	tsEarly   = "2024-01-01T10:00:00Z"
	tsEarlyS1 = "2024-01-01T10:00:01Z"
	tsEarlyS5 = "2024-01-01T10:00:05Z"
	tsLate    = "2024-01-01T10:01:00Z"
	tsLateS5  = "2024-01-01T10:01:05Z"
)

// Parsed time.Time values used as expected results in
// timestamp parsing tests.
var testJan15_1030UTC = time.Date(
	2024, 1, 15, 10, 30, 0, 0, time.UTC,
)

// --- Data Generators ---

func generateLargeString(size int) string {
	return strings.Repeat("x", size)
}

// --- Assertions ---

func assertSessionMeta(t *testing.T, s *ParsedSession, wantID, wantProject string, wantAgent AgentType) {
	t.Helper()
	require.NotNil(t, s, "session is nil")
	assert.Equal(t, wantID, s.ID, "session ID")
	assert.Equal(t, wantProject, s.Project, "project")
	assert.Equal(t, wantAgent, s.Agent, "agent")
}

func assertMessage(t *testing.T, m ParsedMessage, wantRole RoleType, wantContentSnippet string) {
	t.Helper()
	assert.Equal(t, wantRole, m.Role, "role")
	if wantContentSnippet != "" {
		assert.Contains(t, m.Content, wantContentSnippet)
	}
}

func assertMessageCount(t *testing.T, count, want int) {
	t.Helper()
	require.Equal(t, want, count, "message count")
}

func assertTimestamp(t *testing.T, got time.Time, want time.Time) {
	t.Helper()
	assert.True(t, got.Equal(want), "timestamp = %v, want %v", got, want)
}

func assertZeroTimestamp(
	t *testing.T, ts time.Time, label string,
) {
	t.Helper()
	assert.True(t, ts.IsZero(), "%s = %v, want zero", label, ts)
}

// captureLog redirects log output to a buffer for the
// duration of the test and restores it on cleanup.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

func assertLogContains(
	t *testing.T, buf *bytes.Buffer, substrs ...string,
) {
	t.Helper()
	got := buf.String()
	for _, s := range substrs {
		assert.Contains(t, got, s, "log missing substring %q", s)
	}
}

func assertLogNotContains(
	t *testing.T, buf *bytes.Buffer, substrs ...string,
) {
	t.Helper()
	got := buf.String()
	for _, s := range substrs {
		assert.NotContains(t, got, s, "log should not contain substring %q", s)
	}
}

func assertLogEmpty(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	assert.Zero(t, buf.Len(), "expected no log output, got: %q", buf.String())
}

func assertToolCallField(t *testing.T, i int, field, got, want string) {
	t.Helper()
	assert.Equal(t, want, got, fmt.Sprintf("tool_calls[%d].%s", i, field))
}

func assertToolCall(t *testing.T, i int, got, want ParsedToolCall) {
	t.Helper()
	assertToolCallField(t, i, "ToolName", got.ToolName, want.ToolName)
	assertToolCallField(t, i, "Category", got.Category, want.Category)
	if want.ToolUseID != "" {
		assertToolCallField(t, i, "ToolUseID", got.ToolUseID, want.ToolUseID)
	}
	if want.InputJSON != "" {
		assertToolCallField(t, i, "InputJSON", got.InputJSON, want.InputJSON)
	}
	if want.SkillName != "" {
		assertToolCallField(t, i, "SkillName", got.SkillName, want.SkillName)
	}
	assertToolCallField(t, i, "SubagentSessionID", got.SubagentSessionID, want.SubagentSessionID)
}

func assertToolCalls(
	t *testing.T, got, want []ParsedToolCall,
) {
	t.Helper()
	if !assert.Equal(t, len(want), len(got), "tool calls count") {
		return
	}
	for i := range want {
		assertToolCall(t, i, got[i], want[i])
	}
}

func parseClaudeTestFile(
	t *testing.T, name, content, project string,
) (ParsedSession, []ParsedMessage) {
	t.Helper()
	path := createTestFile(t, name, content)
	results, err := parseClaudeSession(
		path, project, "local",
	)
	require.NoError(t, err, "parseClaudeSession")
	require.NotEmpty(t, results, "parseClaudeSession returned no results")
	return results[0].Session, results[0].Messages
}

// parseClaudeSession parses a standalone Claude transcript through the Claude
// provider's upload entry point, honoring the explicit project. It is the
// test harness replacement for the former ParseClaudeSession free function,
// exercising the same provider-owned parse body that production uploads use.
func parseClaudeSession(
	path, project, machine string,
) ([]ParseResult, error) {
	provider, ok := NewProvider(AgentClaude, ProviderConfig{Machine: machine})
	if !ok {
		return nil, fmt.Errorf("claude provider unavailable")
	}
	uploader, ok := provider.(ClaudeUploadParser)
	if !ok {
		return nil, fmt.Errorf("claude provider does not support upload parsing")
	}
	return uploader.ParseUploadedTranscript(path, project, machine)
}
