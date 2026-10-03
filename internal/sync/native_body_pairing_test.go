package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestNativeUnmatchedNamedResultObeysBlockedCategory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "trajectories", "trajectory-standalone_result-policy.ndjson")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"tool_call.result","tool_call_result":{"id":"missing-call","tool_name":"shell","observation":"blocked-output"}}`+"\n"), 0o600))
	provider, ok := parser.NewProvider(parser.AgentPoolside, parser.ProviderConfig{Roots: []string{root}, Machine: "local"})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), parser.FindSourceRequest{RawSessionID: "standalone_result-policy"})
	require.NoError(t, err)
	require.True(t, found)
	outcome, err := provider.Parse(t.Context(), parser.ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	parsed := outcome.Results[0].Result
	require.Len(t, parsed.Messages, 1)
	require.Equal(t, 1, parsed.Messages[0].Ordinal)
	for _, blocked := range []map[string]bool{nil, {"Bash": true}} {
		converted := toDBMessages(pendingWrite{sess: parsed.Session, msgs: parsed.Messages}, blocked)
		require.Len(t, converted, 1)
		archive := openTestDB(t)
		require.NoError(t, archive.UpsertSession(t.Context(), db.Session{ID: converted[0].SessionID, Agent: "poolside", Project: "project"}))
		require.NoError(t, archive.InsertMessages(t.Context(), converted))
		stored, err := archive.GetAllMessages(t.Context(), converted[0].SessionID)
		require.NoError(t, err)
		require.Len(t, stored, 1)
		assert.Equal(t, 1, stored[0].Ordinal)
		if blocked == nil {
			assert.Equal(t, "blocked-output", stored[0].ToolResultText)
		} else {
			assert.Empty(t, stored[0].ToolResultText)
			require.NotNil(t, stored[0].ContentLayout)
			require.Len(t, stored[0].ContentLayout.Blocks, 1)
			assert.Equal(t, 0, stored[0].ContentLayout.Blocks[0].End)
		}
	}
}

func TestNativeUnmatchedResultKeepsBodyAndIdentity(t *testing.T) {
	for _, callID := range []string{"", "missing-call"} {
		t.Run("id="+callID, func(t *testing.T) {
			messages := []db.Message{{
				SessionID: "native-result", Ordinal: 7, Role: "user", SourceUUID: "native-result-id",
				ToolResultText: "result-needle", ContentLength: 91,
				ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "tool_result", End: 13}}},
				ToolResults:   []db.ToolResult{{ToolUseID: callID, ContentLength: 13, ContentRaw: `"result-needle"`}},
			}}
			got := pairAndFilter(messages, nil)
			require.Len(t, got, 1)
			assert.Empty(t, got[0].Content)
			assert.Equal(t, "result-needle", got[0].ToolResultText)
			assert.Equal(t, "native-result-id", got[0].SourceUUID)
			assert.Equal(t, 7, got[0].Ordinal)
			assert.Equal(t, 91, got[0].ContentLength)
			assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "tool_result", End: 13}}}, got[0].ContentLayout)
		})
	}
}

func TestNativeEmptyUnmatchedResultKeepsIdentity(t *testing.T) {
	got := pairAndFilter([]db.Message{{
		SessionID: "empty-result", Ordinal: 7, Role: "user", SourceUUID: "empty-result-id",
		ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "tool_result"}}},
		ToolResults:   []db.ToolResult{{ContentRaw: `""`}},
	}}, nil)
	require.Len(t, got, 1)
	assert.Equal(t, "empty-result-id", got[0].SourceUUID)
	assert.Equal(t, 7, got[0].Ordinal)
	assert.Empty(t, got[0].ToolResultText)
	assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "tool_result"}}}, got[0].ContentLayout)
}

func TestNativePairingMovesOnlyMatchedOutput(t *testing.T) {
	messages := []db.Message{
		{SessionID: "native-result", Role: "assistant", ToolCalls: []db.ToolCall{{ToolUseID: "call", ToolName: "Read", Category: "Read"}}},
		{
			SessionID: "native-result", Ordinal: 1, Role: "user", SourceUUID: "mixed-result-id",
			Content: "next", ToolResultText: "paired\nunmatched\nanonymous", ContentLength: 91,
			ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
				{Kind: "tool_result", End: 6},
				{Kind: "text", End: 4},
				{Kind: "tool_result", Start: 7, End: 16},
				{Kind: "tool_result", Start: 17, End: 26},
			}},
			ToolResults: []db.ToolResult{
				{ToolUseID: "call", ContentRaw: `"paired"`, ContentLength: 6},
				{ToolUseID: "missing", ContentRaw: `"unmatched"`, ContentLength: 9},
				{ContentRaw: `"anonymous"`, ContentLength: 9},
			},
		},
	}
	got := pairAndFilter(messages, nil)
	require.Len(t, got, 2)
	require.Len(t, got[0].ToolCalls, 1)
	assert.Equal(t, "paired", got[0].ToolCalls[0].ResultContent)
	assert.Equal(t, 6, got[0].ToolCalls[0].ResultContentLength)
	assert.Equal(t, "next", got[1].Content)
	assert.Equal(t, "unmatched\nanonymous", got[1].ToolResultText)
	assert.Equal(t, "mixed-result-id", got[1].SourceUUID)
	assert.Equal(t, 91, got[1].ContentLength)
	assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
		{Kind: "text", End: 4}, {Kind: "tool_result", End: 9}, {Kind: "tool_result", Start: 10, End: 19},
	}}, got[1].ContentLayout)
	second := pairAndFilter(got, nil)
	require.Len(t, second, 2)
	assert.Equal(t, "unmatched\nanonymous", second[1].ToolResultText)
}

func TestNativeBlockedResultCannotSurviveInStandaloneOwner(t *testing.T) {
	messages := []db.Message{
		{Role: "assistant", ToolCalls: []db.ToolCall{{ToolUseID: "call", ToolName: "Read", Category: "Read"}}},
		{
			Role: "user", Content: "next", ToolResultText: "private-output", ContentLength: 91,
			ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "text", End: 4}, {Kind: "tool_result", End: 14}}},
			ToolResults:   []db.ToolResult{{ToolUseID: "call", ContentRaw: `"private-output"`, ContentLength: 14}},
		},
	}
	got := pairAndFilter(messages, map[string]bool{"Read": true})
	require.Len(t, got, 2)
	assert.Empty(t, got[0].ToolCalls[0].ResultContent)
	assert.Equal(t, 14, got[0].ToolCalls[0].ResultContentLength)
	assert.Equal(t, "next", got[1].Content)
	assert.Empty(t, got[1].ToolResultText)
	assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "text", End: 4}}}, got[1].ContentLayout)
}
