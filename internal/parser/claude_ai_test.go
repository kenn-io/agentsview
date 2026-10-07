package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testExportJSON = `[
  {
    "uuid": "conv-001",
    "name": "Test Chat",
    "summary": "A test conversation",
    "created_at": "2026-01-15T10:00:00.000000Z",
    "updated_at": "2026-01-15T10:30:00.000000Z",
    "account": {"uuid": "acct-1"},
    "chat_messages": [
      {
        "uuid": "msg-001",
        "text": "Hello, how are you?",
        "content": [{"type": "text", "text": "Hello, how are you?"}],
        "sender": "human",
        "created_at": "2026-01-15T10:00:00.000000Z",
        "updated_at": "2026-01-15T10:00:00.000000Z",
        "attachments": [],
        "files": []
      },
      {
        "uuid": "msg-002",
        "text": "I'm doing well, thanks!",
        "content": [{"type": "text", "text": "I'm doing well, thanks!"}],
        "sender": "assistant",
        "created_at": "2026-01-15T10:00:30.000000Z",
        "updated_at": "2026-01-15T10:00:30.000000Z",
        "attachments": [],
        "files": []
      }
    ]
  },
  {
    "uuid": "conv-002",
    "name": "",
    "summary": "",
    "created_at": "2026-01-16T12:00:00.000000Z",
    "updated_at": "2026-01-16T12:05:00.000000Z",
    "account": {"uuid": "acct-1"},
    "chat_messages": []
  },
  {
    "uuid": "conv-003",
    "name": "Second Chat",
    "summary": "",
    "created_at": "2026-01-17T08:00:00.000000Z",
    "updated_at": "2026-01-17T08:10:00.000000Z",
    "account": {"uuid": "acct-1"},
    "chat_messages": [
      {
        "uuid": "msg-003",
        "text": "What is Go?",
        "content": [{"type": "text", "text": "What is Go?"}],
        "sender": "human",
        "created_at": "2026-01-17T08:00:00.000000Z",
        "updated_at": "2026-01-17T08:00:00.000000Z",
        "attachments": [],
        "files": []
      }
    ]
  }
]`

func TestParseClaudeAIExport(t *testing.T) {
	var results []ParseResult
	err := parseClaudeAIExport(
		strings.NewReader(testExportJSON),
		func(r ParseResult) error {
			results = append(results, r)
			return nil
		},
	)
	require.NoError(t, err)

	// conv-002 has no messages, should be skipped.
	require.Len(t, results, 2)

	// First conversation.
	s := results[0].Session
	assert.Equal(t, "claude-ai:conv-001", s.ID)
	assert.Equal(t, "claude.ai", s.Project)
	assert.Equal(t, "local", s.Machine)
	assert.Equal(t, AgentClaudeAI, s.Agent)
	assert.Equal(t, "Hello, how are you?", s.FirstMessage)
	assert.Equal(t, "Test Chat", s.SessionName)
	assert.Equal(t, 2, s.MessageCount)
	assert.Equal(t, 1, s.UserMessageCount)
	assert.Equal(t, "2026-01-15T10:00:00.000000Z",
		s.StartedAt.Format("2006-01-02T15:04:05.000000Z"),
	)

	msgs := results[0].Messages
	require.Len(t, msgs, 2)
	assert.Equal(t, 0, msgs[0].Ordinal)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Hello, how are you?", msgs[0].Content)
	assert.Equal(t, 1, msgs[1].Ordinal)
	assert.Equal(t, RoleAssistant, msgs[1].Role)

	// Third conversation (second result).
	s2 := results[1].Session
	assert.Equal(t, "claude-ai:conv-003", s2.ID)
	assert.Equal(t, 1, s2.MessageCount)
	assert.Equal(t, 1, s2.UserMessageCount)
}

