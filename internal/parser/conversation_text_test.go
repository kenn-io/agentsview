package parser

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestClaudeVisibleConversationTextExtractionPaths(t *testing.T) {
	t.Parallel()

	entries := mergeClaudeAssistantMessageChunks([]dagEntry{
		{
			uuid:      "user-native",
			entryType: "user",
			line: `{"type":"user","uuid":"user-native","message":{"content":[` +
				`{"type":"text","text":"first"},` +
				`{"type":"thinking","thinking":"private user reasoning"},` +
				`{"type":"tool_use","id":"call-user","name":"Read","input":{"path":"private user path"}},` +
				`{"type":"tool_result","tool_use_id":"call-1","content":"private result"},` +
				`{"type":"text","text":"second"}]}}`,
		},
		{
			uuid:       "assistant-chunk-1",
			parentUuid: "user-native",
			entryType:  "assistant",
			line: `{"type":"assistant","uuid":"assistant-chunk-1",` +
				`"parentUuid":"user-native","message":{"id":"assistant-native","content":[` +
				`{"type":"thinking","thinking":"private reasoning"},` +
				`{"type":"text","text":"working"}]}}`,
		},
		{
			uuid:       "assistant-chunk-2",
			parentUuid: "assistant-chunk-1",
			entryType:  "assistant",
			line: `{"type":"assistant","uuid":"assistant-chunk-2",` +
				`"parentUuid":"assistant-chunk-1","message":{"id":"assistant-native","content":[` +
				`{"type":"thinking","thinking":"private reasoning"},` +
				`{"type":"text","text":"working"},` +
				`{"type":"tool_use","id":"call-2","name":"Read","input":{"path":"private"}},` +
				`{"type":"text","text":"done"}]}}`,
		},
	})

	legacy, _, _ := extractMessagesFrom(entries, 0)
	contextual, _, _, err := extractMessagesContext(context.Background(), entries)
	require.NoError(t, err)

	for name, messages := range map[string][]ParsedMessage{
		"incremental": legacy,
		"full":        contextual,
	} {
		t.Run(name, func(t *testing.T) {
			require.Len(t, messages, 2)

			assertVisibleText(t, messages[0].VisibleText, "first\nsecond")
			assert.Contains(t, messages[0].Content, "private user reasoning")
			assert.Contains(t, messages[0].Content, "private user path")
			assert.Equal(t, "user-native", messages[0].ConversationSourceID)
			assert.True(t, messages[0].Timestamp.IsZero())

			assert.Contains(t, messages[1].Content, "private reasoning")
			assert.Contains(t, messages[1].Content, "private")
			assertVisibleText(t, messages[1].VisibleText, "working\ndone")
			assert.Equal(t, "assistant-native", messages[1].ConversationSourceID)
			assert.Equal(t, "assistant-chunk-2", messages[1].SourceUUID)
			assert.True(t, messages[1].Timestamp.IsZero())
		})
	}
}

