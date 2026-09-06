package parser

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"

	_ "github.com/mattn/go-sqlite3"
)

// newCopilotTestProvider builds a concrete copilotProvider for the given roots
// so package tests can exercise the folded parse, discovery, and source-lookup
// behavior directly through provider methods, replacing the removed
// package-level entrypoints.
func newCopilotTestProvider(t *testing.T, roots ...string) *copilotProvider {
	t.Helper()
	provider, ok := NewProvider(AgentCopilot, ProviderConfig{
		Roots:   roots,
		Machine: "local",
	})
	require.True(t, ok)
	cp, ok := provider.(*copilotProvider)
	require.True(t, ok)
	return cp
}

// parseCopilotTestSession parses a Copilot JSONL session file at path through
// the provider-owned parse method, replacing the removed package-level
// ParseCopilotSession entrypoint.
func parseCopilotTestSession(
	t *testing.T, path, machine string,
) (*ParsedSession, []ParsedMessage, []ParsedUsageEvent, error) {
	t.Helper()
	return newCopilotTestProvider(t).parseSession(path, machine)
}

// discoverCopilotTestSessions discovers Copilot sessions under root through the
// provider, returning the legacy DiscoveredFile shape (path) the tests assert
// against.
func discoverCopilotTestSessions(t *testing.T, root string) []DiscoveredFile {
	t.Helper()
	provider := newCopilotTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	if len(sources) == 0 {
		return nil
	}
	files := make([]DiscoveredFile, 0, len(sources))
	for _, source := range sources {
		files = append(files, DiscoveredFile{
			Path:  source.DisplayPath,
			Agent: AgentCopilot,
		})
	}
	return files
}

// findCopilotTestSourceFile resolves a Copilot session ID to a session file
// path through the provider, replacing the removed FindCopilotSourceFile.
func findCopilotTestSourceFile(t *testing.T, root, rawID string) string {
	t.Helper()
	return newCopilotTestProvider(t, root).sources.findSourceFile(root, rawID)
}

// writeCopilotJSONL writes JSONL lines to a temp file and
// returns the file path.
func writeCopilotJSONL(
	t *testing.T, lines ...string,
) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test-session.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	require.NoError(t, os.WriteFile(
		path, []byte(content), 0o644,
	))
	return path
}

// parseAndValidateHelper parses the session and fails the test on basic errors.
func parseAndValidateHelper(t *testing.T, path string, machine string, wantMsgs int) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	sess, msgs, _, err := parseCopilotTestSession(t, path, machine)
	require.NoError(t, err)
	require.NotNil(t, sess, "expected non-nil session")
	require.Len(t, msgs, wantMsgs)
	return sess, msgs
}

func assertEqual[T comparable](t *testing.T, want, got T, name string) {
	t.Helper()
	assert.Equal(t, want, got, name)
}

func TestParseCopilotSession_Basic(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"abc-123","context":{"cwd":"/home/alice/code/myproject","branch":"main"}},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Fix the login bug"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"I'll fix the login bug."},"timestamp":"2025-01-15T10:00:02Z"}`,
	)

	sess, msgs := parseAndValidateHelper(t, path, "test-machine", 2)

	assertEqual(t, "copilot:abc-123", sess.ID, "session ID")
	assertEqual(t, AgentCopilot, sess.Agent, "agent")
	assertEqual(t, "test-machine", sess.Machine, "machine")
	assertEqual(t, "myproject", sess.Project, "project")
	assertEqual(t, "Fix the login bug", sess.FirstMessage, "first_message")
	assertEqual(t, 2, sess.MessageCount, "message_count")

	assertEqual(t, RoleUser, msgs[0].Role, "msgs[0].Role")
	assertEqual(t, RoleAssistant, msgs[1].Role, "msgs[1].Role")
	assertEqual(t, "Fix the login bug", msgs[0].Content, "msgs[0].Content")
}

func TestParseCopilotSession_ToolCalls(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"tool-test"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Read the config file"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"tc-1","name":"view","arguments":"{\"path\":\"config.json\"}"}]},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"tc-1"},"timestamp":"2025-01-15T10:00:02.100Z"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"tc-1","success":true,"result":"{\"key\":\"value\"}"},"timestamp":"2025-01-15T10:00:03Z"}`,
		`{"type":"assistant.message","data":{"content":"The config file contains a key-value pair."},"timestamp":"2025-01-15T10:00:04Z"}`,
	)

	_, msgs := parseAndValidateHelper(t, path, "m", 4)

	// Check tool call message.
	tcMsg := msgs[1]
	assert.True(t, tcMsg.HasToolUse, "expected HasToolUse on tool call message")
	assertToolCalls(t, tcMsg.ToolCalls, []ParsedToolCall{{
		ToolName:  "view",
		Category:  "Read",
		ToolUseID: "tc-1",
		InputJSON: `{"path":"config.json"}`,
	}})
	require.Len(t, tcMsg.ToolCalls[0].ResultEvents, 2)
	assert.Equal(t, "started", tcMsg.ToolCalls[0].ResultEvents[0].Status)
	assert.Equal(t, "completed", tcMsg.ToolCalls[0].ResultEvents[1].Status)
	assert.Equal(t, "tool_execution", tcMsg.ToolCalls[0].ResultEvents[1].Source)
	assert.Equal(t, parseTimestamp("2025-01-15T10:00:03Z"),
		tcMsg.ToolCalls[0].ResultEvents[1].Timestamp)

	// Check tool result message.
	trMsg := msgs[2]
	assertEqual(t, 1, len(trMsg.ToolResults), "len(trMsg.ToolResults)")
	assertEqual(t, "tc-1", trMsg.ToolResults[0].ToolUseID, "tool result ID")
	assertEqual(t, 15, trMsg.ToolResults[0].ContentLength, "tool result ContentLength")

	wantTS := parseTimestamp("2025-01-15T10:00:03Z")
	assertEqual(t, wantTS, trMsg.Timestamp, "tool result timestamp")
}

func TestParseCopilotSession_ToolResultTypes(t *testing.T) {
	tests := []struct {
		name        string
		resultJSON  string
		expectedLen int
	}{
		{"Object", `{"files":["a.go","b.go"]}`, 25},
		{"Array", `["one","two","three"]`, 21},
		{"EmptyString", `""`, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeCopilotJSONL(t,
				`{"type":"session.start","data":{"sessionId":"test"},"timestamp":"2025-01-15T10:00:00Z"}`,
				`{"type":"user.message","data":{"content":"cmd"},"timestamp":"2025-01-15T10:00:01Z"}`,
				`{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"tc","name":"ls","arguments":"{}"}]},"timestamp":"2025-01-15T10:00:02Z"}`,
				`{"type":"tool.execution_complete","data":{"toolCallId":"tc","success":true,"result":`+tt.resultJSON+`},"timestamp":"2025-01-15T10:00:03Z"}`,
				`{"type":"assistant.message","data":{"content":"Done."},"timestamp":"2025-01-15T10:00:04Z"}`,
			)

			_, msgs := parseAndValidateHelper(t, path, "m", 4)
			trMsg := msgs[2]

			assertEqual(t, tt.expectedLen, trMsg.ContentLength, "ContentLength")
			assertEqual(t, tt.expectedLen, trMsg.ToolResults[0].ContentLength, "tool result ContentLength")
		})
	}
}

func TestParseCopilotSession_Reasoning(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"reason-test"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Explain the bug"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Here is my analysis.","reasoningText":"Let me think about this carefully..."},"timestamp":"2025-01-15T10:00:02Z"}`,
	)

	_, msgs := parseAndValidateHelper(t, path, "m", 2)

	ast := msgs[1]
	assert.True(t, ast.HasThinking, "expected HasThinking on assistant message with reasoningText")
	assert.Contains(t, ast.Content, "[Thinking]\nLet me think about this carefully...\n[/Thinking]")
	assert.Contains(t, ast.Content, "Here is my analysis.")
	// Thinking block must precede the visible content.
	thinkIdx := strings.Index(ast.Content, "[Thinking]")
	visibleIdx := strings.Index(ast.Content, "Here is my analysis.")
	assert.Less(t, thinkIdx, visibleIdx, "thinking block should appear before visible content")
}

