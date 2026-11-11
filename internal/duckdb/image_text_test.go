//go:build !(windows && arm64)

package duckdb

import (
	"encoding/base64"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestDuckDBImageTextFollowsIncrementalPushes(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	const id, ts = "screenshot", "2026-01-02T03:04:05.000Z"
	content := `[{"type":"input_image","image_url":"data:image/png;base64,` +
		base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage")) + `"}]`
	writeTranscript := func(result string) {
		t.Helper()
		require.NoError(t, local.UpsertSession(ctx, syncSession(id, "shots", "look", ts, 2)))
		require.NoError(t, local.ReplaceSessionMessages(ctx, id, []db.Message{
			syncMessage(id, 0, "user", "look at this", ts),
			syncMessage(id, 1, "assistant", "reading", ts, db.ToolCall{
				ToolName: "Read", Category: "Read", ToolUseID: "shot", ResultContent: result,
			}, db.ToolCall{
				ToolName: "Bash", Category: "Bash", ToolUseID: "event",
				ResultEvents: []db.ToolResultEvent{{ToolUseID: "event", Source: "tool", Status: "completed", Content: result}},
			}),
		}))
	}
	writeTranscript(content)
	hash := db.RetainedToolResultImages(content)[0].SHA256

	path := filepath.Join(t.TempDir(), "image-text.duckdb")
	push := func() storage.MirrorPushResult {
		t.Helper()
		result, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
		return result
	}
	push()

	mirrorRows := func() []db.ImageTextRow {
		t.Helper()
		conn, err := Open(ctx, path)
		require.NoError(t, err)
		defer conn.Close()
		rows, err := conn.QueryContext(ctx, `SELECT session_id, source_kind, message_ordinal, call_index, event_index, image_index, image_hash, processor, provider, model, revision, text FROM image_text ORDER BY ALL`)
		require.NoError(t, err)
		defer rows.Close()
		var out []db.ImageTextRow
		for rows.Next() {
			var r db.ImageTextRow
			require.NoError(t, rows.Scan(&r.SessionID, &r.SourceKind, &r.MessageOrdinal, &r.CallIndex, &r.EventIndex, &r.ImageIndex, &r.ImageHash, &r.Processor, &r.Provider, &r.Model, &r.Revision, &r.Text))
			out = append(out, r)
		}
		require.NoError(t, rows.Err())
		return out
	}
	// found reports the anchor ordinals global search and tool_result
	// substring and regex search return for query, one result per call or
	// event in content search.
	found := func(query string) []int {
		t.Helper()
		store := openPushedStore(t, path)
		defer func() { require.NoError(t, store.Close()) }()
		var ordinals []int
		page, err := store.Search(ctx, db.SearchFilter{Query: query})
		require.NoError(t, err)
		for _, r := range page.Results {
			require.Equal(t, id, r.SessionID)
			assert.Contains(t, r.Snippet, query)
			ordinals = append(ordinals, r.Ordinal)
		}
		for _, mode := range []string{"substring", "regex"} {
			got, err := store.SearchContent(ctx, db.ContentSearchFilter{
				Pattern: query, Mode: mode, Sources: []string{"tool_result"},
				IncludeOneShot: true, IncludeAutomated: true,
			})
			require.NoError(t, err)
			for _, m := range got.Matches {
				require.Equal(t, id, m.SessionID)
				assert.Contains(t, m.Snippet, query)
				ordinals = append(ordinals, m.Ordinal)
			}
		}
		return ordinals
	}
	require.Empty(t, mirrorRows())
	require.Empty(t, found("invoice"))

	first := ocr.Identity{Fingerprint: "proc-a", Provider: "glm-ocr", Model: "glm-ocr", Revision: "r1"}
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	require.NoError(t, local.SaveImageText(ctx, hash, first, "invoice 4827", false))
	result := push()
	require.False(t, result.Diagnostics.Full, result.Diagnostics.RebuildReason)
	require.Equal(t, 1, result.SessionsPushed)
	want := func() []db.ImageTextRow {
		t.Helper()
		rows, err := local.SessionImageTextRows(ctx, id)
		require.NoError(t, err)
		return rows
	}
	assert.Equal(t, want(), mirrorRows())
	assert.Equal(t, []int{1, 1, 1, 1, 1}, found("invoice 4827"))
	conn, err := Open(ctx, path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO image_text
		SELECT session_id, source_kind, message_ordinal, call_index, event_index, image_index,
			image_hash, 'zzzz', provider, model, revision, 'invoice 4827 losing'
		FROM image_text`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store := openPushedStore(t, path)
	page, err := store.Search(ctx, db.SearchFilter{Query: "invoice 4827", Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, "invoice 4827", page.Results[0].Snippet)
	require.NoError(t, store.Close())
	conn, err = Open(ctx, path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `DELETE FROM image_text WHERE processor = 'zzzz'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.NoError(t, local.ClearImageText(ctx))
	result = push()
	require.False(t, result.Diagnostics.Full)
	assert.Empty(t, mirrorRows())
	assert.Empty(t, found("invoice"))
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	require.NoError(t, local.SaveImageText(ctx, hash, first, "invoice 4827", false))
	push()
	assert.Equal(t, want(), mirrorRows())

	result = push()
	assert.Zero(t, result.SessionsPushed, "an unchanged push must skip the session")
	assert.Equal(t, want(), mirrorRows())

	second := ocr.Identity{Fingerprint: "proc-b", Provider: "tesseract", Model: "eng", Revision: "r2"}
	require.NoError(t, local.SaveImageText(ctx, hash, second, "receipt 9931", false))
	push()

	assert.Equal(t, want(), mirrorRows())
	assert.Empty(t, found("invoice"))
	assert.Equal(t, []int{1, 1, 1, 1, 1}, found("receipt 9931"))

	// A mirror from before image text rebuilds with the rows.
	setMirrorMetadataValue(t, path, schemaVersionMetadataKey, strconv.Itoa(SchemaVersion-1))
	result = push()
	require.True(t, result.Diagnostics.Full)
	assert.Equal(t, want(), mirrorRows())

	writeTranscript("no image")
	push()
	assert.Empty(t, mirrorRows())
	assert.Empty(t, found("receipt"))
}

func TestDuckDBImageTextContentPageOrder(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	const id, ts = "image-pages", "2026-01-02T03:04:05.000Z"
	content := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"input_image","image_url":"data:image/png;base64,AwQF"}]`
	require.NoError(t, local.UpsertSession(ctx, syncSession(id, "shots", "look", ts, 2)))
	require.NoError(t, local.ReplaceSessionMessages(ctx, id, []db.Message{
		syncMessage(id, 0, "user", "look", ts),
		syncMessage(id, 1, "assistant", "answer", ts, db.ToolCall{ToolName: "Read", ResultContent: content}),
	}))
	images := db.RetainedToolResultImages(content)
	require.Len(t, images, 2)
	identity := ocr.Identity{Fingerprint: "proc-a", Provider: "glm-ocr", Model: "glm-ocr", Revision: "r1"}
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	require.NoError(t, local.SaveImageText(ctx, images[0].SHA256, identity, "needle zebra", false))
	require.NoError(t, local.SaveImageText(ctx, images[1].SHA256, identity, "needle apple "+strings.Repeat("界", 250)+" invoice example", false))
	path := filepath.Join(t.TempDir(), "image.duckdb")
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store := openPushedStore(t, path)
	defer func() { require.NoError(t, store.Close()) }()
	filter := db.ContentSearchFilter{Pattern: "needle", Mode: "substring", Sources: []string{"tool_result"}, Limit: 1, IncludeOneShot: true, IncludeAutomated: true}
	page, err := store.SearchContent(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	require.Contains(t, page.Matches[0].Snippet, "apple")
	require.NotZero(t, page.NextCursor)
	filter.Cursor = page.NextCursor
	page, err = store.SearchContent(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	require.Contains(t, page.Matches[0].Snippet, "zebra")
	require.Zero(t, page.NextCursor)
	search, err := store.Search(ctx, db.SearchFilter{Query: "invoice"})
	require.NoError(t, err)
	require.Len(t, search.Results, 1)
	require.Equal(t, id, search.Results[0].SessionID)
	require.Equal(t, 1, search.Results[0].Ordinal)
	require.Contains(t, search.Results[0].Snippet, "invoice")
}