func TestClaudeVisibleConversationTextSafetyBoundaries(t *testing.T) {
	t.Parallel()

	entries := []dagEntry{
		{
			uuid:      "ide-native",
			entryType: "user",
			line: `{"type":"user","uuid":"ide-native","message":{"content":` +
				`"<ide_opened_file>The user opened README.md.</ide_opened_file> Explain this file."}}`,
		},
		{
			uuid:      "notice-native",
			entryType: "user",
			line: `{"type":"user","uuid":"notice-native","message":{"content":` +
				`"<task-notification>background work finished</task-notification>"}}`,
		},
		{
			uuid:      "unknown-chunk",
			entryType: "assistant",
			line: `{"type":"assistant","uuid":"unknown-chunk","message":` +
				`{"id":"unknown-native","content":[` +
				`{"type":"future_private_block","text":"must not be trusted"},` +
				`{"type":"text","text":"ordinary prose"}]}}`,
		},
		{
			uuid:      "mixed-ide-native",
			entryType: "user",
			line: `{"type":"user","uuid":"mixed-ide-native","message":{"content":[` +
				`{"type":"thinking","thinking":"private IDE reasoning"},` +
				`{"type":"text","text":"<ide_opened_file>The user opened main.go.</ide_opened_file> Review it."},` +
				`{"type":"tool_use","id":"call-ide","name":"Read","input":{"path":"private IDE path"}}]}}`,
		},
		{
			uuid:      "mixed-notice-native",
			entryType: "user",
			line: `{"type":"user","uuid":"mixed-notice-native","message":{"content":[` +
				`{"type":"thinking","thinking":"private notice reasoning"},` +
				`{"type":"text","text":"<task-notification>background work finished</task-notification>"}]}}`,
		},
	}

	messages, _, _, err := extractMessagesContext(context.Background(), entries)
	require.NoError(t, err)
	require.Len(t, messages, 6)

	assert.True(t, messages[0].IsSystem)
	assertVisibleText(t, messages[0].VisibleText, "")
	assert.Empty(t, messages[0].ConversationSourceID,
		"a split source identity must not identify both derived rows")
	assertVisibleText(t, messages[1].VisibleText, "Explain this file.")
	assert.Equal(t, "ide-native", messages[1].ConversationSourceID)

	assert.True(t, messages[2].IsSystem)
	assertVisibleText(t, messages[2].VisibleText, "")
	assert.Equal(t, "notice-native", messages[2].ConversationSourceID)

	assert.Equal(t, "ordinary prose", messages[3].Content)
	assert.Nil(t, messages[3].VisibleText)
	assert.Equal(t, "unknown-native", messages[3].ConversationSourceID)

	assert.Contains(t, messages[4].Content, "private IDE reasoning")
	assert.Contains(t, messages[4].Content, "private IDE path")
	assertVisibleText(t, messages[4].VisibleText, "Review it.")
	assert.Equal(t, "mixed-ide-native", messages[4].ConversationSourceID)

	assert.False(t, messages[5].IsSystem,
		"conversation projection must not change existing UI classification")
	assert.Contains(t, messages[5].Content, "private notice reasoning")
	assertVisibleText(t, messages[5].VisibleText, "")
	assert.Equal(t, "mixed-notice-native", messages[5].ConversationSourceID)
}

func TestClaudeQueuedConversationTextHasNoInventedIdentity(t *testing.T) {
	t.Parallel()

	message := queuedCommandMessage(claudeQueuedCommand{
		prompt: "queued user prose",
	})

	assertVisibleText(t, message.VisibleText, "queued user prose")
	assert.Empty(t, message.ConversationSourceID)
	assert.True(t, message.Timestamp.IsZero())
}

