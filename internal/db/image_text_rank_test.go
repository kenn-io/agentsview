package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

func TestImageTextSearchFirstPageWithManyMessages(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	content := testInlineImageContent()
	for i := range 50 {
		id := fmt.Sprintf("message-%02d", i)
		insertSession(t, d, id, "demo")
		message := Message{SessionID: id, Role: "assistant", Content: "needle"}
		if i == 49 {
			message.ToolCalls = []ToolCall{{ToolName: "Read", ResultContent: content}}
		}
		require.NoError(t, d.InsertMessages(ctx, []Message{message}))
	}
	insertSession(t, d, "zz-image", "demo")
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "zz-image", Role: "assistant", Content: "answer", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, "needle", false))
	page, err := d.Search(ctx, SearchFilter{Query: "needle", Limit: 30})
	require.NoError(t, err)
	require.Len(t, page.Results, 30)
	var found bool
	var imageRank, messageRank float64
	for _, result := range page.Results {
		if result.SessionID == "zz-image" {
			found = true
			imageRank = result.Rank
		} else {
			messageRank = result.Rank
		}
	}
	require.True(t, found, "image text should reach the first page")
	require.Equal(t, messageRank, imageRank, "the matches should exercise equal relevance")
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
	require.Len(t, seen, 51)
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