func TestParseCopilotSession_ReasoningOnly(t *testing.T) {
	// A message with only reasoningText and no visible content or tool calls
	// should still be emitted with thinking content.
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"reason-only"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"What do you think?"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"","reasoningText":"Pondering the question..."},"timestamp":"2025-01-15T10:00:02Z"}`,
	)

	_, msgs := parseAndValidateHelper(t, path, "m", 2)

	ast := msgs[1]
	assert.True(t, ast.HasThinking, "expected HasThinking")
	assert.Contains(t, ast.Content, "[Thinking]\nPondering the question...\n[/Thinking]")
}

func TestParseCopilotSession_AssistantReasoningEvent(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"reason-event"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi there."},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"assistant.reasoning","data":{},"timestamp":"2025-01-15T10:00:03Z"}`,
	)

	_, msgs := parseAndValidateHelper(t, path, "m", 2)
	assert.True(t, msgs[1].HasThinking, "expected HasThinking set by assistant.reasoning event")
}

func TestParseCopilotSession_DirectoryFormat(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "abc-456")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))

	content := strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"abc-456"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"hi"},"timestamp":"2025-01-15T10:00:02Z"}`,
	}, "\n") + "\n"

	path := filepath.Join(sessDir, "events.jsonl")
	require.NoError(t, os.WriteFile(
		path, []byte(content), 0o644,
	))

	sess, _ := parseAndValidateHelper(t, path, "m", 2)
	assertEqual(t, "copilot:abc-456", sess.ID, "session ID")
	// No workspace.yaml, so first user message is used.
	assertEqual(t, "hello", sess.FirstMessage, "FirstMessage")
}

// writeDirSession writes events.jsonl (and optionally
// workspace.yaml) into a temporary session directory and
// returns the path to events.jsonl.
func writeDirSession(
	t *testing.T,
	sessID string,
	events []string,
	workspaceYAML string,
) string {
	t.Helper()
	dir := t.TempDir()
	sessDir := filepath.Join(dir, sessID)
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	eventsPath := filepath.Join(sessDir, "events.jsonl")
	require.NoError(t, os.WriteFile(
		eventsPath,
		[]byte(strings.Join(events, "\n")+"\n"),
		0o644,
	))
	if workspaceYAML != "" {
		yamlPath := filepath.Join(sessDir, "workspace.yaml")
		require.NoError(t, os.WriteFile(
			yamlPath, []byte(workspaceYAML), 0o644,
		))
	}
	return eventsPath
}

func TestParseCopilotSession_WorkspaceName(t *testing.T) {
	events := []string{
		`{"type":"session.start","data":{"sessionId":"ws-name"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Fix the login bug"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Done."},"timestamp":"2025-01-15T10:00:02Z"}`,
	}
	yaml := "id: ws-name\nname: Fix Login Authentication Bug\nuser_named: false\nsummary_count: 1\n"

	path := writeDirSession(t, "ws-name", events, yaml)
	sess, _ := parseAndValidateHelper(t, path, "m", 2)

	// workspace.yaml name takes precedence over first user message.
	assertEqual(t, "Fix Login Authentication Bug", sess.FirstMessage, "FirstMessage")
}

func TestParseCopilotSession_WorkspaceNameUserNamed(t *testing.T) {
	events := []string{
		`{"type":"session.start","data":{"sessionId":"ws-user-named"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Original prompt"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Done."},"timestamp":"2025-01-15T10:00:02Z"}`,
	}
	yaml := "id: ws-user-named\nname: My Custom Session Name\nuser_named: true\nsummary_count: 0\n"

	path := writeDirSession(t, "ws-user-named", events, yaml)
	sess, _ := parseAndValidateHelper(t, path, "m", 2)

	// user_named: true sessions also use name as FirstMessage.
	assertEqual(t, "My Custom Session Name", sess.FirstMessage, "FirstMessage")
}

func TestParseCopilotSession_WorkspaceNameMissing(t *testing.T) {
	// workspace.yaml exists but has no name field (older sessions).
	events := []string{
		`{"type":"session.start","data":{"sessionId":"ws-no-name"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"First user message"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Done."},"timestamp":"2025-01-15T10:00:02Z"}`,
	}
	yaml := "id: ws-no-name\nsummary_count: 0\ncreated_at: 2026-03-08T12:38:01.203Z\n"

	path := writeDirSession(t, "ws-no-name", events, yaml)
	sess, _ := parseAndValidateHelper(t, path, "m", 2)

	// Falls back to first user message.
	assertEqual(t, "First user message", sess.FirstMessage, "FirstMessage")
}

func TestParseCopilotSession_WorkspaceNameWhitespaceOnly(t *testing.T) {
	events := []string{
		`{"type":"session.start","data":{"sessionId":"ws-blank"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Do something"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Done."},"timestamp":"2025-01-15T10:00:02Z"}`,
	}
	yaml := "id: ws-blank\nname:   \nsummary_count: 0\n"

	path := writeDirSession(t, "ws-blank", events, yaml)
	sess, _ := parseAndValidateHelper(t, path, "m", 2)

	// Whitespace-only name falls back to first user message.
	assertEqual(t, "Do something", sess.FirstMessage, "FirstMessage")
}