func TestCodexVisibleConversationText(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"conversation-projection", "/tmp/project", "codex_cli_rs", tsEarly,
		),
		`{"type":"response_item","payload":{"type":"message","id":"user-native",`+
			`"role":"user","content":[`+
			`{"type":"input_text","text":"<recommended_plugins>\nplugin list\n</recommended_plugins>"},`+
			`{"type":"input_text","text":"# AGENTS.md\n<INSTRUCTIONS>\nRepository rules\n</INSTRUCTIONS>"},`+
			`{"type":"input_text","text":"analysis: ordinary pasted log"},`+
			`{"type":"input_image","image_url":"data:image/png;base64,AAAA"},`+
			`{"type":"input_text","text":"same"}]}}`,
		`{"type":"response_item","payload":{"type":"message","id":"commentary-native",`+
			`"role":"assistant","phase":"commentary","content":[`+
			`{"type":"output_text","text":"same"}]}}`,
		`{"type":"response_item","payload":{"type":"message","id":"final-native",`+
			`"role":"assistant","phase":"final_answer","content":[`+
			`{"type":"output_text","text":"same"}]}}`,
		`{"type":"response_item","payload":{"type":"message","id":"analysis-native",`+
			`"role":"assistant","content":[`+
			`{"type":"output_text","text":"private analysis"}]}}`,
		`{"type":"response_item","payload":{"type":"message","id":"legacy-native",`+
			`"role":"assistant","content":[`+
			`{"type":"output_text","text":"unclassified assistant text"}]}}`,
		`{"type":"response_item","payload":{"type":"message","id":"future-native",`+
			`"role":"assistant","phase":"final_answer","content":[`+
			`{"type":"future_private_block","text":"must not be trusted"},`+
			`{"type":"output_text","text":"ordinary prose"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","id":"tool-native",`+
			`"name":"shell_command","call_id":"call-1",`+
			`"arguments":"{\"cmd\":\"private command\"}"}}`,
	)

	_, messages := runCodexParserTest(t, "conversation-projection.jsonl", content, false)
	require.Len(t, messages, 7)

	assertVisibleText(t, messages[0].VisibleText, "analysis: ordinary pasted log\nsame")
	assert.Equal(t, "analysis: ordinary pasted log\nsame", messages[0].Content)
	assert.Equal(t, "user-native", messages[0].ConversationSourceID)
	assert.True(t, messages[0].Timestamp.IsZero())

	assertVisibleText(t, messages[1].VisibleText, "same")
	assert.Equal(t, "commentary-native", messages[1].ConversationSourceID)
	assertVisibleText(t, messages[2].VisibleText, "same")
	assert.Equal(t, "final-native", messages[2].ConversationSourceID)
	assert.NotEqual(t, messages[1].ConversationSourceID, messages[2].ConversationSourceID)

	assert.Equal(t, "private analysis", messages[3].Content)
	assert.Nil(t, messages[3].VisibleText)
	assert.Equal(t, "analysis-native", messages[3].ConversationSourceID)

	assert.Equal(t, "unclassified assistant text", messages[4].Content)
	assert.Nil(t, messages[4].VisibleText)
	assert.Equal(t, "legacy-native", messages[4].ConversationSourceID)

	assert.Equal(t, "ordinary prose", messages[5].Content)
	assert.Nil(t, messages[5].VisibleText)
	assert.Equal(t, "future-native", messages[5].ConversationSourceID)

	assert.Contains(t, messages[6].Content, "private command")
	assertVisibleText(t, messages[6].VisibleText, "")
	assert.Equal(t, "tool-native", messages[6].ConversationSourceID)
}

func TestCompatibleProviderConversationProjectionRemainsUnsupported(t *testing.T) {
	t.Run("Claude-compatible parser", func(t *testing.T) {
		path := createTestFile(t, "compatible.jsonl", testjsonl.JoinJSONL(
			`{"type":"user","uuid":"compatible-native","message":{"content":"hello"}}`,
		))
		results, _, err := claudeParseFile(
			path, "project", "local", claudeParseOptions{},
		)
		require.NoError(t, err)
		require.Len(t, results, 1)
		require.Len(t, results[0].Messages, 1)
		assert.Nil(t, results[0].Messages[0].VisibleText)
		assert.Empty(t, results[0].Messages[0].ConversationSourceID)
	})

	t.Run("Codex-compatible parser", func(t *testing.T) {
		sink := NewCodexCollectingSink(0)
		builder := newCodexSessionBuilder(t.Context(), false, nil, sink)
		builder.conversationProjection = false
		builder.handleResponseItem(gjson.Parse(
			`{"type":"message","id":"compatible-native","role":"assistant",`+
				`"phase":"final_answer","content":[{"type":"output_text","text":"hello"}]}`,
		), time.Time{})

		require.Len(t, sink.Messages(), 1)
		assert.Nil(t, sink.Messages()[0].VisibleText)
		assert.Empty(t, sink.Messages()[0].ConversationSourceID)
	})
}

func assertVisibleText(t *testing.T, got *string, want string) {
	t.Helper()
	require.NotNil(t, got)
	assert.Equal(t, want, *got)
}
