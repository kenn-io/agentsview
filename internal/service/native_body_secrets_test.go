package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
)

func TestNativeSearchContextMasksBodiesAndRemapsUnicode(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "native-context", "project", func(s *db.Session) {
		s.MessageCount, s.UserMessageCount = 2, 2
	})
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{
			SessionID: "native-context", Role: "assistant", Content: "key AKIA7QHWN2DKR4FYPLJM\n界",
			ThinkingText: "plan AKIA7QHWN2DKR4FYPLJM\n界", ToolResultText: "output AKIA7QHWN2DKR4FYPLJM\n界", ContentLength: 91,
			HasThinking: true, HasToolUse: true,
			ToolCalls: []db.ToolCall{{ToolName: "Bash", Category: "Bash", Rendering: "invoke AKIA7QHWN2DKR4FYPLJM 界"}},
			ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
				{Kind: "text", End: 24},
				{Kind: "thinking", End: 15},
				{Kind: "thinking", Start: 15, End: 25},
				{Kind: "tool_result", End: 27},
				{Kind: "tool_call", CallIndex: 0},
				{Kind: "text", Start: 25, End: 28},
				{Kind: "thinking", Start: 26, End: 29},
				{Kind: "tool_result", Start: 28, End: 31},
			}},
		},
		{SessionID: "native-context", Ordinal: 1, Role: "user", Content: "anchor-needle"},
	}))
	be := service.NewDirectBackend(d, nil)
	masked, err := be.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "anchor-needle", Mode: "substring", Context: 1,
	})
	require.NoError(t, err)
	require.Len(t, masked.Matches, 1)
	require.Len(t, masked.Matches[0].ContextBefore, 1)
	got := masked.Matches[0].ContextBefore[0]
	assert.Equal(t, "key AKIA…PLJM\n界", got.Content)
	assert.Equal(t, "plan AKIA…PLJM\n界", got.ThinkingText)
	assert.Equal(t, "output AKIA…PLJM\n界", got.ToolResultText)
	assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
		{Kind: "text", End: 15},
		{Kind: "thinking", End: 16},
		{Kind: "thinking", Start: 16, End: 16},
		{Kind: "tool_result", End: 18},
		{Kind: "tool_call", CallIndex: 0},
		{Kind: "text", Start: 16, End: 19},
		{Kind: "thinking", Start: 17, End: 20},
		{Kind: "tool_result", Start: 19, End: 22},
	}}, got.ContentLayout)
	require.Len(t, got.ToolCalls, 1)
	assert.Equal(t, "invoke AKIA…PLJM 界", got.ToolCalls[0].Rendering)
	assert.Equal(t, 91, got.ContentLength)

	revealed, err := be.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "anchor-needle", Mode: "substring", Context: 1, Reveal: true,
	})
	require.NoError(t, err)
	require.Len(t, revealed.Matches, 1)
	require.Len(t, revealed.Matches[0].ContextBefore, 1)
	full := revealed.Matches[0].ContextBefore[0]
	assert.Equal(t, "key AKIA7QHWN2DKR4FYPLJM\n界", full.Content)
	assert.Equal(t, "plan AKIA7QHWN2DKR4FYPLJM\n界", full.ThinkingText)
	assert.Equal(t, "output AKIA7QHWN2DKR4FYPLJM\n界", full.ToolResultText)
	assert.Equal(t, "invoke AKIA7QHWN2DKR4FYPLJM 界", full.ToolCalls[0].Rendering)
	assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
		{Kind: "text", End: 24},
		{Kind: "thinking", End: 15},
		{Kind: "thinking", Start: 15, End: 25},
		{Kind: "tool_result", End: 27},
		{Kind: "tool_call", CallIndex: 0},
		{Kind: "text", Start: 25, End: 28},
		{Kind: "thinking", Start: 26, End: 29},
		{Kind: "tool_result", Start: 28, End: 31},
	}}, full.ContentLayout)
}
