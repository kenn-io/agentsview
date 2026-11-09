package db

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

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
	require.NoError(t, d.ReplaceSessionMessages(ctx, "one", []Message{{SessionID: "one", Role: "assistant", Content: "removed"}}))
	var count int
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, d.ReplaceSessionMessages(ctx, "two", []Message{{SessionID: "two", Role: "assistant", Content: "removed"}}))
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, d.SaveImageText(ctx, images[0].SHA256, identity, "late", false))
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache`).Scan(&count))
	require.Zero(t, count)
}

func TestImageTextCJKSearchAndRebuild(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS runtime is not installed")
	}
	ctx := t.Context()
	d := testDB(t)
	content := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	images := RetainedToolResultImages(content)
	require.Len(t, images, 1)
	insertSession(t, d, "cjk-image", "demo")
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "cjk-image", Ordinal: 2, Role: "assistant", Content: "answer", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, images[0].SHA256, identity, "请验证中文搜索功能", false))
	require.NoError(t, d.DropFTS(ctx))
	require.NoError(t, d.RebuildFTS(ctx))
	page, err := d.Search(ctx, SearchFilter{Query: "中文搜索"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Equal(t, "cjk-image", page.Results[0].SessionID)
	require.Equal(t, 2, page.Results[0].Ordinal)
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

func TestImageTextBackfillNumericCursorAndTargetedPruning(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	for _, id := range []string{"10", "2", "3"} {
		insertSession(t, d, id, "demo")
		insertMessages(t, d, testImageMessage(id))
	}
	_, err := d.getWriter().Exec(ctx, `DELETE FROM image_text_occurrences`)
	require.NoError(t, err)
	more, err := d.BackfillImageTextOccurrences(ctx, 1)
	require.NoError(t, err)
	require.True(t, more)
	for more {
		more, err = d.BackfillImageTextOccurrences(ctx, 1)
		require.NoError(t, err)
	}
	var count int
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_occurrences`).Scan(&count))
	require.Equal(t, 3, count)
	// An unrelated cached hash must not be removed by another session's refresh.
	_, err = d.getWriter().Exec(ctx, `INSERT INTO image_text_cache(image_hash,processor,provider,model,revision,text,succeeded) VALUES('unrelated','p','test','m','r','saved',1)`)
	require.NoError(t, err)
	require.NoError(t, d.ReplaceSessionMessages(ctx, "10", []Message{{SessionID: "10", Role: "assistant", Content: "no pictures"}}))
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache WHERE image_hash='unrelated'`).Scan(&count))
	require.Equal(t, 1, count)
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

func TestImageTextRefreshIgnoresPlainResults(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	insertSession(t, d, "plain", "demo")
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "plain", Role: "assistant", Content: "answer", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: "plain result"}}}}))
	var count int
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_occurrences`).Scan(&count))
	require.Zero(t, count)
}

func TestRetainedImageFormsContainImageMarker(t *testing.T) {
	for _, content := range []string{
		`[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`,
		`[{"type":"file","uri":"data:image/png;base64,AAEC"}]`,
		`[{"type":"agentsview_image","version":1,"image_ref":"asset://ae4b3280e56e2faf83f414a6e3dabe9d5fbe18976544c05fed121accb85b53fc.png","media_type":"image/png","byte_size":3}]`,
	} {
		require.NotEmpty(t, RetainedToolResultImages(content))
		require.Contains(t, strings.ToLower(content), "image")
	}
}

func TestImageTextSearchSidechainOccurrence(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	insertSession(t, d, "session", "demo")
	content := testInlineImageContent()
	messages := []Message{
		{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "main", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}},
		{SessionID: "session", Ordinal: 2, Role: "assistant", Content: "side", IsSidechain: true, ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}},
	}
	require.NoError(t, d.InsertMessages(ctx, messages))
	hash := RetainedToolResultImages(content)[0].SHA256
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, hash, identity, "invoice", false))
	for _, mode := range []string{"substring", "regex"} {
		page, err := d.SearchContent(ctx, ContentSearchFilter{Pattern: "invoice", Mode: mode, Sources: []string{"tool_result"}, Scope: "subordinate", IncludeOneShot: true, IncludeAutomated: true})
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		require.Equal(t, 2, page.Matches[0].Ordinal)
		require.True(t, page.Matches[0].Sidechain)
	}
}

func TestImageTextDuplicateInSameResult(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	insertSession(t, d, "session", "demo")
	content := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "answer", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	hash := RetainedToolResultImages(content)[0].SHA256
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, hash, identity, "invoice", false))
	for _, mode := range []string{"substring", "regex"} {
		page, err := d.SearchContent(ctx, ContentSearchFilter{Pattern: "invoice", Mode: mode, Sources: []string{"tool_result"}, IncludeOneShot: true, IncludeAutomated: true, Limit: 1})
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
		{"hidden-event", "system", "hidden", true, true},
		{"prefix-call", "user", "<system-reminder>hidden</system-reminder>", false, false},
		{"prefix-event", "user", "<system-reminder>hidden</system-reminder>", false, true},
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
