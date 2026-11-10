package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

func TestImageTextSearchFirstPageWithManyMessages(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	for i := range 50 {
		id := fmt.Sprintf("message-%02d", i)
		insertSession(t, d, id, "demo")
		require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: id, Role: "assistant", Content: "needle " + strings.Repeat("filler ", i+1)}}))
	}
	insertSession(t, d, "image", "demo")
	content := testInlineImageContent()
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "image", Role: "assistant", Content: "answer", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	insertSession(t, d, "both", "demo")
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "both", Role: "assistant", Content: "needle " + strings.Repeat("filler ", 100), ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, "needle needle needle", false))
	page, err := d.Search(ctx, SearchFilter{Query: "needle", Limit: 30})
	require.NoError(t, err)
	require.Len(t, page.Results, 30)
	var found bool
	for _, result := range page.Results {
		found = found || result.SessionID == "image"
	}
	require.True(t, found, "image text should reach the first page")
	seen := make(map[string]bool)
	var previousRank float64
	first := true
	for {
		for _, result := range page.Results {
			require.False(t, seen[result.SessionID], "session must appear only once across pages")
			if !first {
				require.GreaterOrEqual(t, result.Rank, previousRank)
			}
			previousRank, first = result.Rank, false
			seen[result.SessionID] = true
		}
		if page.NextCursor == 0 {
			break
		}
		page, err = d.Search(ctx, SearchFilter{Query: "needle", Limit: 30, Cursor: page.NextCursor})
		require.NoError(t, err)
	}
	require.Len(t, seen, 52)
}

func TestImageTextSearchPaginationByRelevance(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	content := testInlineImageContent()
	for _, id := range []string{"message-a", "message-b", "message-c", "message-d"} {
		insertSession(t, d, id, "demo")
		require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: id, Ordinal: 1, Role: "assistant", Content: "needle response"}}))
	}
	for _, id := range []string{"image-a", "image-b"} {
		insertSession(t, d, id, "demo")
		require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: id, Ordinal: 1, Role: "assistant", Content: "answer", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	}
	insertSession(t, d, "name", "demo")
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "name", Ordinal: 1, Role: "assistant", Content: "answer"}}))
	_, err := d.getWriter().Exec(ctx, `UPDATE sessions SET display_name='needle' WHERE id IN ('name','image-b')`)
	require.NoError(t, err)
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, "needle", false))
	var ids []string
	ordinals := map[string]int{}
	for cursor := 0; ; {
		page, err := d.Search(ctx, SearchFilter{Query: "needle", Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		for _, result := range page.Results {
			ids = append(ids, result.SessionID)
			ordinals[result.SessionID] = result.Ordinal
		}
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}
	require.Len(t, ids, 7)
	require.ElementsMatch(t, []string{"message-a", "message-b", "message-c", "message-d", "image-a", "image-b"}, ids[:6])
	require.Equal(t, "name", ids[6])
	require.Equal(t, 1, ordinals["image-b"])
	require.Equal(t, -1, ordinals["name"])
}