func TestParseCopilotSession_WorkspaceNoYAMLFile(t *testing.T) {
	// Directory format session with no workspace.yaml at all.
	events := []string{
		`{"type":"session.start","data":{"sessionId":"ws-noyaml"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello there"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi."},"timestamp":"2025-01-15T10:00:02Z"}`,
	}

	path := writeDirSession(t, "ws-noyaml", events, "")
	sess, _ := parseAndValidateHelper(t, path, "m", 2)

	assertEqual(t, "Hello there", sess.FirstMessage, "FirstMessage")
}

func TestParseCopilotSession_FlatFileNoWorkspaceYAML(t *testing.T) {
	// Flat .jsonl format never looks for workspace.yaml.
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"flat-sess"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Flat file prompt"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"OK."},"timestamp":"2025-01-15T10:00:02Z"}`,
	)

	sess, _ := parseAndValidateHelper(t, path, "m", 2)

	assertEqual(t, "Flat file prompt", sess.FirstMessage, "FirstMessage")
}

func TestParseCopilotSession_WorkspaceNameTruncated(t *testing.T) {
	longName := strings.Repeat("a", 350)
	events := []string{
		`{"type":"session.start","data":{"sessionId":"ws-long"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"original"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Done."},"timestamp":"2025-01-15T10:00:02Z"}`,
	}
	yaml := "id: ws-long\nname: " + longName + "\nsummary_count: 1\n"

	path := writeDirSession(t, "ws-long", events, yaml)
	sess, _ := parseAndValidateHelper(t, path, "m", 2)

	// truncate(s, 300) returns at most 303 bytes (300 runes + "...").
	assert.LessOrEqual(t, len(sess.FirstMessage), 303, "FirstMessage not truncated")
	assert.NotEqual(t, len(longName), len(sess.FirstMessage), "FirstMessage was not truncated at all")
}

func TestParseCopilotSession_DirectoryFormatFallbackID(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "def-789")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))

	// No session.start event, so ID comes from dir name.
	content := strings.Join([]string{
		`{"type":"user.message","data":{"content":"test"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"ok"},"timestamp":"2025-01-15T10:00:02Z"}`,
	}, "\n") + "\n"

	path := filepath.Join(sessDir, "events.jsonl")
	require.NoError(t, os.WriteFile(
		path, []byte(content), 0o644,
	))

	sess, _ := parseAndValidateHelper(t, path, "m", 2)
	assertEqual(t, "copilot:def-789", sess.ID, "session ID")
}

func TestParseCopilotSession_EmptySession(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"empty"},"timestamp":"2025-01-15T10:00:00Z"}`,
	)

	sess, msgs, _, err := parseCopilotTestSession(t, path, "m")
	require.NoError(t, err)
	assert.Nil(t, sess, "expected nil session for empty")
	assert.Nil(t, msgs, "expected nil messages for empty")
}

func TestParseCopilotSession_NonexistentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.jsonl")

	sess, msgs, _, err := parseCopilotTestSession(t, path, "m")
	require.NoError(t, err, "expected nil error")
	assert.Nil(t, sess, "expected nil session for nonexistent file")
	assert.Nil(t, msgs, "expected nil messages for nonexistent file")
}

func TestParseCopilotSession_ObjectArguments(t *testing.T) {
	// arguments is a native JSON object, not a string.
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"obj-args"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"list"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"tc-5","name":"glob","arguments":{"pattern":"*.go"}}]},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"assistant.message","data":{"content":"done"},"timestamp":"2025-01-15T10:00:03Z"}`,
	)

	_, msgs := parseAndValidateHelper(t, path, "m", 3)

	assertToolCalls(t, msgs[1].ToolCalls, []ParsedToolCall{{
		ToolName:  "glob",
		Category:  "Glob",
		ToolUseID: "tc-5",
		InputJSON: `{"pattern":"*.go"}`,
	}})
}

func TestCopilotUserMessageCount(t *testing.T) {
	// Tool-result user messages (Content == "") should not count
	// as user prompts. This was the exact bug: Copilot emits
	// user-role messages for tool results with empty Content,
	// inflating UserMessageCount.
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"umc-test"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Fix the bug"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"tc-1","name":"view","arguments":"{}"}]},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"tc-1","success":true,"result":"file contents"},"timestamp":"2025-01-15T10:00:03Z"}`,
		`{"type":"assistant.message","data":{"content":"I see the issue."},"timestamp":"2025-01-15T10:00:04Z"}`,
		`{"type":"user.message","data":{"content":"Ship it"},"timestamp":"2025-01-15T10:00:05Z"}`,
		`{"type":"assistant.message","data":{"content":"Done."},"timestamp":"2025-01-15T10:00:06Z"}`,
	)

	sess, _ := parseAndValidateHelper(t, path, "m", 6)

	// Only 2 real user prompts: "Fix the bug" and "Ship it".
	// The tool-result message at index 2 has empty Content.
	assertEqual(t, 2, sess.UserMessageCount, "UserMessageCount")
}

func TestParseCopilotSession_SkipsSyntheticSkillMessages(t *testing.T) {
	tests := []struct {
		name     string
		dataJSON string
	}{
		{
			name:     "SourceAndContent",
			dataJSON: `{"content":"<skill-context name=\"gh-cli\">\nbody\n</skill-context>","source":"skill-gh-cli"}`,
		},
		{
			name:     "SourceOnly",
			dataJSON: `{"content":"skill payload without wrapper","source":"skill-prd"}`,
		},
		{
			name:     "ContentOnly",
			dataJSON: `{"content":"<skill-context name=\"daily-summary\">\nbody\n</skill-context>"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeCopilotJSONL(t,
				`{"type":"session.start","data":{"sessionId":"skill-filter"},"timestamp":"2025-01-15T10:00:00Z"}`,
				`{"type":"user.message","data":`+tt.dataJSON+`,"timestamp":"2025-01-15T10:00:01Z"}`,
				`{"type":"user.message","data":{"content":"Fix the parser"},"timestamp":"2025-01-15T10:00:02Z"}`,
				`{"type":"assistant.message","data":{"content":"Working on it."},"timestamp":"2025-01-15T10:00:03Z"}`,
			)

			sess, msgs := parseAndValidateHelper(t, path, "m", 2)

			assertEqual(t, "Fix the parser", sess.FirstMessage, "FirstMessage")
			assertEqual(t, 1, sess.UserMessageCount, "UserMessageCount")
			assertEqual(t, RoleUser, msgs[0].Role, "msgs[0].Role")
			assertEqual(t, "Fix the parser", msgs[0].Content, "msgs[0].Content")
			assertEqual(t, 0, msgs[0].Ordinal, "msgs[0].Ordinal")
			assertEqual(t, 1, msgs[1].Ordinal, "msgs[1].Ordinal")
		})
	}
}

func TestParseCopilotSession_ModelChange(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"model-test"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"session.model_change","data":{"newModel":"claude-sonnet-4.6"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi there"},"timestamp":"2025-01-15T10:00:03Z"}`,
	)

	_, msgs := parseAndValidateHelper(t, path, "m", 2)

	assertEqual(t, "claude-sonnet-4-6", msgs[1].Model, "msgs[1].Model")
	assertEqual(t, "", msgs[0].Model, "msgs[0].Model")
}

func TestParseCopilotSession_AssistantModel(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"assistant-model"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi there","model":"claude-sonnet-4.6"},"timestamp":"2025-01-15T10:00:02Z"}`,
	)

	_, msgs := parseAndValidateHelper(t, path, "m", 2)

	assert.Equal(t, "claude-sonnet-4-6", msgs[1].Model)
}

