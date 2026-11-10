package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

func TestImageTextRanksWithMessages(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	require.NoError(t, d.BackfillImageTextOccurrences(ctx))
	for i := range 200 {
		id := fmt.Sprintf("message-%03d", i)
		insertSession(t, d, id, "demo")
		var messages []Message
		for j := range 10 {
			body := strings.Repeat("ordinary background words ", 30)
			if j == 0 {
				body += " invoice"
			}
			messages = append(messages, Message{SessionID: id, Ordinal: j, Role: "assistant", Content: body})
		}
		require.NoError(t, d.InsertMessages(ctx, messages))
	}
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	for i, payload := range []string{"AQID", "BAUG", "BwgJ"} {
		id := fmt.Sprintf("image-%d", i)
		insertSession(t, d, id, "demo")
		content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, payload)
		require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: id, Role: "assistant", Content: "image", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
		text := "invoice"
		if i == 2 {
			text = "other"
		}
		require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, text, false))
	}
	page, err := d.Search(ctx, SearchFilter{Query: "invoice", Limit: 10})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(page.Results[0].SessionID, "image-"), "a short image match should outrank a long message")
	page, err = d.Search(ctx, SearchFilter{Query: `"invoice" AND rendered:invoice`, Limit: 10})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(page.Results[0].SessionID, "image-"), "rendered search should include image text")
	page, err = d.Search(ctx, SearchFilter{Query: `"invoice" AND content:invoice`, Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, page.Results)
	for _, r := range page.Results {
		require.False(t, strings.HasPrefix(r.SessionID, "image-"), "content search should not match image text")
	}
}

func TestImageTextSearchPaginationByRelevance(t *testing.T) {
	t.Run("first page with many messages", func(t *testing.T) {
		ctx := t.Context()
		d := testDB(t)
		require.NoError(t, d.BackfillImageTextOccurrences(ctx))
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
		_, err := d.getWriter().Exec(ctx, `UPDATE sessions SET started_at='2024-01-01T00:00:00Z', ended_at='2024-01-01T00:00:00Z'`)
		require.NoError(t, err)
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
		require.InDelta(t, messageRank, imageRank, 0.000001, "the matches should exercise equal relevance")
	})
	t.Run("cursor pages", func(t *testing.T) {
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
	})
}
