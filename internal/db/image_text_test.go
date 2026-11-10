package db

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

func TestImageTextBackfillAndCopy(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	insertSession(t, d, "image", "demo")
	content := testInlineImageContent()
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "image", Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	_, err := d.getWriter().ExecContext(ctx, `CREATE TRIGGER fail_image_backfill BEFORE INSERT ON image_text_occurrences BEGIN SELECT RAISE(ABORT,'backfill interrupted'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, d.BackfillImageTextOccurrences(ctx), "backfill interrupted")
	_, err = d.getWriter().ExecContext(ctx, `DROP TRIGGER fail_image_backfill`)
	require.NoError(t, err)
	require.NoError(t, d.BackfillImageTextOccurrences(ctx))
	pending, err := d.PendingImageText(ctx, "test", time.Now(), 20)
	require.NoError(t, err)
	require.Equal(t, []string{RetainedToolResultImages(content)[0].SHA256}, pending)
	disabledSource, disabledDest := testDB(t), testDB(t)
	require.NoError(t, disabledDest.CopyImageTextWithOccurrencesFrom(ctx, disabledSource.Path()))
	insertSession(t, disabledDest, "new-image", "demo")
	require.NoError(t, disabledDest.InsertMessages(ctx, []Message{{SessionID: "new-image", Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	pending, err = disabledDest.PendingImageText(ctx, "processor", time.Now(), 20)
	require.NoError(t, err)
	require.Empty(t, pending)
	source := testDB(t)
	require.NoError(t, source.BackfillImageTextOccurrences(ctx))
	dest := testDB(t)
	require.NoError(t, dest.CopyImageTextWithOccurrencesFrom(ctx, source.Path()))
	insertSession(t, dest, "new-image", "demo")
	require.NoError(t, dest.InsertMessages(ctx, []Message{{SessionID: "new-image", Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	pending, err = dest.PendingImageText(ctx, "processor", time.Now(), 20)
	require.NoError(t, err)
	require.Equal(t, []string{RetainedToolResultImages(content)[0].SHA256}, pending)
}

func TestImageTextDisableClearsSearchAndCache(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	require.NoError(t, d.BackfillImageTextOccurrences(ctx))
	insertSession(t, d, "image", "demo")
	content := testInlineImageContent()
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "image", Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, "invoice", false))
	_, err = d.getWriter().ExecContext(ctx, `UPDATE sessions SET sync_marker='2000-01-01T00:00:00.000Z' WHERE id='image'`)
	require.NoError(t, err)
	require.NoError(t, d.ClearImageText(ctx))
	page, err := d.Search(ctx, SearchFilter{Query: "invoice"})
	require.NoError(t, err)
	require.Empty(t, page.Results)
	var count int
	var marker string
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT sync_marker FROM sessions WHERE id='image'`).Scan(&marker))
	require.Greater(t, marker, "2000-01-01T00:00:00.000Z")
	require.NoError(t, d.BackfillImageTextOccurrences(ctx))
	pending, err := d.PendingImageText(ctx, identity.Fingerprint, time.Now(), 20)
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

func TestImageTextIndexRebuild(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, d.BackfillImageTextOccurrences(ctx))
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
		for _, reopen := range []func() error{d.Reopen, func() error {
			if err := d.CloseWriter(); err != nil {
				return err
			}
			return d.ReopenWriter()
		}} {
			require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, "invoice", false))
			require.NoError(t, reopen())
			require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, "invoice 中文搜索", false))
			page, err = d.Search(ctx, SearchFilter{Query: "中文搜索"})
			require.NoError(t, err)
			require.Len(t, page.Results, 1)
		}
		_, err = d.getWriter().Exec(ctx, `DROP TABLE messages_cjk_fts`)
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