func TestParseCopilotSession_NoModel(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"no-model"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi"},"timestamp":"2025-01-15T10:00:02Z"}`,
	)

	_, msgs := parseAndValidateHelper(t, path, "m", 2)
	assertEqual(t, "", msgs[1].Model, "msgs[1].Model")
}

func TestParseCopilotSession_ModelMidSessionChange(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"switch-test"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"session.model_change","data":{"newModel":"claude-sonnet-4.6"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"user.message","data":{"content":"First"},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"assistant.message","data":{"content":"Reply one"},"timestamp":"2025-01-15T10:00:03Z"}`,
		`{"type":"session.model_change","data":{"newModel":"claude-haiku-4.5"},"timestamp":"2025-01-15T10:00:04Z"}`,
		`{"type":"user.message","data":{"content":"Second"},"timestamp":"2025-01-15T10:00:05Z"}`,
		`{"type":"assistant.message","data":{"content":"Reply two"},"timestamp":"2025-01-15T10:00:06Z"}`,
	)

	_, msgs := parseAndValidateHelper(t, path, "m", 4)

	assertEqual(t, "claude-sonnet-4-6", msgs[1].Model, "msgs[1].Model")
	assertEqual(t, "claude-haiku-4-5", msgs[3].Model, "msgs[3].Model")
}

func TestParseCopilotSession_ModelReset(t *testing.T) {
	// An empty newModel clears the active model so
	// subsequent assistant messages have no model.
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"reset-test"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"session.model_change","data":{"newModel":"claude-sonnet-4.6"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"user.message","data":{"content":"First"},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"assistant.message","data":{"content":"Reply one"},"timestamp":"2025-01-15T10:00:03Z"}`,
		`{"type":"session.model_change","data":{"newModel":""},"timestamp":"2025-01-15T10:00:04Z"}`,
		`{"type":"user.message","data":{"content":"Second"},"timestamp":"2025-01-15T10:00:05Z"}`,
		`{"type":"assistant.message","data":{"content":"Reply two"},"timestamp":"2025-01-15T10:00:06Z"}`,
	)

	_, msgs := parseAndValidateHelper(t, path, "m", 4)

	assertEqual(t, "claude-sonnet-4-6", msgs[1].Model, "msgs[1].Model")
	assertEqual(t, "", msgs[3].Model, "msgs[3].Model (reset)")
}

func TestNormalizeCopilotModel(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"claude-sonnet-4.6", "claude-sonnet-4-6"},
		{"claude-haiku-4.5", "claude-haiku-4-5"},
		{"claude-opus-4.7", "claude-opus-4-7"},
		// GPT models use dots in the pricing catalog and must not be changed.
		{"gpt-5.4", "gpt-5.4"},
		{"gpt-5.5", "gpt-5.5"},
		{"gpt-4o", "gpt-4o"},
		{"o3-mini", "o3-mini"},
		{"", ""},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeCopilotModel(tc.input))
		})
	}
}

func TestSessionIDFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/tmp/abc-123.jsonl", "abc-123"},
		{"/tmp/abc-123/events.jsonl", "abc-123"},
		{"/tmp/foo/bar.jsonl", "bar"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := sessionIDFromPath(tt.path)
			assertEqual(t, tt.want, got, "sessionIDFromPath")
		})
	}
}

func TestParseCopilotSession_OutputTokens(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"tok-test","context":{"cwd":"/home/alice/proj","branch":"main"}},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi there.","outputTokens":120},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"user.message","data":{"content":"How are you?"},"timestamp":"2025-01-15T10:00:03Z"}`,
		`{"type":"assistant.message","data":{"content":"I am fine.","outputTokens":85},"timestamp":"2025-01-15T10:00:04Z"}`,
	)

	sess, msgs := parseAndValidateHelper(t, path, "m", 4)

	// Session total should be sum of both assistant messages.
	assert.True(t, sess.HasTotalOutputTokens, "HasTotalOutputTokens")
	assert.Equal(t, 205, sess.TotalOutputTokens, "TotalOutputTokens")

	// Per-message token presence.
	assert.True(t, msgs[1].HasOutputTokens, "msgs[1].HasOutputTokens")
	assert.Equal(t, 120, msgs[1].OutputTokens, "msgs[1].OutputTokens")
	assert.True(t, msgs[3].HasOutputTokens, "msgs[3].HasOutputTokens")
	assert.Equal(t, 85, msgs[3].OutputTokens, "msgs[3].OutputTokens")
}

func TestParseCopilotSession_OutputTokens_Missing(t *testing.T) {
	// When outputTokens is absent, HasOutputTokens must be false.
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"no-tok","context":{"cwd":"/home/alice/proj","branch":"main"}},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi."},"timestamp":"2025-01-15T10:00:02Z"}`,
	)

	sess, msgs := parseAndValidateHelper(t, path, "m", 2)

	assert.False(t, sess.HasTotalOutputTokens, "HasTotalOutputTokens should be false when field absent")
	assert.Equal(t, 0, sess.TotalOutputTokens, "TotalOutputTokens should be zero")
	assert.False(t, msgs[1].HasOutputTokens, "msgs[1].HasOutputTokens should be false")
}

