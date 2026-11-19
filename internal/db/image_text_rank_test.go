package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

func TestImageTextSearchPaginationByRelevance(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	require.NoError(t, d.BackfillImageTextOccurrences(ctx))
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

func TestImageTextCanOutrankMessageInSameSession(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	require.NoError(t, d.BackfillImageTextOccurrences(ctx))
	for i := range 50 {
		id := fmt.Sprintf("background-%02d", i)
		insertSession(t, d, id, "demo")
		require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: id, Role: "assistant", Content: "ordinary background"}}))
	}
	image := `[{"type":"input_image","image_url":"data:image/png;base64,AQID"}]`
	insertSession(t, d, "image-only", "control")
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "image-only", Ordinal: 1, Role: "assistant", Content: "screenshot", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: image}}}}))
	insertSession(t, d, "both", "demo")
	require.NoError(t, d.InsertMessages(ctx, []Message{
		{SessionID: "both", Ordinal: 0, Role: "assistant", Content: strings.Repeat("background ", 200) + "invoice"},
		{SessionID: "both", Ordinal: 1, Role: "assistant", Content: "screenshot", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: image}}},
	}))
	insertSession(t, d, "weaker", "demo")
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "weaker", Role: "assistant", Content: strings.Repeat("background ", 100) + "invoice"}}))
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(image)[0].SHA256, identity, "invoice invoice invoice", false))
	control, err := d.Search(ctx, SearchFilter{Query: "invoice", Project: "control", Limit: 1})
	require.NoError(t, err)
	require.Len(t, control.Results, 1)
	firstPage, err := d.Search(ctx, SearchFilter{Query: "invoice", Limit: 2})
	require.NoError(t, err)
	require.Len(t, firstPage.Results, 2)
	require.ElementsMatch(t, []string{"both", "image-only"}, []string{firstPage.Results[0].SessionID, firstPage.Results[1].SessionID})
	for _, project := range []string{"", "demo"} {
		t.Run("project="+project, func(t *testing.T) {
			page, err := d.Search(ctx, SearchFilter{Query: "invoice", Project: project, Limit: 10})
			require.NoError(t, err)
			var result SearchResult
			for _, candidate := range page.Results {
				if candidate.SessionID == "both" {
					result = candidate
				}
			}
			require.Equal(t, "both", result.SessionID)
			require.Equal(t, 1, result.Ordinal)
			require.InDelta(t, control.Results[0].Rank, result.Rank, 1e-12)
			require.Equal(t, 3, strings.Count(result.Snippet, "<mark>invoice</mark>"))
		})
	}
}
