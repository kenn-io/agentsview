//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/storage"
)

func TestDuckNativeBodiesSurvivePushAndIncrementalChanges(t *testing.T) {
	local := newLocalDB(t)
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{
		ID: "native-mirror", Agent: "claude", Project: "project", Machine: "local", MessageCount: 3,
	}))
	layout := &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
		{Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", End: 6}, {Kind: "tool_result", End: 5},
	}}
	messages := []db.Message{
		{
			SessionID: "native-mirror", Ordinal: 0, Role: "assistant", Content: "answer", ThinkingText: "plan",
			ToolResultText: "prior", ContentLength: 77, HasThinking: true, HasToolUse: true,
			ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
				{Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", End: 6}, {Kind: "tool_result", End: 5},
			}}, SourceUUID: "native-answer", ToolCalls: []db.ToolCall{{ToolName: "read", Category: "file", Rendering: "old"}},
		},
		{SessionID: "native-mirror", Ordinal: 1, Role: "user", Content: "[Thinking] legacy", ContentLength: 17},
		{SessionID: "native-mirror", Ordinal: 2, Role: "assistant", ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{}}},
	}
	messages[1].SetContentLayout(nil)
	require.NoError(t, local.InsertMessages(t.Context(), messages))
	mirrorPath := filepath.Join(t.TempDir(), "mirror.db")
	_, err := Push(t.Context(), mirrorPath, local, "test", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), mirrorPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	stored, err := store.GetAllMessages(t.Context(), "native-mirror")
	require.NoError(t, err)
	require.Len(t, stored, 3)
	assert.Equal(t, "answer", stored[0].Content)
	assert.Equal(t, "plan", stored[0].ThinkingText)
	assert.Equal(t, "prior", stored[0].ToolResultText)
	assert.Equal(t, layout, stored[0].ContentLayout)
	assert.Equal(t, 77, stored[0].ContentLength)
	assert.Equal(t, "native-answer", stored[0].SourceUUID)
	require.Len(t, stored[0].ToolCalls, 1)
	assert.Equal(t, "old", stored[0].ToolCalls[0].Rendering)
	assert.Nil(t, stored[1].ContentLayout)
	assert.Equal(t, "[Thinking] legacy", stored[1].Content)
	assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{}}, stored[2].ContentLayout)

	for _, source := range []struct{ kind, text string }{
		{"thinking", "plan"}, {"tool_output", "prior"}, {"tool_rendering", "old"},
	} {
		callIndex := 0
		finding := db.SecretFinding{SessionID: "native-mirror", MessageOrdinal: 0, LocationKind: source.kind}
		if source.kind == "tool_rendering" {
			finding.CallIndex = &callIndex
		}
		text, ok, err := store.SecretFindingSource(t.Context(), finding)
		require.NoError(t, err)
		assert.True(t, ok, source.kind)
		assert.Equal(t, source.text, text, source.kind)
	}

	for _, change := range []string{"output", "layout", "rendering"} {
		t.Run(change, func(t *testing.T) {
			require.NoError(t, store.Close())
			switch change {
			case "output":
				messages[0].ToolResultText = "after"
			case "layout":
				messages[0].ContentLayout.Blocks[0], messages[0].ContentLayout.Blocks[2] = messages[0].ContentLayout.Blocks[2], messages[0].ContentLayout.Blocks[0]
			case "rendering":
				messages[0].ToolCalls[0].Rendering = "new"
			}
			wantLayout := layout
			if change != "output" {
				wantLayout = &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
					{Kind: "text", End: 6}, {Kind: "tool_call"}, {Kind: "thinking", End: 4}, {Kind: "tool_result", End: 5},
				}}
			}
			require.NoError(t, local.ReplaceSessionMessages(t.Context(), "native-mirror", messages))
			result, err := Push(t.Context(), mirrorPath, local, "test", storage.MirrorPushOptions{}, false, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, result.Diagnostics.PushedSessions.Total)
			assert.Empty(t, result.Diagnostics.RebuildReason)
			store, err = NewStore(t.Context(), mirrorPath)
			require.NoError(t, err)
			current, err := store.GetAllMessages(t.Context(), "native-mirror")
			require.NoError(t, err)
			require.Len(t, current, 3)
			assert.Equal(t, "after", current[0].ToolResultText)
			assert.Equal(t, wantLayout, current[0].ContentLayout)
			require.Len(t, current[0].ToolCalls, 1)
			wantRendering := "old"
			if change == "rendering" {
				wantRendering = "new"
			}
			assert.Equal(t, wantRendering, current[0].ToolCalls[0].Rendering)
			assert.Nil(t, current[1].ContentLayout)
		})
	}
}
