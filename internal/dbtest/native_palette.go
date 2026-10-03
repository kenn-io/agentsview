package dbtest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// SeedNativePalette preserves native fields and a legacy body in the same
// archived transcript, with a raw tool key that is never displayed.
func SeedNativePalette(t *testing.T, archive *db.DB) {
	t.Helper()
	SeedNativeSearch(t, archive)
	session, err := archive.GetSession(t.Context(), "native-search")
	require.NoError(t, err)
	require.NotNil(t, session)
	session.EndedAt = new("2026-01-01T00:00:00Z")
	require.NoError(t, archive.UpsertSession(t.Context(), *session))
	body := parser.ExtractMessageContent(t.Context(), gjson.Parse(`[
		{"type":"tool_use","id":"read","name":"Read","input":{"file_path":"/tmp/visibleneedle.txt","_i":"hiddenvalue"}}
	]`))
	require.Empty(t, body.Content)
	require.Len(t, body.ToolCalls, 1)
	require.Contains(t, body.ToolCalls[0].Rendering, "visibleneedle.txt")
	require.NotContains(t, body.ToolCalls[0].Rendering, "_i")
	require.NoError(t, archive.InsertMessages(t.Context(), []db.Message{{
		SessionID: "native-search", Ordinal: 8, Role: "assistant",
		ContentLayout: body.ContentLayout, HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName: "Read", ToolUseID: "read", Category: "read",
			InputJSON: body.ToolCalls[0].InputJSON, Rendering: body.ToolCalls[0].Rendering,
		}},
	}, {
		SessionID: "native-search", Ordinal: 9, Role: "assistant",
		ToolResultText: `[{"type":"agentsview_image","version":1,"text":"[Image: image/png, 3 bytes]","media_type":"image/png","byte_size":3,"sha256":"hiddenimagehash"}]`,
	}}))
	ended := "2026-01-02T00:00:00Z"
	name := "namesneedle"
	require.NoError(t, archive.UpsertSession(t.Context(), db.Session{
		ID: "palette-newer", Project: "project", Machine: "local", Agent: "claude",
		EndedAt: &ended, MessageCount: 1,
	}))
	require.NoError(t, archive.RenameSession(t.Context(), "palette-newer", &name))
	require.NoError(t, archive.InsertMessages(t.Context(), []db.Message{{
		SessionID: "palette-newer", Ordinal: 0, Role: "assistant",
		ThinkingText: "reasononlyneedle", HasThinking: true,
	}}))
}

// CheckNativePalette protects full-transcript reach without widening dialogue
// search or searching invisible serialized tool keys.
func CheckNativePalette(t *testing.T, store interface {
	Search(context.Context, db.SearchFilter) (db.SearchPage, error)
	SearchSession(context.Context, string, string) ([]int, error)
},
) {
	t.Helper()
	for _, tc := range []struct {
		query    string
		ordinals []int
	}{
		{"dialogueneedle", []int{1, 5}},
		{"reasonneedle", []int{1, 5}},
		{"commandneedle", []int{1}},
		{"resultneedle", []int{1}},
		{"unmatchedneedle", []int{3}},
		{"legacyneedle", []int{5}},
		{"reasononlyneedle", []int{7}},
		{"visibleneedle", []int{8}},
		{"Image", []int{9}},
		{"byte_size", nil},
		{"hiddenimagehash", nil},
		{"_i", nil},
		{"hiddenvalue", nil},
		{"systemreason", nil},
	} {
		t.Run("find/"+tc.query, func(t *testing.T) {
			ordinals, err := store.SearchSession(t.Context(), "native-search", tc.query)
			require.NoError(t, err)
			assert.Equal(t, tc.ordinals, ordinals)
		})
		if tc.query == "reasononlyneedle" {
			continue
		}
		t.Run("palette/"+tc.query, func(t *testing.T) {
			page, err := store.Search(t.Context(), db.SearchFilter{Query: tc.query})
			require.NoError(t, err)
			if len(tc.ordinals) == 0 {
				assert.Empty(t, page.Results)
				return
			}
			require.Len(t, page.Results, 1, "best matching row per session")
			assert.Equal(t, "native-search", page.Results[0].SessionID)
			assert.Contains(t, tc.ordinals, page.Results[0].Ordinal)
		})
	}
	page, err := store.Search(t.Context(), db.SearchFilter{Query: "dialogueneedle commandneedle resultneedle"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1, "tokenized terms span canonical fields within one message")
	assert.Equal(t, 1, page.Results[0].Ordinal)
	page, err = store.Search(t.Context(), db.SearchFilter{Query: "namesneedle"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, "palette-newer", page.Results[0].SessionID)
	assert.Equal(t, -1, page.Results[0].Ordinal)
	filter := db.SearchFilter{Query: "reasononlyneedle", Sort: "recency", Limit: 1}
	for i, id := range []string{"palette-newer", "native-search"} {
		page, err = store.Search(t.Context(), filter)
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		assert.Equal(t, id, page.Results[0].SessionID)
		if i == 0 {
			assert.Equal(t, 1, page.NextCursor)
		} else {
			assert.Zero(t, page.NextCursor)
		}
		filter.Cursor = page.NextCursor
	}
}