func TestImageTextPendingPrefersUnattempted(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	require.NoError(t, d.BackfillImageTextOccurrences(t.Context()))
	insertSession(t, d, "image", "demo")
	var hashes []string
	for i := range 3 {
		content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "\x89PNG\r\n\x1a\n%d", i)))
		hashes = append(hashes, RetainedToolResultImages(content)[0].SHA256)
		require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "image", Ordinal: i, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	}
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	pending, err := d.PendingImageText(ctx, identity.Fingerprint, time.Now(), 20)
	require.NoError(t, err)
	require.Equal(t, []string{hashes[2], hashes[1], hashes[0]}, pending)
	slices.Sort(hashes)
	for _, hash := range hashes[:2] {
		require.NoError(t, d.SaveImageText(ctx, hash, identity, "", true))
	}
	_, err = d.getWriter().Exec(ctx, `UPDATE image_text_cache SET retry_at=CASE image_hash WHEN ? THEN '2000-01-02T00:00:00Z' ELSE '2000-01-01T00:00:00Z' END`, hashes[0])
	require.NoError(t, err)
	pending, err = d.PendingImageText(ctx, identity.Fingerprint, time.Now(), 20)
	require.NoError(t, err)
	require.Equal(t, []string{hashes[2], hashes[1], hashes[0]}, pending)
}

func TestImageTextMirrorRows(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	require.NoError(t, d.BackfillImageTextOccurrences(t.Context()))
	insertSession(t, d, "image", "demo")
	content := testInlineImageContent()
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "image", Ordinal: 3, Role: "assistant", Content: "answer", ToolCalls: []ToolCall{
		{ToolName: "Read", ResultContent: content},
		{ToolName: "Read", ToolUseID: "call", ResultEvents: []ToolResultEvent{{ToolUseID: "call", Source: "tool", Status: "completed", Content: content}}},
	}}}))
	hash := RetainedToolResultImages(content)[0].SHA256
	var before struct{ marker, modified string }
	var revision int64
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT sync_marker,COALESCE(local_modified_at,''),transcript_revision FROM sessions WHERE id='image'`).Scan(&before.marker, &before.modified, &revision))
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, hash, identity, "", true))
	var marker, modified string
	var afterRevision int64
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT sync_marker FROM sessions WHERE id='image'`).Scan(&marker))
	require.Equal(t, before.marker, marker)
	rows, err := d.SessionImageTextRows(ctx, "image")
	require.NoError(t, err)
	require.Empty(t, rows)

	require.NoError(t, d.SaveImageText(ctx, hash, identity, "invoice 4827", false))
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT sync_marker,COALESCE(local_modified_at,''),transcript_revision FROM sessions WHERE id='image'`).Scan(&marker, &modified, &afterRevision))
	require.Greater(t, marker, before.marker)
	require.Equal(t, before.modified, modified)
	require.Equal(t, revision, afterRevision)
	row := ImageTextRow{SessionID: "image", MessageOrdinal: 3, ImageHash: hash, Processor: identity.Fingerprint, Provider: identity.Provider, Model: identity.Model, Revision: identity.Revision, Text: "invoice 4827"}
	event, call := row, row
	event.SourceKind, event.CallIndex = "event", 1
	call.SourceKind = "tool_call"
	rows, err = d.SessionImageTextRows(ctx, "image")
	require.NoError(t, err)
	require.Equal(t, []ImageTextRow{event, call}, rows)
	snapshot, err := d.LoadSessionMirrorSnapshot(ctx, "image")
	require.NoError(t, err)
	require.Equal(t, rows, snapshot.ImageText)
}

func TestImageTextMembershipAndPruning(t *testing.T) {
	for _, mode := range []string{"replace", "delete"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			d := testDB(t)
			require.NoError(t, d.BackfillImageTextOccurrences(t.Context()))
			shared := testInlineImageContent()
			exclusive := `[{"type":"input_image","image_url":"data:image/png;base64,AQID"}]`
			unrelated := `[{"type":"input_image","image_url":"data:image/png;base64,BAUG"}]`
			for _, id := range []string{"one", "two", "unrelated"} {
				insertSession(t, d, id, "demo")
				content := shared
				if id == "unrelated" {
					content = unrelated
				}
				require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: id, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
			}
			require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "one", Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: exclusive}}}}))
			identity, err := ocr.NewIdentity("test", "model", "revision")
			require.NoError(t, err)
			sharedHash, exclusiveHash, unrelatedHash := RetainedToolResultImages(shared)[0].SHA256, RetainedToolResultImages(exclusive)[0].SHA256, RetainedToolResultImages(unrelated)[0].SHA256
			for _, hash := range []string{sharedHash, exclusiveHash, unrelatedHash} {
				require.NoError(t, d.SaveImageText(ctx, hash, identity, "invoice", false))
			}
			for _, step := range []struct {
				id     string
				shared int
			}{{"one", 1}, {"two", 0}} {
				if mode == "replace" {
					messages := []Message{{SessionID: step.id, Role: "assistant", Content: "removed"}}
					if step.id == "one" {
						messages[0].ToolCalls = []ToolCall{{ToolName: "Read", ResultContent: "text only"}}
						messages = append(messages, Message{SessionID: step.id, Ordinal: 1, Role: "assistant", Content: "removed"})
					}
					require.NoError(t, d.ReplaceSessionMessages(ctx, step.id, messages))
					var count int
					require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_occurrences WHERE session_id=?`, step.id).Scan(&count))
					require.Zero(t, count)
				} else {
					require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
						_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, step.id)
						return err
					}))
					var count int
					require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache WHERE image_hash=?`, sharedHash).Scan(&count))
					require.Equal(t, step.shared, count)
				}
				require.NoError(t, d.BackfillImageTextOccurrences(ctx))
				for hash, want := range map[string]int{sharedHash: step.shared, exclusiveHash: 0, unrelatedHash: 1} {
					var count int
					require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache WHERE image_hash=?`, hash).Scan(&count))
					require.Equal(t, want, count)
				}
			}
			require.NoError(t, d.SaveImageText(ctx, sharedHash, identity, "late", false))
			var count int
			require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache WHERE image_hash=?`, sharedHash).Scan(&count))
			require.Zero(t, count)
		})
	}
	t.Run("copy", func(t *testing.T) {
		ctx := t.Context()
		src := testDB(t)
		require.NoError(t, src.BackfillImageTextOccurrences(t.Context()))
		dst := testDB(t)
		require.NoError(t, dst.BackfillImageTextOccurrences(t.Context()))
		first := testInlineImageContent()
		terminal := `[{"type":"input_image","image_url":"data:image/png;base64,AwQF"}]`
		orphan := `[{"type":"input_image","image_url":"data:image/png;base64,BgcI"}]`
		for _, item := range []struct{ id, content string }{{"success", first}, {"terminal", terminal}, {"orphan", orphan}} {
			insertSession(t, src, item.id, "demo")
			require.NoError(t, src.InsertMessages(ctx, []Message{{SessionID: item.id, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: item.content}}}}))
			if item.id != "orphan" {
				insertSession(t, dst, item.id, "demo")
				require.NoError(t, dst.InsertMessages(ctx, []Message{{SessionID: item.id, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: item.content}}}}))
			}
		}
		identity, err := ocr.NewIdentity("test", "model", "revision")
		require.NoError(t, err)
		for _, content := range []string{first, orphan} {
			require.NoError(t, src.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, "invoice", false))
		}
		require.NoError(t, src.SaveImageText(ctx, RetainedToolResultImages(terminal)[0].SHA256, identity, "", false))
		_, err = dst.CopyImageTextFrom(ctx, src.Path())
		require.NoError(t, err)
		page, err := dst.Search(ctx, SearchFilter{Query: "invoice"})
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		require.Equal(t, "success", page.Results[0].SessionID)
		pending, err := dst.PendingImageText(ctx, identity.Fingerprint, time.Now(), 20)
		require.NoError(t, err)
		require.Empty(t, pending)
		var count int
		require.NoError(t, dst.BackfillImageTextOccurrences(ctx))
		require.NoError(t, dst.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache`).Scan(&count))
		require.Equal(t, 2, count)
	})

	t.Run("shared writes", imageTextSharedAcrossSessionWrites)
}

