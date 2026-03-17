package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCopilotJSONL writes JSONL lines to a temp file and
// returns the file path.
func writeCopilotJSONL(
	t *testing.T, lines ...string,
) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test-session.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(
		path, []byte(content), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	return path
}

// parseAndValidateHelper parses the session and fails the test on basic errors.
func parseAndValidateHelper(t *testing.T, path string, machine string, wantMsgs int) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	sess, msgs, err := ParseCopilotSession(path, machine)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess == nil {
		t.Fatal("expected non-nil session")
	}
	if len(msgs) != wantMsgs {
		t.Fatalf("got %d messages, want %d", len(msgs), wantMsgs)
	}
	return sess, msgs
}

func assertEqual[T comparable](t *testing.T, want, got T, name string) {
	t.Helper()
	if want != got {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
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
		`{"type":"tool.execution_complete","data":{"toolCallId":"tc-1","success":true,"result":"{\"key\":\"value\"}"},"timestamp":"2025-01-15T10:00:03Z"}`,
		`{"type":"assistant.message","data":{"content":"The config file contains a key-value pair."},"timestamp":"2025-01-15T10:00:04Z"}`,
	)

	_, msgs := parseAndValidateHelper(t, path, "m", 4)

	// Check tool call message.
	tcMsg := msgs[1]
	if !tcMsg.HasToolUse {
		t.Error("expected HasToolUse on tool call message")
	}
	assertToolCalls(t, tcMsg.ToolCalls, []ParsedToolCall{{
		ToolName:  "view",
		Category:  "Read",
		ToolUseID: "tc-1",
		InputJSON: `{"path":"config.json"}`,
	}})

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
	if !ast.HasThinking {
		t.Error("expected HasThinking on assistant message with reasoningText")
	}
	if !strings.Contains(ast.Content, "[Thinking]\nLet me think about this carefully...\n[/Thinking]") {
		t.Errorf("expected thinking block in content, got: %q", ast.Content)
	}
	if !strings.Contains(ast.Content, "Here is my analysis.") {
		t.Errorf("expected visible content after thinking block, got: %q", ast.Content)
	}
	// Thinking block must precede the visible content.
	thinkIdx := strings.Index(ast.Content, "[Thinking]")
	visibleIdx := strings.Index(ast.Content, "Here is my analysis.")
	if thinkIdx >= visibleIdx {
		t.Errorf("thinking block should appear before visible content")
	}
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
	if !ast.HasThinking {
		t.Error("expected HasThinking")
	}
	if !strings.Contains(ast.Content, "[Thinking]\nPondering the question...\n[/Thinking]") {
		t.Errorf("expected thinking block in content, got: %q", ast.Content)
	}
}

func TestParseCopilotSession_AssistantReasoningEvent(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"reason-event"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi there."},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"assistant.reasoning","data":{},"timestamp":"2025-01-15T10:00:03Z"}`,
	)

	_, msgs := parseAndValidateHelper(t, path, "m", 2)
	if !msgs[1].HasThinking {
		t.Error("expected HasThinking set by assistant.reasoning event")
	}
}

func TestParseCopilotSession_DirectoryFormat(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "abc-456")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"abc-456"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"hi"},"timestamp":"2025-01-15T10:00:02Z"}`,
	}, "\n") + "\n"

	path := filepath.Join(sessDir, "events.jsonl")
	if err := os.WriteFile(
		path, []byte(content), 0o644,
	); err != nil {
		t.Fatal(err)
	}

	sess, _ := parseAndValidateHelper(t, path, "m", 2)
	assertEqual(t, "copilot:abc-456", sess.ID, "session ID")
}

func TestParseCopilotSession_DirectoryFormatFallbackID(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "def-789")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// No session.start event, so ID comes from dir name.
	content := strings.Join([]string{
		`{"type":"user.message","data":{"content":"test"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"ok"},"timestamp":"2025-01-15T10:00:02Z"}`,
	}, "\n") + "\n"

	path := filepath.Join(sessDir, "events.jsonl")
	if err := os.WriteFile(
		path, []byte(content), 0o644,
	); err != nil {
		t.Fatal(err)
	}

	sess, _ := parseAndValidateHelper(t, path, "m", 2)
	assertEqual(t, "copilot:def-789", sess.ID, "session ID")
}

func TestParseCopilotSession_EmptySession(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"empty"},"timestamp":"2025-01-15T10:00:00Z"}`,
	)

	sess, msgs, err := ParseCopilotSession(path, "m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess != nil {
		t.Errorf("expected nil session for empty, got %+v", sess)
	}
	if msgs != nil {
		t.Errorf("expected nil messages for empty, got %d", len(msgs))
	}
}

