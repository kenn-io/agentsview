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
	assert.Equal(t,
		"2026-01-15T10:00:00.000000Z",
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

func TestParseClaudeAIExport_InvalidJSON(t *testing.T) {
	err := parseClaudeAIExport(
		strings.NewReader("{not json"),
		func(r ParseResult) error { return nil },
	)
	require.Error(t, err)
}
