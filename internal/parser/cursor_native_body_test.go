package parser

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCursorNativeLegacyMessageBodies(t *testing.T) {
	path := createTestFile(t, "native.txt", `user:
<user_query>[Thinking] is literal</user_query>
assistant:
[Tool result]
  orphan-output
first
[Thinking]
  plan
[Tool call] ReadFile
  path=input-needle
[Tool result]
  paired-output
after
assistant:
[Thinking]
assistant:
[Tool result]
  final-orphan`)
	sess, msgs := parseCursorTestFile(t, path)
	require.NotNil(t, sess)
	require.Len(t, msgs, 4)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "first\nafter", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, "orphan-output", msgs[1].ToolResultText)
	assert.Equal(t, 11, msgs[1].ContentLength)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "[Tool call] ReadFile\n  path=input-needle", msgs[1].ToolCalls[0].Rendering)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "paired-output", msgs[1].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "tool_result", End: 13},
		{Kind: "text", End: 5},
		{Kind: "thinking", End: 4},
		{Kind: "tool_call"},
		{Kind: "text", Start: 6, End: 11},
	}}, msgs[1].ContentLayout)
	assert.True(t, msgs[2].HasThinking)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[2].ContentLayout)
	assert.Empty(t, msgs[3].Content)
	assert.Equal(t, "final-orphan", msgs[3].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 12}}}, msgs[3].ContentLayout)
}

func TestCursorNativeStoreEnrichmentBody(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "plain"
		if replace {
			name = "replace-existing-thinking"
		}
		t.Run(name, func(t *testing.T) {
			fx := setupCursorStoreFixture(t, false)
			if replace {
				require.NoError(t, os.WriteFile(fx.Transcript, []byte(`{"role":"user","message":{"content":[{"type":"text","text":"is this composer? what model is this?"}]}}
{"role":"assistant","message":{"content":[{"type":"thinking","thinking":"old"},{"type":"text","text":"I'm Auto, an agent router designed by Cursor."}]}}
{"type":"turn_ended","status":"success"}`), 0o600))
			}
			outcome, err := fx.Provider.Parse(t.Context(), ParseRequest{Source: fx.Source})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			msgs := outcome.Results[0].Result.Messages
			require.Len(t, msgs, 2)
			assert.Equal(t, "The user is asking who I am and what model I am.", msgs[1].ThinkingText)
			assert.Equal(t, "I'm Auto, an agent router designed by Cursor.", msgs[1].Content)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
				{Kind: "thinking", End: 48}, {Kind: "text", End: 45},
			}}, msgs[1].ContentLayout)
		})
	}
}
