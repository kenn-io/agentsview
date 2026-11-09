package db

import (
	"encoding/base64"
	"fmt"
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
