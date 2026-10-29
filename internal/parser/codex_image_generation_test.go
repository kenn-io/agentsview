package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"image/color"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestParseCodexImageGenerationWithoutIDKeepsImageInInput(t *testing.T) {
	png := testjsonl.SolidPNGBase64(2, 2, color.RGBA{B: 200, A: 255})
	item, err := json.Marshal(map[string]any{
		"type":      "response_item",
		"timestamp": "2024-01-01T10:00:02Z",
		"payload": map[string]any{
			"type":           "image_generation_call",
			"status":         "completed",
			"revised_prompt": "A blue square",
			"result":         png,
		},
	})
	require.NoError(t, err)
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("abc-123", "/workspace/project", "user", "2024-01-01T10:00:00Z"),
		testjsonl.CodexMsgJSON("user", "draw a blue square", "2024-01-01T10:00:01Z"),
		string(item),
	)

	_, msgs := runCodexParserTest(t, "test.jsonl", content, false)

	var calls []ParsedToolCall
	var callText []string
	for _, msg := range msgs {
		if len(msg.ToolCalls) > 0 {
			callText = append(callText, msg.Content)
		}
		calls = append(calls, msg.ToolCalls...)
	}
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"[Tool: image_generation]"}, callText,
		"the call's message holds only the marker the viewer hides")
	assert.Equal(t, "image_generation", calls[0].ToolName)
	assert.Empty(t, calls[0].ResultEvents, "a call without an id has no result to attach")
	var input map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(calls[0].InputJSON), &input))
	assert.JSONEq(t, `"`+png+`"`, string(input["result"]), "the image stays in the input")
}