func imageTextSharedAcrossSessionWrites(t *testing.T) {
	for _, mode := range []string{"atomic batch", "trashed copy"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			d := testDB(t)
			require.NoError(t, d.BackfillImageTextOccurrences(t.Context()))
			content := testInlineImageContent()
			insertSession(t, d, "a", "demo")
			require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "a", Role: "assistant", Content: "answer", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
			identity, err := ocr.NewIdentity("test", "model", "revision")
			require.NoError(t, err)
			hash := RetainedToolResultImages(content)[0].SHA256
			require.NoError(t, d.SaveImageText(ctx, hash, identity, "invoice", false))
			want := []string{"b"}
			switch mode {
			case "atomic batch":
				_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{
					{Session: Session{ID: "a", Project: "demo", Agent: "test"}, ReplaceMessages: true, Messages: []Message{{SessionID: "a", Role: "assistant", Content: "removed"}}},
					{Session: Session{ID: "b", Project: "demo", Agent: "test"}, ReplaceMessages: true, Messages: []Message{{SessionID: "b", Role: "assistant", Content: "answer", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}},
				})
				require.NoError(t, err)
			case "trashed copy":
				copyID, err := d.ReplaceSessionKeepingTrashedCopy(ctx, SessionBatchWrite{Session: Session{ID: "a", Project: "demo", Agent: "test"}, Messages: []Message{{SessionID: "a", Role: "assistant", Content: "removed"}}})
				require.NoError(t, err)
				_, err = d.RestoreSession(ctx, copyID)
				require.NoError(t, err)
				want = []string{copyID}
			}
			page, err := d.Search(ctx, SearchFilter{Query: "invoice"})
			require.NoError(t, err)
			var got []string
			for _, result := range page.Results {
				got = append(got, result.SessionID)
			}
			require.ElementsMatch(t, want, got)
			pending, err := d.PendingImageText(ctx, identity.Fingerprint, time.Now(), 20)
			require.NoError(t, err)
			require.Empty(t, pending)
		})
	}
}