func TestParseClaudeAIExport_ContentBlocks(t *testing.T) {
	input := `[{
		"uuid": "conv-blocks",
		"name": "Block Test",
		"created_at": "2026-01-20T10:00:00.000000Z",
		"updated_at": "2026-01-20T10:05:00.000000Z",
		"account": {"uuid": "acct-1"},
		"chat_messages": [
			{
				"uuid": "m1",
				"text": "This block is not supported on your current device yet.",
				"content": [
					{"type": "text", "text": "First part."},
					{"type": "tool_use", "text": ""},
					{"type": "tool_result", "text": ""},
					{"type": "text", "text": "Second part."}
				],
				"sender": "assistant",
				"created_at": "2026-01-20T10:00:00.000000Z"
			},
			{
				"uuid": "m2",
				"text": "",
				"content": [
					{"type": "thinking", "thinking": "deep thought"},
					{"type": "text", "text": "The answer."}
				],
				"sender": "assistant",
				"created_at": "2026-01-20T10:01:00.000000Z"
			}
		]
	}]`

	var results []ParseResult
	err := parseClaudeAIExport(
		strings.NewReader(input),
		func(r ParseResult) error {
			results = append(results, r)
			return nil
		},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)

	msgs := results[0].Messages

	// Message with tool_use/tool_result blocks should use
	// text blocks, not the truncated top-level text.
	assert.Equal(t, "First part.\n\nSecond part.", msgs[0].Content)
	assert.False(t, msgs[0].HasThinking)

	// Message with thinking block.
	assert.Contains(t, msgs[1].Content, "[Thinking]")
	assert.Contains(t, msgs[1].Content, "deep thought")
	assert.Contains(t, msgs[1].Content, "The answer.")
	assert.True(t, msgs[1].HasThinking)
}

func TestParseClaudeAIExport_AttachmentContent(t *testing.T) {
	input := `[{
		"uuid": "conv-attachments",
		"name": "Attachment Test",
		"created_at": "2026-01-21T10:00:00.000000Z",
		"updated_at": "2026-01-21T10:05:00.000000Z",
		"account": {"uuid": "acct-1"},
		"chat_messages": [
			{
				"uuid": "m1",
				"text": "Here is the base idea.",
				"content": [
					{"type": "text", "text": "Here is the base idea."}
				],
				"sender": "assistant",
				"created_at": "2026-01-21T10:00:00.000000Z",
				"attachments": [
					{
						"file_name": "notes.md",
						"extracted_content": "line one\nline two"
					}
				]
			}
		]
	}]`

	var results []ParseResult
	err := parseClaudeAIExport(
		strings.NewReader(input),
		func(r ParseResult) error {
			results = append(results, r)
			return nil
		},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)

	msgs := results[0].Messages
	require.Len(t, msgs, 1)

	assert.Equal(
		t,
		"Here is the base idea.\n\n[Attachment: notes.md]\nline one\nline two",
		msgs[0].Content,
	)
}

func TestParseClaudeAIExport_AttachmentFallbackPaths(t *testing.T) {
	input := `[{
		"uuid": "conv-attachment-fallbacks",
		"name": "Attachment Fallback Test",
		"created_at": "2026-01-21T11:00:00.000000Z",
		"updated_at": "2026-01-21T11:05:00.000000Z",
		"account": {"uuid": "acct-1"},
		"chat_messages": [
			{
				"uuid": "m1",
				"text": "Top-level text survives.",
				"content": [],
				"sender": "assistant",
				"created_at": "2026-01-21T11:00:00.000000Z",
				"attachments": [
					{
						"extracted_content": "attachment with no filename"
					}
				]
			},
			{
				"uuid": "m2",
				"text": "Fallback text survives too.",
				"content": [
					{"type": "tool_use", "text": ""}
				],
				"sender": "assistant",
				"created_at": "2026-01-21T11:01:00.000000Z",
				"attachments": [
					{
						"extracted_content": "attachment after unsupported block"
					}
				]
			}
		]
	}]`

	var results []ParseResult
	err := parseClaudeAIExport(
		strings.NewReader(input),
		func(r ParseResult) error {
			results = append(results, r)
			return nil
		},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)

	msgs := results[0].Messages
	require.Len(t, msgs, 2)
	assert.Equal(t,
		"Top-level text survives.\n\nattachment with no filename",
		msgs[0].Content,
	)
	assert.Equal(t,
		"Fallback text survives too.\n\nattachment after unsupported block",
		msgs[1].Content,
	)
}

func TestParseClaudeAIExport_IgnoredAttachments(t *testing.T) {
	input := `[{
		"uuid": "conv-attachments-ignored",
		"name": "Attachment Ignore Test",
		"created_at": "2026-01-22T10:00:00.000000Z",
		"updated_at": "2026-01-22T10:05:00.000000Z",
		"account": {"uuid": "acct-1"},
		"chat_messages": [
			{
				"uuid": "m1",
				"text": "User prompt",
				"content": [
					{"type": "text", "text": "User prompt"}
				],
				"sender": "human",
				"created_at": "2026-01-22T10:00:00.000000Z"
			},
			{
				"uuid": "m2",
				"text": "Response kept the same.",
				"content": [
					{"type": "text", "text": "Response kept the same."}
				],
				"sender": "assistant",
				"created_at": "2026-01-22T10:01:00.000000Z",
				"attachments": [
					{"file_name": "empty.txt", "extracted_content": ""},
					{"file_name": "noop.txt", "extracted_content": "   "},
					{"file_name": "missing.txt"},
					{
						"file_name": "metadata.json",
						"mime_type": "application/json",
						"metadata": {"unexpected": "value"}
					}
				]
			}
		]
	}]`

	var results []ParseResult
	err := parseClaudeAIExport(
		strings.NewReader(input),
		func(r ParseResult) error {
			results = append(results, r)
			return nil
		},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)

	msgs := results[0].Messages
	require.Len(t, msgs, 2)

	assert.Equal(t, "User prompt", msgs[0].Content)
	assert.Equal(t, "Response kept the same.", msgs[1].Content)
	assert.NotContains(t, msgs[1].Content, "metadata.json")
	assert.NotContains(t, msgs[1].Content, "unexpected")
}

