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
	require.ErrorContains(t, err, "expected conversation object")
	assert.Zero(t, calls)
}

func TestParseClaudeAIExport_InvalidJSON(t *testing.T) {
	err := parseClaudeAIExport(
		strings.NewReader("{not json"),
		func(r ParseResult) error { return nil },
	)
	require.Error(t, err)
}

func TestParseClaudeAIExport_SelectedPath(t *testing.T) {
	const root = `00000000-0000-4000-8000-000000000000`
	const question = `{"uuid":"q","parent_message_uuid":"` + root + `","sender":"human","text":"Question"}`
	const first = `{"uuid":"a","parent_message_uuid":"q","sender":"assistant","text":"First"}`
	const second = `{"uuid":"b","parent_message_uuid":"q","sender":"assistant","text":"Second"}`
	for _, tt := range []struct {
		name, leaf, messages, err string
		omitLeaf                  bool
		want                      []string
	}{
		{name: "siblings without leaf select first", omitLeaf: true, messages: question + "," + first + "," + second, want: []string{"Question", "First"}},
		{name: "second sibling leaf", leaf: "b", messages: question + "," + first + "," + second, want: []string{"Question", "Second"}},
		{name: "root sibling leaf", leaf: "b", messages: `{"uuid":"a","parent_message_uuid":"` + root + `","sender":"assistant","text":"First"},{"uuid":"b","parent_message_uuid":"` + root + `","sender":"assistant","text":"Second"}`, want: []string{"Second"}},
		{name: "missing leaf falls back", leaf: "missing", messages: question + "," + first + "," + second, want: []string{"Question", "Second"}},
		{name: "nearest lower index", leaf: "b", messages: `{"uuid":"q","index":0,"sender":"human","text":"Question"},{"uuid":"b","index":5,"parent_message_uuid":null,"sender":"assistant","text":"Second"},{"uuid":"a","index":2,"parent_message_uuid":"","sender":"assistant","text":"First"}`, want: []string{"Question", "First", "Second"}},
		{name: "absent index starts another root", leaf: "b", messages: `{"uuid":"q","index":0,"sender":"human","text":"Question"},{"uuid":"b","sender":"assistant","text":"Second"}`, want: []string{"Second"}},
		{name: "unanswered branch drops", messages: question + "," + first + `,{"uuid":"unused","parent_message_uuid":"q","sender":"human","text":"Unused"}`, want: []string{"Question", "First"}},
		{name: "last unanswered prompt", messages: question + "," + first + `,{"uuid":"old","parent_message_uuid":"a","sender":"human","text":"Old"},{"uuid":"pending","parent_message_uuid":"a","sender":"human","text":"Pending"}`, want: []string{"Question", "First", "Pending"}},
		{name: "leaf unanswered prompt", leaf: "old", messages: question + "," + first + `,{"uuid":"old","parent_message_uuid":"a","sender":"human","text":"Old"},{"uuid":"pending","parent_message_uuid":"a","sender":"human","text":"Pending"}`, want: []string{"Question", "First", "Old"}},
		{name: "null parent pending root", messages: `{"uuid":"pending","parent_message_uuid":null,"sender":"human","text":"Pending"}`, want: []string{"Pending"}},
		{name: "ancestor retained", leaf: "b", messages: question + `,{"uuid":"middle","parent_message_uuid":"q","sender":"human","text":"Middle"},{"uuid":"b","parent_message_uuid":"middle","sender":"assistant","text":"Second"}`, want: []string{"Question", "Middle", "Second"}},
		{name: "unresolved parent", messages: `{"uuid":"q","parent_message_uuid":"missing","sender":"human","text":"Question"},` + first, err: "message q's parent missing is missing"},
		{name: "cycle", leaf: "b", messages: question + "," + first + `,{"uuid":"b","parent_message_uuid":"c","sender":"assistant"},{"uuid":"c","parent_message_uuid":"b","sender":"assistant"}`, err: "cycle"},
		{name: "no root", messages: `{"uuid":"a","parent_message_uuid":"b","sender":"assistant"},{"uuid":"b","parent_message_uuid":"a","sender":"assistant"}`, err: "no root message"},
		{name: "duplicate uuid", messages: question + "," + first + "," + first, err: "duplicate message uuid a"},
		{name: "empty leaf still normalizes", leaf: "", messages: question + "," + first + "," + second, want: []string{"Question", "First"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			leafField := `"current_leaf_message_uuid":"` + tt.leaf + `",`
			if tt.omitLeaf {
				leafField = ""
			}
			input := `[{"uuid":"tree","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z",` + leafField + `"chat_messages":[` + tt.messages + `]}]`
			var results []ParseResult
			err := parseClaudeAIExport(strings.NewReader(input), func(r ParseResult) error { results = append(results, r); return nil })
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				assert.Empty(t, results)
				return
			}
			require.NoError(t, err)
			require.Len(t, results, 1)
			var contents []string
			for i, m := range results[0].Messages {
				contents = append(contents, m.Content)
				assert.Equal(t, i, m.Ordinal)
			}
			assert.Equal(t, tt.want, contents)
			assert.Equal(t, len(tt.want), results[0].Session.MessageCount)
		})
	}
}
