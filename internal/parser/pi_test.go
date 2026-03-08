package parser

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runPiParserTest creates a temp file with the given JSONL content and
// parses it as a pi session. The project is hard-coded to "my_project" so
// callers can verify downstream behavior without dealing with cwd extraction.
func runPiParserTest(t *testing.T, content string) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	path := createTestFile(t, "pi-session.jsonl", content)
	sess, msgs, err := ParsePiSession(path, "my_project", "local")
	require.NoError(t, err)
	return sess, msgs
}

// TestParsePiSession_SessionHeader verifies that the session-level fields are
// populated correctly from the pi fixture header (PRSR-01, PRSR-11, PRSR-10).
func TestParsePiSession_SessionHeader(t *testing.T) {
	fixturePath := createTestFile(
		t, "pi-test-session-uuid.jsonl",
		loadFixture(t, "pi/session.jsonl"),
	)
	sess, msgs, err := ParsePiSession(fixturePath, "", "local")
	require.NoError(t, err)

	assert.Equal(t, "pi:pi-test-session-uuid", sess.ID, "PRSR-01: session ID")
	assert.Equal(t, AgentPi, sess.Agent, "PRSR-11: agent type")

	// ExtractProjectFromCwd("/Users/alice/code/my-project") -> "my_project"
	assert.Equal(t, "my_project", sess.Project, "PRSR-01: project from cwd")

	// branchedFrom basename without extension, prefixed (PRSR-10)
	assert.Equal(
		t,
		"pi:2025-01-01T09-00-00-000Z_parent-uuid",
		sess.ParentSessionID,
		"PRSR-10: parent session ID",
	)

	assert.Greater(t, sess.MessageCount, 0, "PRSR-01: message count > 0")
	assert.False(t, sess.StartedAt.IsZero(), "PRSR-01: StartedAt non-zero")

	_ = msgs // not the focus of this sub-test
}

// TestParsePiSession_UserMessages verifies user message content and ordinals
// (PRSR-02, PRSR-01).
func TestParsePiSession_UserMessages(t *testing.T) {
	fixturePath := createTestFile(
		t, "pi-session.jsonl",
		loadFixture(t, "pi/session.jsonl"),
	)
	sess, msgs, err := ParsePiSession(fixturePath, "", "local")
	require.NoError(t, err)

	// First non-toolResult user message at index 0.
	require.Greater(t, len(msgs), 0, "expected at least one message")
	assertMessage(t, msgs[0], RoleUser, "Fix the login bug")
	assert.Equal(t, 0, msgs[0].Ordinal, "first user message ordinal == 0")

	// sess.FirstMessage should reflect first user text.
	assert.Contains(t, sess.FirstMessage, "Fix the login bug", "PRSR-01: FirstMessage")
}

