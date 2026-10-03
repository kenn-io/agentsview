package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func nativeExportMessage() db.Message {
	return db.Message{
		Ordinal: 1, Role: "assistant", Content: "界𐐀 [Thinking]\n\nTail",
		ThinkingText: "reason-before\n\nreason-after", ToolResultText: "orphan",
		HasThinking: true, HasToolUse: true,
		ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
			{Kind: "text", End: 18},
			{Kind: "thinking", End: 13},
			{Kind: "tool_call", CallIndex: 0},
			{Kind: "tool_result", End: 6},
			{Kind: "thinking", Start: 15, End: 27},
			{Kind: "text", Start: 20, End: 24},
		}},
		ToolCalls: []db.ToolCall{{
			ToolName: "Bash", Category: "Bash", ToolUseID: "call-one",
			Rendering: "[Bash]\n$ echo command", InputJSON: `{"command":"echo command"}`,
			ResultContent: "attached-output\n\nsecond-output",
			ResultEvents: []db.ToolResultEvent{
				{EventIndex: 0, Source: "tool_result", Content: "attached-output"},
				{EventIndex: 1, Source: "tool_result", Content: "second-output"},
			},
		}},
	}
}

func TestNativeTranscriptExportsPreserveOwnerOrder(t *testing.T) {
	message := nativeExportMessage()
	session := testSession()
	for _, format := range []string{"html", "markdown"} {
		t.Run(format, func(t *testing.T) {
			var output string
			if format == "html" {
				output = generateExportHTML(session, []db.Message{message})
				assert.Equal(t, 2, strings.Count(output, `class="thinking-block"`))
			} else {
				output = generateExportMarkdown(session, []db.Message{message}, exportMarkdownOptions{})
				assert.Equal(t, 2, strings.Count(output, "<thinking>"))
				assert.Equal(t, 1, strings.Count(output, `<tool_call id="call-one"`))
			}
			previous := -1
			for _, needle := range []string{"界𐐀 [Thinking]", "reason-before", "echo command", "attached-output", "second-output", "orphan", "reason-after", "Tail"} {
				index := strings.Index(output, needle)
				require.Greater(t, index, previous, "owner %q missing or out of order", needle)
				previous = index
			}
			assert.Equal(t, 1, strings.Count(output, "attached-output"))
			assert.Equal(t, 1, strings.Count(output, "second-output"))
		})
	}
}

func TestNativeFocusedExportUsesDialogueRatherThanLiteralMarkers(t *testing.T) {
	messages := []db.Message{
		{Ordinal: 0, Role: "user", Content: "Q", ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "text", End: 1}}}},
		{Ordinal: 1, Role: "assistant", Content: "[Thinking]\nAnswer", ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "text", End: 17}}}},
		{Ordinal: 2, Role: "assistant", ThinkingText: "reason", HasThinking: true, ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "thinking", End: 6}}}},
	}
	assert.Equal(t, map[int]bool{0: true, 1: true}, focusedExportOrdinals(messages, "claude"))
	tool := db.Message{
		Ordinal: 3, Role: "assistant", HasToolUse: true,
		ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "tool_call", CallIndex: 0}}},
		ToolCalls:     []db.ToolCall{{ToolName: "Bash", Category: "Bash", Rendering: "[Bash]\n$ echo work"}},
	}
	messages = append(messages, tool)
	assert.Equal(t, map[int]bool{0: true}, focusedExportOrdinals(messages, "claude"))
	assert.Equal(t, map[int]bool{0: true, 1: true}, focusedExportOrdinals(messages, "codex"))
}

func TestNativeTranscriptExportKeepsEmptyAndRedactedReasoning(t *testing.T) {
	for _, thinking := range []string{"", "[redacted]"} {
		message := db.Message{
			Role: "assistant", HasThinking: true, ThinkingText: thinking,
			ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "thinking", End: len(thinking)}}},
		}
		output := generateExportHTML(testSession(), []db.Message{message})
		assert.Equal(t, 1, strings.Count(output, `class="thinking-block"`))
		assert.Contains(t, output, `class="message assistant thinking-only focused-hidden"`)
		markdown := generateExportMarkdown(testSession(), []db.Message{message}, exportMarkdownOptions{})
		assert.Equal(t, 1, strings.Count(markdown, "<thinking>"))
		if thinking != "" {
			assert.Contains(t, output, thinking)
			assert.Contains(t, markdown, thinking)
		}
	}
}

func TestNativeMarkdownExportKeepsStoredImageReferences(t *testing.T) {
	message := nativeExportMessage()
	image := `[{"type":"agentsview_image","version":1,"image_ref":"asset://asset-one.png","text":"image display","detail":"retained-meta"}]`
	message.ToolCalls[0].ResultContent = image
	message.ToolCalls[0].ResultEvents = []db.ToolResultEvent{{EventIndex: 0, Source: "tool_result", Content: image}}
	output := generateExportMarkdown(testSession(), []db.Message{message}, exportMarkdownOptions{})
	assert.Contains(t, output, image)
	assert.Equal(t, 1, strings.Count(output, `"image_ref":"asset://asset-one.png"`))
}