func TestLoadCopilotStoreUsage(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "session-store.db")
	store, err := sql.Open("sqlite3", storePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.Exec(`
		CREATE TABLE assistant_usage_events (
			id INTEGER PRIMARY KEY, session_id TEXT, model TEXT,
			input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER,
			cache_write_tokens INTEGER, reasoning_tokens INTEGER,
			total_nano_aiu INTEGER, created_at TEXT
		);
		INSERT INTO assistant_usage_events VALUES
			(7, 'session-1', 'claude-sonnet-4.6', 100, 30, 60, 10, 4,
			 125050000, '2026-09-04T17:40:54.970Z'),
			(8, 'other-session', 'gpt-5.6-sol', 9, 8, 0, 0, 0,
			 5000000, '2026-09-04T17:41:00Z');
	`)
	require.NoError(t, err)

	events, err := loadCopilotStoreUsage(storePath, "session-1")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "session-store", events[0].Source)
	assert.Equal(t, "claude-sonnet-4-6", events[0].Model)
	assert.Equal(t, 30, events[0].InputTokens)
	assert.Equal(t, 30, events[0].OutputTokens)
	assert.Equal(t, 60, events[0].CacheReadInputTokens)
	assert.Equal(t, 10, events[0].CacheCreationInputTokens)
	assert.Equal(t, 4, events[0].ReasoningTokens)
	assert.Nil(t, events[0].Cost)
	assert.Empty(t, events[0].CostSource)
	assert.Equal(t, "2026-09-04T17:40:54.970Z", events[0].OccurredAt)
	assert.Equal(t, "session-store:7", events[0].DedupKey)
}

func TestParseCopilotSession_StoreUsageSupersedesShutdown(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "session-store.db")
	store, err := sql.Open("sqlite3", storePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.Exec(`
		CREATE TABLE assistant_usage_events (
			id INTEGER PRIMARY KEY, session_id TEXT, model TEXT,
			input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER,
			cache_write_tokens INTEGER, reasoning_tokens INTEGER,
			total_nano_aiu INTEGER, created_at TEXT
		);
		INSERT INTO assistant_usage_events VALUES
			(42, 'store-wins', 'gpt-5.6-sol', 200, 50, 100, 20, 10,
			 250000000, '2026-09-04T17:40:54.970Z');
	`)
	require.NoError(t, err)
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"store-wins"},"timestamp":"2026-09-04T17:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2026-09-04T17:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi.","model":"gpt-5.6-sol","outputTokens":3},"timestamp":"2026-09-04T17:00:02Z"}`,
		`{"type":"session.shutdown","data":{"totalNanoAiu":100000000,"modelMetrics":{"gpt-5.6-sol":{"usage":{"inputTokens":10,"outputTokens":3}}}},"timestamp":"2026-09-04T17:00:03Z"}`,
	)

	sess, _, usage, err := newCopilotTestProvider(t).parseSessionWithStore(
		path, "local", storePath,
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, usage, 2)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 50, sess.TotalOutputTokens)
	assert.Equal(t, "session-store", usage[0].Source)
	assert.Equal(t, 80, usage[0].InputTokens)
	assert.Equal(t, 50, usage[0].OutputTokens)
	assert.Nil(t, usage[0].Cost)
	assert.Empty(t, usage[0].CostSource)
	assert.Equal(t, "2026-09-04T17:40:54.970Z", usage[0].OccurredAt)
	assert.Equal(t, "session-store:42", usage[0].DedupKey)
	assert.Equal(t, "shutdown", usage[1].Source)
	assert.Zero(t, usage[1].InputTokens)
	assert.Zero(t, usage[1].OutputTokens)
	assert.Zero(t, usage[1].CacheCreationInputTokens)
	assert.Zero(t, usage[1].CacheReadInputTokens)
	assert.Zero(t, usage[1].ReasoningTokens)
	require.NotNil(t, usage[1].Cost)
	assert.Equal(t, money.MustParseDollars("0.001"), *usage[1].Cost)
	assert.Equal(t, copilotReportedCostSource, usage[1].CostSource)
	assert.Equal(t, "2026-09-04T17:00:03Z", usage[1].OccurredAt)
}

func TestParseCopilotSession_IncompatibleStoreRetainsShutdownUsage(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "session-store.db")
	store, err := sql.Open("sqlite3", storePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.Exec(`CREATE TABLE unrelated (id INTEGER PRIMARY KEY)`)
	require.NoError(t, err)

	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"store-unavailable"},"timestamp":"2026-09-04T17:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2026-09-04T17:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi.","model":"gpt-5.6-sol","outputTokens":3},"timestamp":"2026-09-04T17:00:02Z"}`,
		`{"type":"session.shutdown","data":{"totalNanoAiu":100000000,"modelMetrics":{"gpt-5.6-sol":{"usage":{"inputTokens":10,"outputTokens":3}}}},"timestamp":"2026-09-04T17:00:03Z"}`,
	)
	logs := captureLog(t)

	sess, _, usage, err := newCopilotTestProvider(t).parseSessionWithStore(
		path, "local", storePath,
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, usage, 1)
	assert.Equal(t, "shutdown", usage[0].Source)
	require.NotNil(t, usage[0].Cost)
	assert.Equal(t, money.MustParseDollars("0.001"), *usage[0].Cost)
	assertLogContains(t, logs, "copilot session store unavailable", "retaining transcript usage")
}

func TestParseCopilotSession_ResumedAfterShutdownFallsBackForUncoveredOutput(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"resumed"},"timestamp":"2026-09-04T17:00:00Z"}`,
		`{"type":"user.message","data":{"content":"First"},"timestamp":"2026-09-04T17:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"First answer","model":"gpt-5.6-sol","outputTokens":3},"timestamp":"2026-09-04T17:00:02Z"}`,
		`{"type":"session.shutdown","data":{"modelMetrics":{"gpt-5.6-sol":{"usage":{"inputTokens":10,"outputTokens":3}}}},"timestamp":"2026-09-04T17:00:03Z"}`,
		`{"type":"user.message","data":{"content":"Resume"},"timestamp":"2026-09-04T17:00:04Z"}`,
		`{"type":"assistant.message","data":{"content":"Second answer","model":"gpt-5.6-sol","outputTokens":7},"timestamp":"2026-09-04T17:00:05Z"}`,
	)

	sess, messages, usage := parseCopilotFull(t, path, "local")

	require.NotNil(t, sess)
	require.Len(t, usage, 1)
	assert.Equal(t, 3, usage[0].OutputTokens)
	require.Len(t, messages, 4)
	assert.Empty(t, messages[1].TokenUsage,
		"shutdown accounting covers the first assistant response")
	assert.Equal(t, `{"output_tokens":7}`, string(messages[3].TokenUsage),
		"the resumed assistant response must retain its known output usage")
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 10, sess.TotalOutputTokens)
}