func TestImageTextMigrationStrippingAndDeletion(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	require.NoError(t, d.BackfillImageTextOccurrences(t.Context()))
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
	require.NoError(t, d.BackfillImageTextOccurrences(t.Context()))
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
	require.NoError(t, d.BackfillImageTextOccurrences(t.Context()))
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
	for _, query := range []string{`"invoice" OR content:absent`, `"invoice" OR content:"absent phrase"`, `"invoice" OR content:(absent OR missing)`, `"invoice" NOT content:invoice`} {
		page, err = d.Search(ctx, SearchFilter{Query: query})
		require.NoError(t, err, query)
		require.Len(t, page.Results, 1, query)
		require.Equal(t, "image", page.Results[0].SessionID)
	}
	page, err = d.Search(ctx, SearchFilter{Query: `"invoice" AND rendered:invoice`})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Equal(t, "image", page.Results[0].SessionID)
	page, err = d.Search(ctx, SearchFilter{Query: `"invoice" AND content:invoice`})
	require.NoError(t, err)
	require.Empty(t, page.Results)
}

func TestImageTextSearchContent(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	require.NoError(t, d.BackfillImageTextOccurrences(t.Context()))
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
	results, err := d.Search(ctx, SearchFilter{Query: "invoice 42"})
	require.NoError(t, err)
	require.Len(t, results.Results, 1)
	pageContent := `[{"type":"input_image","image_url":"data:image/png;base64,AwQF"},{"type":"input_image","image_url":"data:image/png;base64,BgcI"}]`
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "session", Ordinal: 3, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: pageContent}}}}))
	images := RetainedToolResultImages(pageContent)
	require.NoError(t, d.SaveImageText(ctx, images[0].SHA256, identity, "needle zebra", false))
	require.NoError(t, d.SaveImageText(ctx, images[1].SHA256, identity, "needle apple", false))
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
		filter = ContentSearchFilter{Pattern: "needle", Mode: mode, Sources: []string{"tool_result"}, Limit: 1, IncludeOneShot: true, IncludeAutomated: true}
		page, err = d.SearchContent(ctx, filter)
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		require.Contains(t, page.Matches[0].Snippet, "apple")
		require.NotZero(t, page.NextCursor)
		filter.Cursor = page.NextCursor
		page, err = d.SearchContent(ctx, filter)
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		require.Contains(t, page.Matches[0].Snippet, "zebra")
		require.Zero(t, page.NextCursor)
	}
}

func TestImageTextSearchUsesCurrentResultEvents(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	require.NoError(t, d.BackfillImageTextOccurrences(t.Context()))
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
	require.Contains(t, page.Results[0].Snippet, "<mark>currentimageword</mark>")
}