func TestParseClaudeAIExport_TextFallbackPreservesWhitespace(t *testing.T) {
	input := `[{
		"uuid": "conv-fallback",
		"name": "Fallback Test",
		"created_at": "2026-01-23T10:00:00.000000Z",
		"updated_at": "2026-01-23T10:05:00.000000Z",
		"account": {"uuid": "acct-1"},
		"chat_messages": [
			{
				"uuid": "m1",
				"text": "  keep surrounding whitespace  ",
				"content": [],
				"sender": "assistant",
				"created_at": "2026-01-23T10:00:00.000000Z",
				"attachments": []
			}
		]
	}]`

	var results []ParseResult
	err := parseClaudeAIExport(
		strings.NewReader(input),
		func(r ParseResult) error {
			results = append(results, r)
			return nil
		},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(
		t,
		"  keep surrounding whitespace  ",
		results[0].Messages[0].Content,
	)
}

func TestParseClaudeAIExport_EmptyArray(t *testing.T) {
	var results []ParseResult
	err := parseClaudeAIExport(
		strings.NewReader("[]"),
		func(r ParseResult) error {
			results = append(results, r)
			return nil
		},
	)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestParseClaudeAIExport_NullConversation(t *testing.T) {
	calls := 0
	err := parseClaudeAIExport(strings.NewReader("[null]"), func(ParseResult) error {
		calls++
		return nil
	})
	require.NoError(t, err)
	assert.Zero(t, calls)
}

func TestParseClaudeAIExport_InvalidJSON(t *testing.T) {
	err := parseClaudeAIExport(
		strings.NewReader("{not json"),
		func(r ParseResult) error { return nil },
	)
	require.Error(t, err)
}

func TestParseClaudeAIDetail_SelectedPath(t *testing.T) {
	const question = `{"uuid":"q","parent_message_uuid":"00000000-0000-4000-8000-000000000000","sender":"human","content":[{"type":"text","text":"Question"}]}`
	const first = `{"uuid":"a","parent_message_uuid":"q","sender":"assistant","content":[{"type":"text","text":"First"}]}`
	const retry = `{"uuid":"retry","parent_message_uuid":"q","sender":"assistant","content":[{"type":"text","text":"Retry"}]}`
	const more = `{"uuid":"q2","parent_message_uuid":"a","sender":"human","content":[{"type":"text","text":"More"}]},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","content":[{"type":"text","text":"Answer"}]}`
	const edit = `{"uuid":"edited","parent_message_uuid":"00000000-0000-4000-8000-000000000000","sender":"human","content":[{"type":"text","text":"Edited"}]},{"uuid":"b","parent_message_uuid":"edited","sender":"assistant","content":[{"type":"text","text":"Second"}]}`
	for _, tt := range []struct {
		name, leaf string
		want       []string
	}{
		{"edit", "b", []string{"Edited", "Second"}},
		{"switch back", "a2", []string{"Question", "First", "More", "Answer"}},
		{"retry", "retry", []string{"Question", "Retry"}},
		{"retry switch back", "a", []string{"Question", "First"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := `{"uuid":"tree","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","current_leaf_message_uuid":"` + tt.leaf + `","chat_messages":[` + edit + "," + more + "," + retry + "," + first + "," + question + `]}`
			result, err := ParseClaudeAIDetail([]byte(input))
			require.NoError(t, err)
			require.NotNil(t, result.Session.LastEntryUUID)
			assert.Equal(t, tt.leaf, *result.Session.LastEntryUUID)
			var contents []string
			for i, m := range result.Messages {
				contents = append(contents, m.Content)
				assert.Equal(t, i, m.Ordinal)
			}
			assert.Equal(t, tt.want, contents)
			assert.Equal(t, len(tt.want), result.Session.MessageCount)
		})
	}
}

func TestParseClaudeAIDetail_InvalidTree(t *testing.T) {
	const question = `{"uuid":"q","parent_message_uuid":"00000000-0000-4000-8000-000000000000","sender":"human"}`
	const answer = `{"uuid":"a","parent_message_uuid":"q","sender":"assistant"}`
	for _, tt := range []struct{ name, leaf, messages, err string }{
		{"null leaf", `"current_leaf_message_uuid":null,`, question + "," + answer, "expected current_leaf"},
		{"absent leaf", "", question + "," + answer, "expected current_leaf"},
		{"missing leaf message", `"current_leaf_message_uuid":"missing",`, question + "," + answer, "message missing is missing"},
		{"null parent", `"current_leaf_message_uuid":"a",`, question + `,{"uuid":"a","parent_message_uuid":null}`, "parent must be a string"},
		{"absent parent", `"current_leaf_message_uuid":"a",`, question + `,{"uuid":"a"}`, "parent must be a string"},
		{"numeric parent", `"current_leaf_message_uuid":"a",`, question + `,{"uuid":"a","parent_message_uuid":42}`, "parent must be a string"},
		{"orphan parent", `"current_leaf_message_uuid":"a",`, question + `,{"uuid":"a","parent_message_uuid":"missing"}`, "parent missing is missing"},
		{"cycle", `"current_leaf_message_uuid":"a",`, `{"uuid":"q","parent_message_uuid":"a"},` + answer, "cycle"},
		{"duplicate", `"current_leaf_message_uuid":"a",`, question + "," + answer + "," + answer, "duplicate message uuid a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := `{"uuid":"tree","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z",` + tt.leaf + `"chat_messages":[` + tt.messages + `]}`
			result, err := ParseClaudeAIDetail([]byte(input))
			require.ErrorContains(t, err, tt.err)
			assert.Empty(t, result.Messages)
		})
	}
}

func TestParseClaudeAIExport_FlatNullLeaf(t *testing.T) {
	for _, messages := range []string{
		`{"uuid":"q","sender":"human","text":"Question"},{"uuid":"a","sender":"assistant","text":"Answer"}`,
		`{"uuid":"same","sender":"human","text":"Question"},{"uuid":"same","sender":"assistant","text":"Answer"}`,
		`{"sender":"human","text":"Question","parent_message_uuid":null},{"sender":"assistant","text":"Answer","parent_message_uuid":null}`,
	} {
		input := `[null,{"uuid":"flat","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","current_leaf_message_uuid":null,"chat_messages":[` + messages + `]}]`
		var results []ParseResult
		require.NoError(t, parseClaudeAIExport(strings.NewReader(input), func(r ParseResult) error { results = append(results, r); return nil }))
		require.Len(t, results, 1)
		require.Len(t, results[0].Messages, 2)
		assert.Equal(t, "Question", results[0].Messages[0].Content)
		assert.Equal(t, "Answer", results[0].Messages[1].Content)
	}
}

func TestParseClaudeAIExport_IgnoresTreeFields(t *testing.T) {
	for _, messages := range []string{
		`{"uuid":"q","parent_message_uuid":"00000000-0000-4000-8000-000000000000","sender":"human","text":"Question"},{"uuid":"a","parent_message_uuid":"q","sender":"assistant","text":"First"},{"uuid":"b","parent_message_uuid":"q","sender":"assistant","text":"Second"}`,
		`{"uuid":"same","sender":"human","text":"Question"},{"uuid":"same","sender":"assistant","text":"First"},{"uuid":"b","parent_message_uuid":"missing","sender":"assistant","text":"Second"}`,
		`{"uuid":"q","parent_message_uuid":"b","sender":"human","text":"Question"},{"uuid":"a","parent_message_uuid":"q","sender":"assistant","text":"First"},{"uuid":"b","parent_message_uuid":"a","sender":"assistant","text":"Second"}`,
	} {
		input := `[{"uuid":"export","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","current_leaf_message_uuid":"a","chat_messages":[` + messages + `]}]`
		var results []ParseResult
		require.NoError(t, parseClaudeAIExport(strings.NewReader(input), func(r ParseResult) error { results = append(results, r); return nil }))
		require.Len(t, results, 1)
		require.Len(t, results[0].Messages, 3)
		assert.Equal(t, "Question", results[0].Messages[0].Content)
		assert.Equal(t, "First", results[0].Messages[1].Content)
		assert.Equal(t, "Second", results[0].Messages[2].Content)
		assert.Empty(t, results[0].Messages[2].SourceUUID)
	}
}