func TestCopilotProviderStoreChangeRefreshesSession(t *testing.T) {
	root := t.TempDir()
	sessionID := "store-fresh"
	eventsPath := filepath.Join(root, copilotStateDir, sessionID, "events.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(eventsPath), 0o755))
	require.NoError(t, os.WriteFile(eventsPath, []byte(
		`{"type":"session.start","data":{"sessionId":"store-fresh"},"timestamp":"2026-09-04T17:00:00Z"}`+"\n"+
			`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2026-09-04T17:00:01Z"}`+"\n",
	), 0o644))
	storePath := filepath.Join(root, "session-store.db")
	require.NoError(t, os.WriteFile(storePath, []byte("before"), 0o644))
	storeInfo, err := os.Stat(storePath)
	require.NoError(t, err)

	provider := newCopilotTestProvider(t, root)
	hasher, ok := any(provider).(MultiFileStatHasher)
	require.True(t, ok)
	assert.Equal(t, CapabilitySupported,
		provider.Capabilities().Source.MultiFileStatHash)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	before, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	beforeStatHash := hasher.ComputeMultiFileStatHash(eventsPath)

	require.NoError(t, os.WriteFile(storePath, []byte("after!"), 0o644))
	require.NoError(t, os.Chtimes(
		storePath, storeInfo.ModTime(), storeInfo.ModTime(),
	))
	afterStoreInfo, err := os.Stat(storePath)
	require.NoError(t, err)
	assert.Equal(t, storeInfo.Size(), afterStoreInfo.Size())
	assert.Equal(t, storeInfo.ModTime(), afterStoreInfo.ModTime())
	after, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	afterStatHash := hasher.ComputeMultiFileStatHash(eventsPath)
	if beforeStatHash != 0 {
		assert.NotEqual(t, beforeStatHash, afterStatHash,
			"a ctime-aware stat digest must detect a same-size, mtime-preserving store update")
		assert.NotEqual(t, before.Hash, after.Hash,
			"the final fingerprint must retain ctime-aware store changes")
	}
	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)
	assert.Equal(t, []string{"session-store.db", "session-store.db-wal"},
		plan.Roots[1].IncludeGlobs)

	for _, changedPath := range []string{storePath, storePath + "-wal"} {
		changed, err := provider.SourcesForChangedPath(context.Background(),
			ChangedPathRequest{Path: changedPath, EventKind: "write", WatchRoot: root})
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, eventsPath, changed[0].DisplayPath)
	}
}

// parseCopilotFull calls ParseCopilotSession and returns all four values.
func parseCopilotFull(
	t *testing.T, path, machine string,
) (*ParsedSession, []ParsedMessage, []ParsedUsageEvent) {
	t.Helper()
	sess, msgs, usage, err := parseCopilotTestSession(t, path, machine)
	require.NoError(t, err)
	return sess, msgs, usage
}

func TestParseCopilotSession_ShutdownUsageEvents(t *testing.T) {
	shutdownLine := `{"type":"session.shutdown","data":{"totalNanoAiu":1750000000,"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":931647,"outputTokens":7150,"cacheReadTokens":873267,"cacheWriteTokens":51438,"reasoningTokens":432}}}},"timestamp":"2026-06-15T10:01:00Z"}`
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"shut-test","context":{"cwd":"/proj","branch":"main"}},"timestamp":"2026-06-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2026-06-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi."},"timestamp":"2026-06-15T10:00:02Z"}`,
		shutdownLine,
	)

	sess, _, usage := parseCopilotFull(t, path, "m")
	require.NotNil(t, sess)
	require.Len(t, usage, 1)

	u := usage[0]
	assert.Equal(t, "copilot:shut-test", u.SessionID)
	assert.Equal(t, "shutdown", u.Source)
	assert.Equal(t, "claude-sonnet-4-6", u.Model)
	// Fresh input = 931647 - 873267 - 51438 = 6942
	assert.Equal(t, 6942, u.InputTokens, "InputTokens should be fresh only")
	assert.Equal(t, 7150, u.OutputTokens)
	assert.Equal(t, 873267, u.CacheReadInputTokens)
	assert.Equal(t, 51438, u.CacheCreationInputTokens)
	assert.Equal(t, 432, u.ReasoningTokens)
	require.NotNil(t, u.Cost)
	assert.Equal(t, money.MustParseDollars("0.0175"), *u.Cost)
	assert.Equal(t, "exact", u.CostStatus)
	assert.Equal(t, copilotReportedCostSource, u.CostSource)
	assert.Equal(t, "shutdown:copilot:shut-test:claude-sonnet-4-6:0", u.DedupKey)
}

func TestParseCopilotSession_ReportedCostPricingCutoff(t *testing.T) {
	tests := []struct {
		name         string
		startedAt    string
		shutdownAt   string
		wantReported bool
	}{
		{
			name:         "before cutoff",
			startedAt:    "2026-05-31T23:59:59Z",
			shutdownAt:   "2026-06-01T00:01:00Z",
			wantReported: false,
		},
		{
			name:         "exactly at cutoff",
			startedAt:    "2026-06-01T00:00:00Z",
			shutdownAt:   "2026-06-01T00:01:00Z",
			wantReported: true,
		},
		{
			name:         "after cutoff",
			startedAt:    "2026-06-01T00:00:01Z",
			shutdownAt:   "2026-06-01T00:01:00Z",
			wantReported: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeCopilotJSONL(t,
				fmt.Sprintf(`{"type":"session.start","data":{"sessionId":"cutoff"},"timestamp":%q}`, tt.startedAt),
				fmt.Sprintf(`{"type":"user.message","data":{"content":"Hello"},"timestamp":%q}`, tt.startedAt),
				fmt.Sprintf(`{"type":"session.shutdown","data":{"totalNanoAiu":2500000000,"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":100,"outputTokens":50}}}},"timestamp":%q}`, tt.shutdownAt),
			)
			_, _, usage := parseCopilotFull(t, path, "m")
			require.Len(t, usage, 1)
			if tt.wantReported {
				require.NotNil(t, usage[0].Cost)
				assert.Equal(t, money.MustParseDollars("0.025"), *usage[0].Cost)
				assert.Equal(t, "copilot-reported", usage[0].CostSource)
			} else {
				assert.Nil(t, usage[0].Cost)
				assert.Empty(t, usage[0].CostSource)
			}
		})
	}
}

func TestParseCopilotSession_ShutdownMultiModel(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"multi-model","context":{"cwd":"/proj","branch":"main"}},"timestamp":"2026-06-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2026-06-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi."},"timestamp":"2026-06-15T10:00:02Z"}`,
		`{"type":"session.shutdown","data":{"totalNanoAiu":2500000000,"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":100,"outputTokens":50,"cacheReadTokens":60,"cacheWriteTokens":10}},"claude-haiku-4.5":{"usage":{"inputTokens":200,"outputTokens":80,"cacheReadTokens":120,"cacheWriteTokens":20}}}},"timestamp":"2026-06-15T10:01:00Z"}`,
	)

	_, _, usage := parseCopilotFull(t, path, "m")
	require.Len(t, usage, 2)

	byModel := make(map[string]ParsedUsageEvent)
	for _, u := range usage {
		byModel[u.Model] = u
	}

	sonnet := byModel["claude-sonnet-4-6"]
	// fresh = 100 - 60 - 10 = 30
	assert.Equal(t, 30, sonnet.InputTokens)
	assert.Equal(t, 50, sonnet.OutputTokens)

	haiku := byModel["claude-haiku-4-5"]
	// fresh = 200 - 120 - 20 = 60
	assert.Equal(t, 60, haiku.InputTokens)
	assert.Equal(t, 80, haiku.OutputTokens)

	reported := money.Money{}
	carriers := 0
	for _, u := range usage {
		if u.CostSource == copilotReportedCostSource {
			require.NotNil(t, u.Cost)
			reported = money.MustAdd(reported, *u.Cost)
			carriers++
		}
	}
	assert.Equal(t, 1, carriers, "session cost must have one carrier row")
	assert.Equal(t, money.MustParseDollars("0.025"), reported)
}