func TestParseCopilotSession_NonexistentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.jsonl")

	sess, msgs, err := ParseCopilotSession(path, "m")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if sess != nil {
		t.Error("expected nil session for nonexistent file")
	}
	if msgs != nil {
		t.Error("expected nil messages for nonexistent file")
	}
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

func TestParseCopilotSession_ModelChange(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"model-test"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"session.model_change","data":{"newModel":"claude-sonnet-4.6"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi there"},"timestamp":"2025-01-15T10:00:03Z"}`,
	)

	sess, msgs := parseAndValidateHelper(t, path, "m", 2)

	// Model should be tracked on assistant message.
	assertEqual(t, "claude-sonnet-4.6", msgs[1].Model, "msgs[1].Model")
	// User messages have no model.
	assertEqual(t, "", msgs[0].Model, "msgs[0].Model")
	// MainModel is the single model used.
	assertEqual(t, "claude-sonnet-4.6", sess.MainModel, "sess.MainModel")
}

func TestParseCopilotSession_NoModel(t *testing.T) {
	// Sessions without a session.model_change event should have
	// empty model fields — model data is simply not available.
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"no-model"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi"},"timestamp":"2025-01-15T10:00:02Z"}`,
	)

	sess, msgs := parseAndValidateHelper(t, path, "m", 2)

	assertEqual(t, "", msgs[1].Model, "msgs[1].Model")
	assertEqual(t, "", sess.MainModel, "sess.MainModel")
}

func TestParseCopilotSession_ModelMidSessionChange(t *testing.T) {
	// Model switches mid-session: first two assistant messages use
	// sonnet, then user switches to haiku for the last one.
	// MainModel should be sonnet (majority).
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"switch-test"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"session.model_change","data":{"newModel":"claude-sonnet-4.6"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"user.message","data":{"content":"First"},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"assistant.message","data":{"content":"Reply one"},"timestamp":"2025-01-15T10:00:03Z"}`,
		`{"type":"user.message","data":{"content":"Second"},"timestamp":"2025-01-15T10:00:04Z"}`,
		`{"type":"assistant.message","data":{"content":"Reply two"},"timestamp":"2025-01-15T10:00:05Z"}`,
		`{"type":"session.model_change","data":{"newModel":"claude-haiku-4.5"},"timestamp":"2025-01-15T10:00:06Z"}`,
		`{"type":"user.message","data":{"content":"Third"},"timestamp":"2025-01-15T10:00:07Z"}`,
		`{"type":"assistant.message","data":{"content":"Reply three"},"timestamp":"2025-01-15T10:00:08Z"}`,
	)

	sess, msgs := parseAndValidateHelper(t, path, "m", 6)

	assertEqual(t, "claude-sonnet-4.6", msgs[1].Model, "msgs[1].Model")
	assertEqual(t, "claude-sonnet-4.6", msgs[3].Model, "msgs[3].Model")
	assertEqual(t, "claude-haiku-4.5", msgs[5].Model, "msgs[5].Model")
	// Majority is sonnet (2 vs 1).
	assertEqual(t, "claude-sonnet-4.6", sess.MainModel, "sess.MainModel")
}

func TestParseCopilotSession_ModelFromMultipleShutdowns(t *testing.T) {
	// A long session with multiple session.shutdown events (Copilot
	// appends one per reconnect). The last shutdown alone shows haiku
	// winning, but accumulated counts show sonnet as dominant.
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"multi-shutdown"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi"},"timestamp":"2025-01-15T10:00:02Z"}`,
		// First shutdown: sonnet=29, haiku=9 → sonnet leads
		`{"type":"session.shutdown","data":{"shutdownType":"routine","modelMetrics":{"claude-sonnet-4.6":{"requests":{"count":29}},"claude-haiku-4.5":{"requests":{"count":9}}}},"timestamp":"2025-01-15T10:01:00Z"}`,
		// Second shutdown: sonnet=137 only
		`{"type":"session.shutdown","data":{"shutdownType":"routine","modelMetrics":{"claude-sonnet-4.6":{"requests":{"count":137}}}},"timestamp":"2025-01-15T10:02:00Z"}`,
		// Third shutdown: haiku=39, sonnet=20 → haiku wins in isolation
		`{"type":"session.shutdown","data":{"shutdownType":"routine","modelMetrics":{"claude-haiku-4.5":{"requests":{"count":39}},"claude-sonnet-4.6":{"requests":{"count":20}}}},"timestamp":"2025-01-15T10:03:00Z"}`,
	)

	// Accumulated: sonnet=29+137+20=186, haiku=9+39=48 → sonnet wins
	sess, _ := parseAndValidateHelper(t, path, "m", 2)
	assertEqual(t, "claude-sonnet-4.6", sess.MainModel, "sess.MainModel")
}

