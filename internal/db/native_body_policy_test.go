package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

func TestNativeArchivePolicyRemovesStandaloneOutput(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "native-policy", "project")
			d.SetArchiveContent(policy)
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{
				SessionID: "native-policy", Role: "assistant", Content: "answer", ThinkingText: "plan",
				ToolResultText: "private-output", ContentLength: 91, HasThinking: true,
				ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
					{Kind: "thinking", End: 4}, {Kind: "text", End: 6}, {Kind: "tool_result", End: 14},
				}},
			}}))
			got, err := d.GetAllMessages(t.Context(), "native-policy")
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Empty(t, got[0].ToolResultText)
			if policy == config.ArchiveContentTranscripts {
				assert.Equal(t, "answer", got[0].Content)
				assert.Equal(t, "plan", got[0].ThinkingText)
				assert.Equal(t, 91, got[0].ContentLength)
				assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
					{Kind: "thinking", End: 4}, {Kind: "text", End: 6},
				}}, got[0].ContentLayout)
			} else {
				assert.Empty(t, got[0].Content)
				assert.Empty(t, got[0].ThinkingText)
				assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{}}, got[0].ContentLayout)
			}
		})
	}
}

func TestNativeTranscriptPolicyKeepsLiteralToolRenderingDialogue(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "native-literal", "project")
	d.SetArchiveContent(config.ArchiveContentTranscripts)
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{
		SessionID: "native-literal", Role: "assistant", Content: "[Bash]\n$ literal-command", ContentLength: 91,
		HasToolUse:    true,
		ToolCalls:     []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"literal-command"}`, Rendering: "[Bash]\n$ literal-command"}},
		ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "text", End: 24}, {Kind: "tool_call", CallIndex: 0}}},
	}}))
	got, err := d.GetAllMessages(t.Context(), "native-literal")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "[Bash]\n$ literal-command", got[0].Content)
	assert.Equal(t, 91, got[0].ContentLength)
	assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "text", End: 24}, {Kind: "tool_call", CallIndex: 0}}}, got[0].ContentLayout)
	require.Len(t, got[0].ToolCalls, 1)
	assert.Equal(t, "[Bash]", got[0].ToolCalls[0].Rendering)
	assert.Empty(t, got[0].ToolCalls[0].InputJSON)
}

func TestNativeCopiedArchivePolicies(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		for _, path := range []string{"orphan", "trash"} {
			t.Run(string(policy)+"/"+path, func(t *testing.T) {
				source := testDB(t)
				insertSession(t, source, "native-copy", "project")
				require.NoError(t, source.InsertMessages(t.Context(), []Message{{
					SessionID: "native-copy", Role: "assistant", Content: "[Bash]\n$ literal-command",
					ThinkingText: "plan", ToolResultText: "private-output", ContentLength: 91, HasThinking: true, HasToolUse: true,
					ToolCalls: []ToolCall{
						{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"literal-command"}`, Rendering: "[Bash]\n$ literal-command"},
						{ToolName: "Task", Category: "Task", ToolUseID: "delegation", Rendering: "[Task]\nprivate-input", SubagentSessionID: "child"},
					},
					ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
						{Kind: "thinking", End: 4},
						{Kind: "text", End: 24},
						{Kind: "tool_call", CallIndex: 0},
						{Kind: "tool_result", End: 14},
						{Kind: "tool_call", CallIndex: 1},
					}},
				}}))
				destination := testDB(t)
				destination.SetArchiveContent(policy)
				copyData := func() ([]string, error) { return destination.CopyOrphanedDataFromExcluding(source.Path(), nil) }
				if path == "trash" {
					require.NoError(t, source.SoftDeleteSession(t.Context(), "native-copy"))
					copyData = func() ([]string, error) { return destination.CopyTrashedDataFrom(source.Path()) }
				}
				copied, err := copyData()
				require.NoError(t, err)
				assert.Equal(t, []string{"native-copy"}, copied)
				if path == "trash" {
					_, err = destination.RestoreSession(t.Context(), "native-copy")
					require.NoError(t, err)
				}
				got, err := destination.GetAllMessages(t.Context(), "native-copy")
				require.NoError(t, err)
				require.Len(t, got, 1)
				assert.Empty(t, got[0].ToolResultText)
				if policy == config.ArchiveContentTranscripts {
					assert.Equal(t, "[Bash]\n$ literal-command", got[0].Content)
					assert.Equal(t, "plan", got[0].ThinkingText)
					assert.Equal(t, 91, got[0].ContentLength)
					assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
						{Kind: "thinking", End: 4}, {Kind: "text", End: 24}, {Kind: "tool_call", CallIndex: 0}, {Kind: "tool_call", CallIndex: 1},
					}}, got[0].ContentLayout)
					require.Len(t, got[0].ToolCalls, 2)
					assert.Equal(t, "[Bash]", got[0].ToolCalls[0].Rendering)
					assert.Equal(t, "[Task]", got[0].ToolCalls[1].Rendering)
					assert.Empty(t, got[0].ToolCalls[0].InputJSON)
				} else {
					assert.Empty(t, got[0].Content)
					assert.Empty(t, got[0].ThinkingText)
					assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{}}, got[0].ContentLayout)
					require.Len(t, got[0].ToolCalls, 1)
					assert.Empty(t, got[0].ToolCalls[0].Rendering)
					assert.Equal(t, "delegation", got[0].ToolCalls[0].ToolUseID)
				}
			})
		}
	}
}

func TestLegacyCopiedTranscriptRedactsStoredRendering(t *testing.T) {
	for _, path := range []string{"orphan", "trash"} {
		t.Run(path, func(t *testing.T) {
			source := testDB(t)
			insertSession(t, source, "legacy-rendering", "project")
			message := Message{
				SessionID: "legacy-rendering", Role: "assistant", Content: "legacy prose\n[Bash]\n$ legacy-command", ContentLength: 100,
				ToolResultText: "legacy-output", HasToolUse: true,
				ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"legacy-command"}`, Rendering: "[Bash]\n$ legacy-command"}},
			}
			message.SetContentLayout(nil)
			require.NoError(t, source.InsertMessages(t.Context(), []Message{message}))
			destination := testDB(t)
			destination.SetArchiveContent(config.ArchiveContentTranscripts)
			copyData := func() ([]string, error) { return destination.CopyOrphanedDataFromExcluding(source.Path(), nil) }
			if path == "trash" {
				require.NoError(t, source.SoftDeleteSession(t.Context(), "legacy-rendering"))
				copyData = func() ([]string, error) { return destination.CopyTrashedDataFrom(source.Path()) }
			}
			copied, err := copyData()
			require.NoError(t, err)
			assert.Equal(t, []string{"legacy-rendering"}, copied)
			got, err := destination.GetAllMessages(t.Context(), "legacy-rendering")
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, "legacy prose\n[Bash]", got[0].Content)
			assert.Equal(t, 19, got[0].ContentLength)
			assert.Nil(t, got[0].ContentLayout)
			assert.Empty(t, got[0].ToolResultText)
			require.Len(t, got[0].ToolCalls, 1)
			assert.Equal(t, "[Bash]", got[0].ToolCalls[0].Rendering)
			assert.Empty(t, got[0].ToolCalls[0].InputJSON)
		})
	}
}