func TestParseCopilotSession_MultiShutdown_SameModel(t *testing.T) {
	// Sessions with compaction have multiple shutdown events for the
	// same model. All segments must be captured with distinct DedupKeys.
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"multi-shut","context":{"cwd":"/proj","branch":"main"}},"timestamp":"2026-06-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2026-06-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi."},"timestamp":"2026-06-15T10:00:02Z"}`,
		`{"type":"session.shutdown","data":{"totalNanoAiu":1250000000,"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":100,"outputTokens":50,"cacheReadTokens":60,"cacheWriteTokens":10}}}},"timestamp":"2026-06-15T10:01:00Z"}`,
		`{"type":"user.message","data":{"content":"Continue"},"timestamp":"2026-06-15T10:02:00Z"}`,
		`{"type":"assistant.message","data":{"content":"Sure."},"timestamp":"2026-06-15T10:02:01Z"}`,
		`{"type":"session.shutdown","data":{"totalNanoAiu":2750000000,"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":300,"outputTokens":80,"cacheReadTokens":250,"cacheWriteTokens":20}}}},"timestamp":"2026-06-15T10:03:00Z"}`,
	)

	_, _, usage := parseCopilotFull(t, path, "m")
	require.Len(t, usage, 2, "both shutdown segments must be captured")

	assert.Equal(t, "shutdown:copilot:multi-shut:claude-sonnet-4-6:0", usage[0].DedupKey)
	assert.Equal(t, "shutdown:copilot:multi-shut:claude-sonnet-4-6:1", usage[1].DedupKey)

	// First segment: fresh = 100 - 60 - 10 = 30
	assert.Equal(t, 30, usage[0].InputTokens)
	assert.Equal(t, 50, usage[0].OutputTokens)

	// Second segment: fresh = 300 - 250 - 20 = 30
	assert.Equal(t, 30, usage[1].InputTokens)
	assert.Equal(t, 80, usage[1].OutputTokens)
	assert.Nil(t, usage[0].Cost,
		"earlier cumulative shutdown total must be superseded")
	require.NotNil(t, usage[1].Cost)
	assert.Equal(t, money.MustParseDollars("0.0275"), *usage[1].Cost)
	assert.Equal(t, copilotReportedCostSource, usage[1].CostSource)
}

func TestParseCopilotSession_MultiShutdown_MissingTotalPreservesReportedCost(
	t *testing.T,
) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"multi-shut-missing-total","context":{"cwd":"/proj","branch":"main"}},"timestamp":"2026-06-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2026-06-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi."},"timestamp":"2026-06-15T10:00:02Z"}`,
		`{"type":"session.shutdown","data":{"totalNanoAiu":1250000000,"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":100,"outputTokens":50}}}},"timestamp":"2026-06-15T10:01:00Z"}`,
		`{"type":"session.shutdown","data":{"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":200,"outputTokens":80}}}},"timestamp":"2026-06-15T10:03:00Z"}`,
	)

	_, _, usage := parseCopilotFull(t, path, "m")
	require.Len(t, usage, 2)
	require.NotNil(t, usage[0].Cost,
		"shutdown without totalNanoAiu must preserve the last reported total")
	assert.Equal(t, money.MustParseDollars("0.0125"), *usage[0].Cost)
	assert.Equal(t, copilotReportedCostSource, usage[0].CostSource)
	assert.Nil(t, usage[1].Cost)
	assert.Empty(t, usage[1].CostSource)
}

func TestParseCopilotSession_MultiShutdown_InvalidTotalPreservesReportedCost(
	t *testing.T,
) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "null", value: "null"},
		{name: "nonnumeric", value: `"invalid"`},
		{name: "negative", value: "-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeCopilotJSONL(t,
				`{"type":"session.start","data":{"sessionId":"multi-shut-invalid-total","context":{"cwd":"/proj","branch":"main"}},"timestamp":"2026-06-15T10:00:00Z"}`,
				`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2026-06-15T10:00:01Z"}`,
				`{"type":"session.shutdown","data":{"totalNanoAiu":1250000000,"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":100,"outputTokens":50}}}},"timestamp":"2026-06-15T10:01:00Z"}`,
				fmt.Sprintf(`{"type":"session.shutdown","data":{"totalNanoAiu":%s,"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":200,"outputTokens":80}}}},"timestamp":"2026-06-15T10:03:00Z"}`, tt.value),
			)

			_, _, usage := parseCopilotFull(t, path, "m")
			require.Len(t, usage, 2)
			require.NotNil(t, usage[0].Cost)
			assert.Equal(t, money.MustParseDollars("0.0125"), *usage[0].Cost)
			assert.Equal(t, copilotReportedCostSource, usage[0].CostSource)
			assert.Nil(t, usage[1].Cost)
			assert.Empty(t, usage[1].CostSource)
		})
	}
}

func TestParseCopilotSession_MultiShutdown_LastZeroIsAuthoritative(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"multi-shut-zero","context":{"cwd":"/proj","branch":"main"}},"timestamp":"2026-06-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2026-06-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi."},"timestamp":"2026-06-15T10:00:02Z"}`,
		`{"type":"session.shutdown","data":{"totalNanoAiu":1250000000,"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":100,"outputTokens":50}}}},"timestamp":"2026-06-15T10:01:00Z"}`,
		`{"type":"session.shutdown","data":{"totalNanoAiu":0,"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":200,"outputTokens":80}}}},"timestamp":"2026-06-15T10:03:00Z"}`,
	)

	_, _, usage := parseCopilotFull(t, path, "m")
	require.Len(t, usage, 2)
	assert.Nil(t, usage[0].Cost)
	require.NotNil(t, usage[1].Cost)
	assert.Zero(t, *usage[1].Cost)
	assert.Equal(t, copilotReportedCostSource, usage[1].CostSource)
}

func TestParseCopilotSession_ShutdownZeroUsage_Skipped(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"zero-use","context":{"cwd":"/proj","branch":"main"}},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi."},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"session.shutdown","data":{"modelMetrics":{"claude-sonnet-4.6":{"usage":{"inputTokens":0,"outputTokens":0,"cacheReadTokens":0,"cacheWriteTokens":0,"reasoningTokens":0}}}},"timestamp":"2025-01-15T10:01:00Z"}`,
	)

	_, _, usage := parseCopilotFull(t, path, "m")
	assert.Empty(t, usage, "zero-usage model entry should be skipped")
}

