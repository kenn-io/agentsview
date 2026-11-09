package db

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

func TestImageTextIndexRebuild(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() {
		if d != nil {
			_ = d.Close()
		}
	})
	insertSession(t, d, "image", "demo")
	content := testInlineImageContent()
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "image", Role: "assistant", Content: "answer", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, "invoice 中文搜索", false))
	t.Run("CJK reopen", func(t *testing.T) {
		if !simpleFTSRuntimeConfig.available() {
			t.Skip("simple FTS runtime is not installed")
		}
		require.NoError(t, d.Close())
		d, err = Open(ctx, path)
		require.NoError(t, err)
		page, err := d.Search(ctx, SearchFilter{Query: "中文搜索"})
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		_, err = d.getWriter().Exec(ctx, `DROP TABLE image_text_cjk_fts`)
		require.NoError(t, err)
		require.NoError(t, d.Close())
		d, err = Open(ctx, path)
		require.NoError(t, err)
		page, err = d.Search(ctx, SearchFilter{Query: "中文搜索"})
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
	})
	require.NoError(t, d.DropFTS(ctx))
	require.NoError(t, d.RebuildFTS(ctx))
	queries := []string{"invoice"}
	if simpleFTSRuntimeConfig.available() {
		queries = append(queries, "中文搜索")
	}
	for _, query := range queries {
		page, err := d.Search(ctx, SearchFilter{Query: query})
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		require.Equal(t, "image", page.Results[0].SessionID)
	}
}

func TestImageTextMembershipAndPruning(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	image := []byte("\x89PNG\r\n\x1a\nfixture")
	content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(image))
	images := RetainedToolResultImages(content)
	require.Len(t, images, 1)
	add := func(id string) {
		t.Helper()
		insertSession(t, d, id, "demo")
		require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: id, Ordinal: 1, Role: "assistant", Content: "answer", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	}
	add("one")
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, images[0].SHA256, identity, "invoice", false))
	add("two")
	insertSession(t, d, "other", "demo")
	other := `[{"type":"input_image","image_url":"data:image/png;base64,AQID"}]`
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "other", Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: other}}}}))
	otherHash := RetainedToolResultImages(other)[0].SHA256
	require.NoError(t, d.SaveImageText(ctx, otherHash, identity, "other text", false))
	require.NoError(t, d.ReplaceSessionMessages(ctx, "one", []Message{{SessionID: "one", Role: "assistant", Content: "removed"}}))
	var count int
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache WHERE image_hash=?`, images[0].SHA256).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, d.ReplaceSessionMessages(ctx, "two", []Message{{SessionID: "two", Role: "assistant", Content: "removed"}}))
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache WHERE image_hash=?`, images[0].SHA256).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache WHERE image_hash=?`, otherHash).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, d.SaveImageText(ctx, images[0].SHA256, identity, "late", false))
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache WHERE image_hash=?`, images[0].SHA256).Scan(&count))
	require.Zero(t, count)
}