func TestImageTextSearchRespectsMessageVisibility(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	require.NoError(t, d.BackfillImageTextOccurrences(t.Context()))
	content := testInlineImageContent()
	for _, tc := range []struct {
		id, role, body string
		system         bool
	}{
		{"hidden-call", "system", "hidden", true},
		{"prefix-call", "user", "<system-reminder>hidden</system-reminder>", false},
		{"visible-call", "assistant", "answer", false},
	} {
		insertSession(t, d, tc.id, "demo")
		call := ToolCall{ToolName: "Read", ToolUseID: "call", ResultContent: content}
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
	require.ElementsMatch(t, []string{"visible-call"}, ids)
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "hidden-call", Ordinal: 2, Role: "assistant", Content: "visible reply"}}))
	_, err = d.getWriter().Exec(ctx, `UPDATE sessions SET display_name='invoice' WHERE id='hidden-call'`)
	require.NoError(t, err)
	page, err = d.Search(ctx, SearchFilter{Query: "invoice"})
	require.NoError(t, err)
	ids = nil
	for _, result := range page.Results {
		ids = append(ids, result.SessionID)
	}
	require.ElementsMatch(t, []string{"visible-call", "hidden-call"}, ids)
}

func TestMirrorImageTextDataVersion(t *testing.T) {
	for _, tc := range []struct {
		version int
		hasText bool
		want    int
	}{{169, false, 169}, {169, true, ImageTextDataVersion}, {172, true, ImageTextDataVersion}, {174, true, 174}} {
		require.Equal(t, tc.want, MirrorDataVersion(tc.version, tc.hasText))
	}
}

