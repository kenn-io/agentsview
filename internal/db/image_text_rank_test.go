package db

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

func TestImageTextSearchPaginationAfterMessageMatches(t *testing.T) {
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
	_, err := d.getWriter().Exec(ctx, `UPDATE sessions SET display_name='needle' WHERE id='name'`)
	require.NoError(t, err)
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, "needle", false))
	var ids []string
	for cursor := 0; ; {
		page, err := d.Search(ctx, SearchFilter{Query: "needle", Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		for _, result := range page.Results {
			ids = append(ids, result.SessionID)
		}
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}
	require.Len(t, ids, 7)
	require.ElementsMatch(t, []string{"message-a", "message-b", "message-c", "message-d"}, ids[:4])
	require.ElementsMatch(t, []string{"image-a", "image-b"}, ids[4:6])
	require.Equal(t, "name", ids[6])
}