func TestParseCopilotSession_NoShutdown_NoUsageEvents(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"no-shut","context":{"cwd":"/proj","branch":"main"}},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi.","model":"gpt-5.6-terra","outputTokens":42},"timestamp":"2025-01-15T10:00:02Z"}`,
	)

	_, msgs, usage := parseCopilotFull(t, path, "m")
	assert.Empty(t, usage, "no shutdown event should produce no usage events")
	assert.Equal(t, "gpt-5.6-terra", msgs[1].Model)
	assert.JSONEq(t, `{"output_tokens":42}`, string(msgs[1].TokenUsage))
}

func TestParseCopilotSession_ShutdownUsageSuppressesMessageFallback(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"shutdown-wins"},"timestamp":"2026-06-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2026-06-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi.","model":"gpt-5.6-terra","outputTokens":42},"timestamp":"2026-06-15T10:00:02Z"}`,
		`{"type":"session.shutdown","data":{"modelMetrics":{"gpt-5.6-terra":{"usage":{"inputTokens":100,"outputTokens":50}}}},"timestamp":"2026-06-15T10:01:00Z"}`,
	)

	_, msgs, usage := parseCopilotFull(t, path, "m")

	require.Len(t, usage, 1)
	assert.Equal(t, 50, usage[0].OutputTokens)
	assert.Empty(t, msgs[1].TokenUsage)
}

func TestParseCopilotSession_ShutdownCoverageWithoutTimestamps(t *testing.T) {
	for _, tc := range []struct {
		name              string
		messageTimestamp  string
		shutdownTimestamp string
	}{
		{"missing shutdown", "2026-06-15T10:00:02Z", ""},
		{"invalid shutdown", "2026-06-15T10:00:02Z", "invalid"},
		{"missing message", "", "2026-06-15T10:01:00Z"},
		{"both missing", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeCopilotJSONL(t,
				`{"type":"session.start","data":{"sessionId":"coverage"},"timestamp":"2026-06-15T10:00:00Z"}`,
				`{"type":"user.message","data":{"content":"Hello"}}`,
				`{"type":"assistant.message","data":{"content":"First","model":"gpt-5.6-terra","outputTokens":42},"timestamp":"`+tc.messageTimestamp+`"}`,
				`{"type":"session.shutdown","data":{"modelMetrics":{"gpt-5.6-terra":{"usage":{"inputTokens":100,"outputTokens":50}}}},"timestamp":"`+tc.shutdownTimestamp+`"}`,
				`{"type":"assistant.message","data":{"content":"Resumed","model":"gpt-5.6-terra","outputTokens":7}}`,
			)
			_, messages, usage := parseCopilotFull(t, path, "local")
			require.Len(t, usage, 1)
			assert.Equal(t, 50, usage[0].OutputTokens)
			require.Len(t, messages, 3)
			assert.Empty(t, messages[1].TokenUsage, "shutdown already includes this response")
			assert.JSONEq(t, `{"output_tokens":7}`, string(messages[2].TokenUsage), "later responses still need fallback usage")
		})
	}
}

func TestParseCopilotSession_StoreCoverageReplacesShutdownCoverage(t *testing.T) {
	for _, tc := range []struct {
		name              string
		shutdownTimestamp string
		storeTimestamp    string
		storeOutput       int
		wantFallback      bool
	}{
		{"stale store", "2026-09-04T17:00:05Z", "2026-09-04T17:00:03Z", 50, true},
		{"missing shutdown timestamp", "", "2026-09-04T17:00:03Z", 50, true},
		{"invalid shutdown timestamp", "invalid", "2026-09-04T17:00:03Z", 50, true},
		{"caught up store", "", "2026-09-04T17:00:04Z", 53, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storePath := filepath.Join(t.TempDir(), "session-store.db")
			store, err := sql.Open("sqlite3", storePath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			_, err = store.Exec(`CREATE TABLE assistant_usage_events (
				id INTEGER PRIMARY KEY, session_id TEXT, model TEXT,
				input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER,
				cache_write_tokens INTEGER, reasoning_tokens INTEGER, created_at TEXT
			)`)
			require.NoError(t, err)
			_, err = store.Exec(`INSERT INTO assistant_usage_events VALUES
				(1, 'store-coverage', 'gpt-5.6-sol', 100, ?, 0, 0, 0, ?)`, tc.storeOutput, tc.storeTimestamp)
			require.NoError(t, err)
			path := writeCopilotJSONL(t,
				`{"type":"session.start","data":{"sessionId":"store-coverage"},"timestamp":"2026-09-04T17:00:00Z"}`,
				`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2026-09-04T17:00:01Z"}`,
				`{"type":"assistant.message","data":{"content":"First","model":"gpt-5.6-sol","outputTokens":50},"timestamp":"2026-09-04T17:00:02Z"}`,
				`{"type":"assistant.message","data":{"content":"Later","model":"gpt-5.6-sol","outputTokens":3},"timestamp":"2026-09-04T17:00:04Z"}`,
				`{"type":"session.shutdown","data":{"modelMetrics":{"gpt-5.6-sol":{"usage":{"inputTokens":100,"outputTokens":53}}}},"timestamp":"`+tc.shutdownTimestamp+`"}`,
			)
			sess, messages, usage, err := newCopilotTestProvider(t).parseSessionWithStore(path, "local", storePath)
			require.NoError(t, err)
			require.NotNil(t, sess)
			assert.Equal(t, 53, sess.TotalOutputTokens)
			require.Len(t, usage, 1, "discarded shutdown tokens must not also be counted")
			assert.Equal(t, "session-store", usage[0].Source)
			assert.Equal(t, tc.storeOutput, usage[0].OutputTokens)
			require.Len(t, messages, 3)
			assert.Empty(t, messages[1].TokenUsage, "store already covers the first response")
			if tc.wantFallback {
				assert.JSONEq(t, `{"output_tokens":3}`, string(messages[2].TokenUsage))
			} else {
				assert.Empty(t, messages[2].TokenUsage, "caught-up store also covers the later response")
			}
		})
	}
}