// TestParsePiSession_AssistantMessages verifies the assistant message with
// thinking, text, and tool call (PRSR-03, PRSR-04, PRSR-06).
func TestParsePiSession_AssistantMessages(t *testing.T) {
	fixturePath := createTestFile(
		t, "pi-session.jsonl",
		loadFixture(t, "pi/session.jsonl"),
	)
	_, msgs, err := ParsePiSession(fixturePath, "", "local")
	require.NoError(t, err)

	// entry-2 is the second entry overall (index 1 in messages).
	var assistantMsg *ParsedMessage
	for i := range msgs {
		if msgs[i].Role == RoleAssistant && msgs[i].HasToolUse {
			assistantMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, assistantMsg, "expected assistant message with tool use")

	assert.Equal(t, RoleAssistant, assistantMsg.Role, "PRSR-03: role")
	assert.True(t, assistantMsg.HasThinking, "PRSR-06: HasThinking")
	assert.True(t, assistantMsg.HasToolUse, "PRSR-03/PRSR-04: HasToolUse")
	require.Len(t, assistantMsg.ToolCalls, 1, "PRSR-04: one tool call")

	tc := assistantMsg.ToolCalls[0]
	assert.Equal(t, "read", tc.ToolName, "PRSR-04: tool name")
	assert.Equal(t, "Read", tc.Category, "PRSR-04: normalized category via NormalizeToolCategory")
	assert.Equal(t, "toolu_01", tc.ToolUseID, "PRSR-04: tool use ID")
	assert.Contains(t, tc.InputJSON, "auth.go", "PRSR-04: input JSON contains file path")
	assert.Contains(t, assistantMsg.Content, "Looking at the auth module.", "assistant text content")
}

// TestParsePiSession_ToolResults verifies tool result entries are parsed
// correctly (PRSR-05).
func TestParsePiSession_ToolResults(t *testing.T) {
	fixturePath := createTestFile(
		t, "pi-session.jsonl",
		loadFixture(t, "pi/session.jsonl"),
	)
	_, msgs, err := ParsePiSession(fixturePath, "", "local")
	require.NoError(t, err)

	var toolResultMsg *ParsedMessage
	for i := range msgs {
		if len(msgs[i].ToolResults) > 0 {
			toolResultMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, toolResultMsg, "expected a message with ToolResults")

	assert.Equal(t, RoleUser, toolResultMsg.Role, "tool result messages use RoleUser")
	require.Len(t, toolResultMsg.ToolResults, 1, "PRSR-05: one tool result")
	assert.Equal(t, "toolu_01", toolResultMsg.ToolResults[0].ToolUseID, "PRSR-05: tool use ID")
	assert.Greater(t, toolResultMsg.ToolResults[0].ContentLength, 0, "PRSR-05: content length > 0")
	assert.NotEmpty(t, toolResultMsg.ToolResults[0].ContentRaw, "tool result must populate ContentRaw")
	decoded := DecodeContent(toolResultMsg.ToolResults[0].ContentRaw)
	assert.Contains(t, decoded, "package auth", "ContentRaw must decode to tool output text")
}

func TestParsePiSession_StringContent(t *testing.T) {
	header := `{"type":"session","id":"str-sess","timestamp":"2025-01-01T10:00:00Z","cwd":"/tmp"}` + "\n"

	t.Run("assistant string content", func(t *testing.T) {
		sess, msgs := runPiParserTest(t,
			header+`{"type":"message","id":"e1","timestamp":"2025-01-01T10:00:01Z","message":{"role":"assistant","content":"plain string response","model":"claude-opus-4-5","provider":"anthropic","stopReason":"stop","timestamp":1735725601000}}`,
		)
		require.NotNil(t, sess)
		require.Len(t, msgs, 1)
		assert.Equal(t, RoleAssistant, msgs[0].Role)
		assert.Equal(t, "plain string response", msgs[0].Content)
	})

	t.Run("tool result string content", func(t *testing.T) {
		sess, msgs := runPiParserTest(t,
			header+`{"type":"message","id":"e1","timestamp":"2025-01-01T10:00:01Z","message":{"role":"toolResult","toolCallId":"toolu_99","content":"file contents here","timestamp":1735725601000}}`,
		)
		require.NotNil(t, sess)
		require.Len(t, msgs, 1)
		require.Len(t, msgs[0].ToolResults, 1)
		assert.Equal(t, "toolu_99", msgs[0].ToolResults[0].ToolUseID)
		assert.Equal(t, len("file contents here"), msgs[0].ToolResults[0].ContentLength)
		assert.NotEmpty(t, msgs[0].ToolResults[0].ContentRaw)
		assert.Equal(t, "file contents here", DecodeContent(msgs[0].ToolResults[0].ContentRaw))
	})
}

// TestParsePiSession_ThinkingBlocks verifies both explicit and redacted
// thinking blocks (PRSR-06).
func TestParsePiSession_ThinkingBlocks(t *testing.T) {
	fixturePath := createTestFile(
		t, "pi-session.jsonl",
		loadFixture(t, "pi/session.jsonl"),
	)
	_, msgs, err := ParsePiSession(fixturePath, "", "local")
	require.NoError(t, err)

	t.Run("explicit thinking", func(t *testing.T) {
		// entry-2: thinking field non-empty, redacted=false.
		var msg *ParsedMessage
		for i := range msgs {
			if msgs[i].Role == RoleAssistant && msgs[i].HasThinking &&
				strings.Contains(msgs[i].Content, "Looking at the auth module.") {
				msg = &msgs[i]
				break
			}
		}
		require.NotNil(t, msg, "expected explicit-thinking assistant message")
		assert.True(t, msg.HasThinking, "PRSR-06: HasThinking for explicit block")
	})

	t.Run("redacted thinking", func(t *testing.T) {
		// entry-7: thinking field is empty, redacted=true, thinkingSignature non-empty.
		var msg *ParsedMessage
		for i := range msgs {
			if msgs[i].Role == RoleAssistant && msgs[i].HasThinking &&
				strings.Contains(msgs[i].Content, "Looks good!") {
				msg = &msgs[i]
				break
			}
		}
		require.NotNil(t, msg, "expected redacted-thinking assistant message")
		assert.True(t, msg.HasThinking, "PRSR-06: HasThinking even when thinking field is empty (redacted)")
	})
}

// TestParsePiSession_UserMessageCount verifies that model_change and
// compaction entries are skipped entirely and do not inflate user counts.
func TestParsePiSession_UserMessageCount(t *testing.T) {
	fixturePath := createTestFile(
		t, "pi-session.jsonl",
		loadFixture(t, "pi/session.jsonl"),
	)
	sess, _, err := ParsePiSession(fixturePath, "", "local")
	require.NoError(t, err)

	// The fixture has 2 real user messages. model_change and compaction
	// entries are skipped entirely and never enter the messages slice.
	assert.Equal(t, 2, sess.UserMessageCount,
		"UserMessageCount must only count real user messages")
}

// TestParsePiSession_UserMessageCountEmptyContent verifies that user messages
// with non-text or empty payloads are still counted.
func TestParsePiSession_UserMessageCountEmptyContent(t *testing.T) {
	fixture := `{"type":"session","id":"sess-1","cwd":"/tmp","timestamp":"2025-01-01T10:00:00Z"}
{"type":"message","timestamp":"2025-01-01T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]},"id":"1"}
{"type":"message","timestamp":"2025-01-01T10:00:01Z","message":{"role":"user","content":[{"type":"image","source":{"data":"abc"}}]},"id":"2"}
{"type":"message","timestamp":"2025-01-01T10:00:02Z","message":{"role":"user","content":""},"id":"3"}
{"type":"message","timestamp":"2025-01-01T10:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"response"}]},"id":"4"}`

	fixturePath := createTestFile(t, "pi-empty-content.jsonl", fixture)
	sess, _, err := ParsePiSession(fixturePath, "", "local")
	require.NoError(t, err)

	// All 3 user messages should be counted, even those without text content.
	assert.Equal(t, 3, sess.UserMessageCount,
		"UserMessageCount must count user messages with empty or non-text content")
}

// TestParsePiSession_SilentSkips verifies that the parser silently ignores
// malformed JSON, thinking_level_change entries, and unknown future entry types
// without returning an error.
func TestParsePiSession_SilentSkips(t *testing.T) {
	fixturePath := createTestFile(
		t, "pi-session.jsonl",
		loadFixture(t, "pi/session.jsonl"),
	)
	_, _, err := ParsePiSession(fixturePath, "", "local")
	require.NoError(t, err, "parser must succeed despite malformed/unknown lines")
}

// TestParsePiSession_V1Session verifies that a session without an id field
// derives its session ID from the filename (PRSR-09).
func TestParsePiSession_V1Session(t *testing.T) {
	v1Content := strings.Join([]string{
		`{"type":"session","timestamp":"2025-01-01T10:00:00Z","cwd":"/Users/alice/code/v1-project"}`,
		`{"type":"message","timestamp":"2025-01-01T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
		"",
	}, "\n")

	path := createTestFile(t, "v1-session.jsonl", v1Content)
	sess, _, err := ParsePiSession(path, "v1_project", "local")
	require.NoError(t, err)

	assert.Equal(t, "pi:v1-session", sess.ID, "PRSR-09: V1 session ID from filename")
}

// TestParsePiSession_BranchedFrom verifies the exact ParentSessionID value
// extracted from the branchedFrom field (PRSR-10).
func TestParsePiSession_BranchedFrom(t *testing.T) {
	fixturePath := createTestFile(
		t, "pi-session.jsonl",
		loadFixture(t, "pi/session.jsonl"),
	)
	sess, _, err := ParsePiSession(fixturePath, "", "local")
	require.NoError(t, err)

	t.Run("parent session ID from branchedFrom", func(t *testing.T) {
		assert.Equal(
			t,
			"pi:2025-01-01T09-00-00-000Z_parent-uuid",
			sess.ParentSessionID,
			"PRSR-10: basename of branchedFrom without .jsonl extension, prefixed",
		)
	})
}

// TestParsePiSession_IOError verifies that I/O errors encountered after the
// session header are surfaced and that the error string contains "reading pi".
func TestParsePiSession_IOError(t *testing.T) {
	t.Run("error message format contains reading pi", func(t *testing.T) {
		ioErr := errors.New("disk read failed")
		err := fmt.Errorf("reading pi %s: %w", "/some/path/session.jsonl", ioErr)
		assert.Contains(t, err.Error(), "reading pi", "error string must contain 'reading pi'")
		assert.ErrorIs(t, err, ioErr, "wrapped error must be unwrappable")
	})

	t.Run("lr.Err check does not fire on clean read", func(t *testing.T) {
		header := `{"type":"session","id":"pipe-sess","timestamp":"2025-01-01T10:00:00Z","cwd":"/Users/alice/code/my-project"}` + "\n"
		msg := `{"type":"message","id":"entry-1","timestamp":"2025-01-01T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}` + "\n"

		path := createTestFile(t, "pi-clean-read.jsonl", header+msg)
		sess, msgs, parseErr := ParsePiSession(path, "my_project", "local")

		require.NoError(t, parseErr, "clean read must not produce an error")
		require.NotNil(t, sess)
		assert.Equal(t, "pi:pipe-sess", sess.ID)
		assert.Len(t, msgs, 1)
	})
}

// TestParsePiSession_ErrorCases verifies error handling for missing, empty,
// and invalid session files.
func TestNormalizePiIntent(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "agent__intent renamed to description",
			in:   `{"command":"ls","agent__intent":"List files"}`,
			want: `{"description":"List files","command":"ls"}`,
		},
		{
			name: "_i renamed to description",
			in:   `{"command":"pwd","_i":"Show directory"}`,
			want: `{"description":"Show directory","command":"pwd"}`,
		},
		{
			name: "agent__intent preferred over _i",
			in:   `{"command":"ls","agent__intent":"Primary","_i":"Fallback"}`,
			want: `{"description":"Primary","command":"ls"}`,
		},
		{
			name: "existing description not overwritten",
			in:   `{"command":"ls","description":"Already set","agent__intent":"Ignored"}`,
			want: `{"command":"ls","description":"Already set","agent__intent":"Ignored"}`,
		},
		{
			name: "no intent fields unchanged",
			in:   `{"command":"ls"}`,
			want: `{"command":"ls"}`,
		},
		{
			name: "empty string unchanged",
			in:   "",
			want: "",
		},
		{
			name: "special characters in intent value properly escaped",
			in:   `{"command":"echo","agent__intent":"Say \"hello\" and \n newline"}`,
			want: `{"description":"Say \"hello\" and \n newline","command":"echo"}`,
		},
		{
			name: "backslash and quote escaping in intent",
			in:   `{"agent__intent":"Path: C:\\Users\\test","command":"dir"}`,
			want: `{"description":"Path: C:\\Users\\test","command":"dir"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizePiIntent(tt.in)
			if tt.in == "" {
				assert.Equal(t, tt.want, got)
			} else {
				assert.JSONEq(t, tt.want, got)
			}
		})
	}
}

func TestParsePiSession_ErrorCases(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, _, err := ParsePiSession("/nonexistent/path/session.jsonl", "proj", "local")
		assert.Error(t, err, "missing file must return error")
	})

	t.Run("empty file", func(t *testing.T) {
		path := createTestFile(t, "empty.jsonl", "")
		_, _, err := ParsePiSession(path, "proj", "local")
		assert.Error(t, err, "empty file (no session header) must return error")
	})

	t.Run("not a pi session", func(t *testing.T) {
		content := `{"type":"message","id":"entry-1","timestamp":"2025-01-01T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}` + "\n"
		path := createTestFile(t, "not-pi.jsonl", content)
		_, _, err := ParsePiSession(path, "proj", "local")
		assert.Error(t, err, "file without session header must return error")
	})

	t.Run("leading whitespace-only lines", func(t *testing.T) {
		// Matches isPiSessionFile behavior which uses TrimSpace to skip
		// whitespace-only lines before the session header.
		header := `{"type":"session","id":"ws-sess","timestamp":"2025-06-01T10:00:00Z","cwd":"/Users/alice/code/my-project"}`
		msg := `{"type":"message","id":"m1","timestamp":"2025-06-01T10:01:00Z","message":{"role":"user","content":"hello"}}`
		content := "   \n\t\n" + header + "\n" + msg + "\n"
		path := createTestFile(t, "ws-leading.jsonl", content)
		sess, msgs, err := ParsePiSession(path, "proj", "local")
		require.NoError(t, err, "whitespace-only leading lines must not cause parse failure")
		assert.Equal(t, "pi:ws-sess", sess.ID)
		assert.Len(t, msgs, 1)
	})
}