func TestImageTextMigrationStrippingAndDeletion(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	insertSession(t, d, "image", "demo")
	insertMessages(t, d, testImageMessage("image"))
	images := RetainedToolResultImages(testInlineImageContent())
	require.Len(t, images, 1)
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, images[0].SHA256, identity, "invoice", false))
	_, err = d.MigrateToolImages(ctx, StripImagesFilter{}, realPut(t.TempDir()))
	require.NoError(t, err)
	page, err := d.Search(ctx, SearchFilter{Query: "invoice"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.NoError(t, d.SoftDeleteSession(ctx, "image"))
	var count int
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = d.StripToolImages(ctx, StripImagesFilter{})
	require.NoError(t, err)
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache`).Scan(&count))
	require.Zero(t, count)
}

func TestImageTextProcessorReplacement(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	insertSession(t, d, "image", "demo")
	insertMessages(t, d, testImageMessage("image"))
	images := RetainedToolResultImages(testInlineImageContent())
	require.Len(t, images, 1)
	old, err := ocr.NewIdentity("test", "model", "old")
	require.NoError(t, err)
	current, err := ocr.NewIdentity("test", "model", "new")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, images[0].SHA256, old, "old words", false))
	require.NoError(t, d.SaveImageText(ctx, images[0].SHA256, current, "new words", false))
	page, err := d.Search(ctx, SearchFilter{Query: "old"})
	require.NoError(t, err)
	require.Empty(t, page.Results)
	page, err = d.Search(ctx, SearchFilter{Query: "new"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	var count int
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestImageTextSearchQuotedAndColumnFiltered(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	insertSession(t, d, "image", "demo")
	insertMessages(t, d, testImageMessage("image"))
	image := RetainedToolResultImages(testInlineImageContent())[0]
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, image.SHA256, identity, "content:invoice", false))
	page, err := d.Search(ctx, SearchFilter{Query: `"content:invoice"`})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	page, err = d.Search(ctx, SearchFilter{Query: `"absent" OR content:invoice`})
	require.NoError(t, err)
	require.Empty(t, page.Results)
}

func TestImageTextSearchContent(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	insertSession(t, d, "session", "demo")
	content := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	messages := []Message{
		{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "main", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}},
		{SessionID: "session", Ordinal: 2, Role: "assistant", Content: "side", IsSidechain: true, ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}},
	}
	require.NoError(t, d.InsertMessages(ctx, messages))
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	text := "invoice 42\n-----BEGIN RSA PRIVATE KEY-----\nMIIBSECRETKEYMATERIAL0123456789ABCDEF\nMIIBSECRETKEYMATERIAL0123456789ABCDEF\nMIIBSECRETKEYMATERIAL0123456789ABCDEF\nMIIBSECRETKEYMATERIAL0123456789ABCDEF\nMIIBSECRETKEYMATERIAL0123456789ABCDEF\n-----END RSA PRIVATE KEY-----"
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, text, false))
	for _, mode := range []string{"substring", "regex"} {
		filter := ContentSearchFilter{Pattern: "invoice 42", Mode: mode, Sources: []string{"tool_result"}, Scope: "subordinate", IncludeOneShot: true, IncludeAutomated: true}
		page, err := d.SearchContent(ctx, filter)
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		require.Equal(t, 2, page.Matches[0].Ordinal)
		require.True(t, page.Matches[0].Sidechain)
		require.Contains(t, page.Matches[0].Snippet, "[redacted private key block]")
		require.NotContains(t, page.Matches[0].Snippet, "MIIBSECRETKEYMATERIAL")
		filter.Scope = ""
		filter.Project = "missing"
		page, err = d.SearchContent(ctx, filter)
		require.NoError(t, err)
		require.Empty(t, page.Matches)
		filter.Project = ""
		filter.Limit = 1
		page, err = d.SearchContent(ctx, filter)
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		require.NotZero(t, page.NextCursor)
		filter.Cursor = page.NextCursor
		page, err = d.SearchContent(ctx, filter)
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		require.Zero(t, page.NextCursor)
	}
}

func TestImageTextSearchUsesCurrentResultEvents(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	insertSession(t, d, "session", "demo")
	stale := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	current := `[{"type":"input_image","image_url":"data:image/png;base64,AwQF"}]`
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "answer", ToolCalls: []ToolCall{{ToolName: "Read", ToolUseID: "call", ResultContent: stale, ResultEvents: []ToolResultEvent{{ToolUseID: "call", Source: "tool", Status: "completed", Content: current}}}}}}))
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(stale)[0].SHA256, identity, "staleimageword", false))
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(current)[0].SHA256, identity, "currentimageword", false))
	page, err := d.Search(ctx, SearchFilter{Query: "staleimageword"})
	require.NoError(t, err)
	require.Empty(t, page.Results)
	page, err = d.Search(ctx, SearchFilter{Query: "currentimageword"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Equal(t, "session", page.Results[0].SessionID)
	require.Equal(t, 1, page.Results[0].Ordinal)
	require.Equal(t, "image_text", page.Results[0].MatchKind)
	require.Contains(t, page.Results[0].Snippet, "<mark>currentimageword</mark>")
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "session", Ordinal: 2, Role: "assistant", Content: "later"}}))
	page, err = d.Search(ctx, SearchFilter{Query: "currentimageword"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Equal(t, 1, page.Results[0].Ordinal)
}

func TestImageTextSearchRespectsMessageVisibility(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	content := testInlineImageContent()
	for _, tc := range []struct {
		id, role, body string
		system, event  bool
	}{
		{"hidden-call", "system", "hidden", true, false},
		{"prefix-call", "user", "<system-reminder>hidden</system-reminder>", false, false},
		{"visible-call", "assistant", "answer", false, false},
		{"visible-event", "assistant", "answer", false, true},
	} {
		insertSession(t, d, tc.id, "demo")
		call := ToolCall{ToolName: "Read", ToolUseID: "call", ResultContent: content}
		if tc.event {
			call.ResultEvents = []ToolResultEvent{{ToolUseID: "call", Source: "tool", Status: "completed", Content: content}}
		}
		require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: tc.id, Ordinal: 1, Role: tc.role, Content: tc.body, IsSystem: tc.system, ToolCalls: []ToolCall{call}}}))
	}
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, "invoice", false))
	page, err := d.Search(ctx, SearchFilter{Query: "invoice"})
	require.NoError(t, err)
	var ids []string
	for _, result := range page.Results {
		ids = append(ids, result.SessionID)
	}
	require.ElementsMatch(t, []string{"visible-call", "visible-event"}, ids)
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "hidden-call", Ordinal: 2, Role: "assistant", Content: "visible reply"}}))
	_, err = d.getWriter().Exec(ctx, `UPDATE sessions SET display_name='invoice' WHERE id='hidden-call'`)
	require.NoError(t, err)
	page, err = d.Search(ctx, SearchFilter{Query: "invoice"})
	require.NoError(t, err)
	ids = nil
	for _, result := range page.Results {
		ids = append(ids, result.SessionID)
	}
	require.ElementsMatch(t, []string{"visible-call", "visible-event", "hidden-call"}, ids)
}