func TestImageTextAppendKeepsUntouchedOccurrences(t *testing.T) {
	for _, mode := range []string{"insert", "replace messages", "replace content", "batch append", "batch replace", "incremental", "metadata messages", "metadata content", "metadata batch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			d := testDB(t)
			require.NoError(t, d.BackfillImageTextOccurrences(ctx))
			insertSession(t, d, "image", "demo")
			content := testInlineImageContent()
			first := Message{SessionID: "image", Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ToolUseID: "old", ResultContent: content}}}
			if strings.HasPrefix(mode, "metadata ") {
				first.ToolCalls[0].ResultEvents = []ToolResultEvent{{ToolUseID: "old", Source: "tool", Status: "completed", Content: content}}
			}
			require.NoError(t, d.InsertMessages(ctx, []Message{first}))
			if mode == "batch replace" {
				require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "image", Ordinal: 1, Role: "assistant", Content: "new text"}}))
			}
			hash := RetainedToolResultImages(content)[0].SHA256
			identity, err := ocr.NewIdentity("test", "model", "revision")
			require.NoError(t, err)
			require.NoError(t, d.SaveImageText(ctx, hash, identity, "invoice", false))
			if strings.HasPrefix(mode, "metadata ") {
				insertSession(t, d, "other", "demo")
				require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "other", Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ToolUseID: "other", ResultContent: "other", ResultEvents: []ToolResultEvent{{ToolUseID: "other", Content: "other"}}}}}}))
				var before, after int64
				require.NoError(t, d.Reader().QueryRow(ctx, `SELECT transcript_revision FROM sessions WHERE id='image'`).Scan(&before))
				first.SourceParentUUID = "parent"
				session := Session{ID: "image", Project: "demo", Agent: defaultAgent, Machine: defaultMachine, MessageCount: 1}
				switch mode {
				case "metadata messages":
					err = d.ReplaceSessionMessages(ctx, "image", []Message{first})
				case "metadata content":
					err = d.ReplaceSessionContent(ctx, "image", []Message{first}, SessionSignalUpdate{}, nil)
				case "metadata batch":
					_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{Session: session, ReplaceMessages: true, Messages: []Message{first}}})
				}
				require.NoError(t, err)
				require.NoError(t, d.Reader().QueryRow(ctx, `SELECT transcript_revision FROM sessions WHERE id='image'`).Scan(&after))
				require.Equal(t, before, after)
				if mode == "metadata batch" {
					_, err = d.getWriter().ExecContext(ctx, `CREATE TABLE image_refresh_count(n INTEGER); INSERT INTO image_refresh_count VALUES (0); CREATE TRIGGER image_refresh_count_insert AFTER INSERT ON image_text_occurrences BEGIN UPDATE image_refresh_count SET n=n+1; END`)
					require.NoError(t, err)
					first.SourceUUID = "source-1"
					_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{Session: session, ReplaceMessages: true, Messages: []Message{first}}})
					require.NoError(t, err)
					var count int
					require.NoError(t, d.Reader().QueryRow(ctx, `SELECT n FROM image_refresh_count`).Scan(&count))
					require.Equal(t, 1, count)
				}
				page, err := d.SearchContent(ctx, ContentSearchFilter{Pattern: "invoice", Mode: "substring", Sources: []string{"tool_result"}, IncludeOneShot: true, IncludeAutomated: true})
				require.NoError(t, err)
				require.Len(t, page.Matches, 1)
				rows, err := d.SessionImageTextRows(ctx, "image")
				require.NoError(t, err)
				require.Len(t, rows, 1)
				require.Equal(t, "invoice", rows[0].Text)
				return
			}
			_, err = d.getWriter().Exec(ctx, `CREATE TRIGGER preserve_old BEFORE DELETE ON image_text_occurrences WHEN old.image_hash='`+hash+`' BEGIN SELECT RAISE(ABORT, 'unrelated image refreshed'); END`)
			require.NoError(t, err)
			appendMessage := Message{SessionID: "image", Ordinal: 1, Role: "assistant", Content: "new text"}
			session := Session{ID: "image", Project: "demo", Agent: defaultAgent, Machine: defaultMachine}
			switch mode {
			case "insert":
				err = d.InsertMessages(ctx, []Message{appendMessage})
			case "incremental":
				_, err = d.WriteSessionIncremental(ctx, "image", []Message{appendMessage}, IncrementalSessionUpdate{})
			case "replace messages":
				err = d.ReplaceSessionMessages(ctx, "image", []Message{first, appendMessage})
			case "replace content":
				err = d.ReplaceSessionContent(ctx, "image", []Message{first, appendMessage}, SessionSignalUpdate{}, nil)
			case "batch append":
				_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{Session: session, Messages: []Message{appendMessage}}})
			case "batch replace":
				appendMessage.SourceUUID = "source-1"
				_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{Session: session, ReplaceMessages: true, Messages: []Message{first, appendMessage}}})
			}
			require.NoError(t, err)
			page, err := d.Search(ctx, SearchFilter{Query: "invoice"})
			require.NoError(t, err)
			require.Len(t, page.Results, 1)
			require.Equal(t, 0, page.Results[0].Ordinal)
			if mode == "incremental" {
				second := `[{"type":"input_image","image_url":"data:image/png;base64,AQID"}]`
				_, err = d.WriteSessionIncremental(ctx, "image", []Message{{SessionID: "image", Ordinal: 2, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ToolUseID: "new", ResultContent: second}}}}, IncrementalSessionUpdate{})
				require.NoError(t, err)
				pending, err := d.PendingImageText(ctx, "processor", time.Now(), 10)
				require.NoError(t, err)
				require.ElementsMatch(t, []string{hash, RetainedToolResultImages(second)[0].SHA256}, pending)
				updated := `[{"type":"input_image","image_url":"data:image/png;base64,BAUG"}]`
				_, err = d.WriteSessionIncremental(ctx, "image", nil, IncrementalSessionUpdate{ToolCallResultUpdates: []ToolCallResultUpdate{{ToolUseID: "new", Position: ToolCallPosition{MessageOrdinal: 2}, Events: []ToolResultEvent{{Source: "tool", Content: updated}}}}})
				require.NoError(t, err)
				pending, err = d.PendingImageText(ctx, "processor", time.Now(), 10)
				require.NoError(t, err)
				require.ElementsMatch(t, []string{hash, RetainedToolResultImages(updated)[0].SHA256}, pending)
			}
		})
	}
}

func TestImageTextBatchFillsStoredResult(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	require.NoError(t, d.BackfillImageTextOccurrences(ctx))
	insertSession(t, d, "image", "demo")
	msg := Message{SessionID: "image", Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ToolUseID: "call"}}}
	require.NoError(t, d.InsertMessages(ctx, []Message{msg}))
	msg.ToolCalls[0].ResultContent = testInlineImageContent()
	_, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{Session: Session{ID: "image", Project: "demo", Agent: defaultAgent, Machine: defaultMachine}, Messages: []Message{msg}, CompleteStoredRows: true}})
	require.NoError(t, err)
	pending, err := d.PendingImageText(ctx, "processor", time.Now(), 10)
	require.NoError(t, err)
	require.Equal(t, []string{RetainedToolResultImages(msg.ToolCalls[0].ResultContent)[0].SHA256}, pending)
}