func TestParseCopilotSession_ModelFromShutdown(t *testing.T) {
	// No session.model_change — main model derived from modelMetrics
	// in session.shutdown (sonnet has more requests than haiku).
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"shutdown-test"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi"},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"session.shutdown","data":{"shutdownType":"routine","modelMetrics":{"claude-sonnet-4.6":{"requests":{"count":5,"cost":1},"usage":{"inputTokens":1000,"outputTokens":200}},"claude-haiku-4.5":{"requests":{"count":2,"cost":0},"usage":{"inputTokens":400,"outputTokens":80}}}},"timestamp":"2025-01-15T10:00:10Z"}`,
	)

	sess, _ := parseAndValidateHelper(t, path, "m", 2)
	assertEqual(t, "claude-sonnet-4.6", sess.MainModel, "sess.MainModel")
}

func TestParseCopilotSession_ModelFromShutdownCurrentModel(t *testing.T) {
	// session.shutdown has currentModel but empty modelMetrics.
	// shutdownModelCounts is empty, ComputeMainModel returns ""
	// (no message has model set), so b.currentModel is the final
	// fallback and should be used as MainModel.
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"cur-model-test"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi"},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"session.shutdown","data":{"shutdownType":"routine","currentModel":"claude-sonnet-4.6","modelMetrics":{}},"timestamp":"2025-01-15T10:00:10Z"}`,
	)

	sess, _ := parseAndValidateHelper(t, path, "m", 2)
	assertEqual(t, "claude-sonnet-4.6", sess.MainModel, "sess.MainModel")
}

func TestParseCopilotSession_ModelFromToolComplete(t *testing.T) {
	// tool.execution_complete carries a model field that should
	// backfill the preceding assistant message and update currentModel.
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"tool-model"},"timestamp":"2025-01-15T10:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Do something"},"timestamp":"2025-01-15T10:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"","toolRequests":[{"toolCallId":"tc1","name":"bash","arguments":{"command":"ls"}}]},"timestamp":"2025-01-15T10:00:02Z"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"tc1","result":"file.txt","model":"claude-sonnet-4.6"},"timestamp":"2025-01-15T10:00:03Z"}`,
		`{"type":"assistant.message","data":{"content":"Done"},"timestamp":"2025-01-15T10:00:04Z"}`,
	)

	// 4 messages: user, assistant (tool-only), tool-result user, assistant (final)
	sess, msgs := parseAndValidateHelper(t, path, "m", 4)

	// The tool-only assistant message should be backfilled.
	assertEqual(t, "claude-sonnet-4.6", msgs[1].Model, "msgs[1].Model (backfilled)")
	// The final assistant message gets currentModel set from the tool complete.
	assertEqual(t, "claude-sonnet-4.6", msgs[3].Model, "msgs[3].Model")
	// ComputeMainModel over these 2 assistant messages → sonnet.
	assertEqual(t, "claude-sonnet-4.6", sess.MainModel, "sess.MainModel")
}

func TestComputeMainModel(t *testing.T) {
	tests := []struct {
		name     string
		messages []ParsedMessage
		want     string
	}{
		{
			name:     "empty",
			messages: nil,
			want:     "",
		},
		{
			name: "no model data",
			messages: []ParsedMessage{
				{Role: RoleAssistant, Model: ""},
				{Role: RoleUser, Model: ""},
			},
			want: "",
		},
		{
			name: "single model",
			messages: []ParsedMessage{
				{Role: RoleAssistant, Model: "claude-sonnet-4.6"},
				{Role: RoleUser, Model: ""},
			},
			want: "claude-sonnet-4.6",
		},
		{
			name: "majority wins",
			messages: []ParsedMessage{
				{Role: RoleAssistant, Model: "claude-sonnet-4.6"},
				{Role: RoleAssistant, Model: "claude-sonnet-4.6"},
				{Role: RoleAssistant, Model: "claude-haiku-4.5"},
			},
			want: "claude-sonnet-4.6",
		},
		{
			name: "user role model ignored",
			messages: []ParsedMessage{
				{Role: RoleUser, Model: "some-model"},
				{Role: RoleAssistant, Model: "claude-sonnet-4.6"},
			},
			want: "claude-sonnet-4.6",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeMainModel(tt.messages)
			assertEqual(t, tt.want, got, "ComputeMainModel")
		})
	}
}
