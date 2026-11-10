//go:build pgtest

package postgres

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"

	"go.kenn.io/agentsview/internal/db"
)

// TestPGImageTextPushAndSearch pushes a screenshot session before OCR, then
// publishes its image-only text through ordinary incremental pushes.
func TestPGImageTextPushAndSearch(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(t, "agentsview_image_text_test")
	image := `[{"type":"input_image","image_url":"data:image/png;base64,` +
		base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage")) + `"}]`
	messages := func(result string) []db.Message {
		return []db.Message{
			{SessionID: "shot", Ordinal: 0, Role: "user", Content: "look at this", ContentLength: 12},
			{SessionID: "shot", Ordinal: 1, Role: "assistant", Content: "reading it", ContentLength: 10,
				ToolCalls: []db.ToolCall{{ToolName: "Read", ToolUseID: "tu-shot", ResultContent: result}}},
		}
	}
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: "shot", Project: "proj", Machine: "workstation", Agent: "claude",
		MessageCount: 2, UserMessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}))
	require.NoError(t, local.InsertMessages(ctx, messages(image)))
	hash := db.RetainedToolResultImages(image)[0].SHA256
	push := func() {
		t.Helper()
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
	}
	push()
	store, err := NewStore(ctx, testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	type hit struct {
		session string
		ordinal int
	}
	searches := func() map[string][]hit {
		t.Helper()
		out := map[string][]hit{}
		page, err := store.Search(ctx, db.SearchFilter{Query: "invoice 4827", Limit: 10})
		require.NoError(t, err)
		for _, r := range page.Results {
			out["global"] = append(out["global"], hit{r.SessionID, r.Ordinal})
			assert.Contains(t, r.Snippet, "invoice 4827")
			assert.Equal(t, 1.0, r.Rank, "image matches keep PostgreSQL's constant rank")
		}
		for mode, pattern := range map[string]string{"substring": "invoice 4827", "regex": `invoice\s+48\d\d`} {
			page, err := store.SearchContent(ctx, db.ContentSearchFilter{
				Pattern: pattern, Mode: mode, Sources: []string{"tool_result"}, Limit: 10, IncludeOneShot: true,
			})
			require.NoError(t, err)
			for _, m := range page.Matches {
				assert.Equal(t, "tool_result", m.Location)
				assert.Equal(t, "Read", m.ToolName)
				assert.Contains(t, m.Snippet, "invoice 4827")
				out[mode] = append(out[mode], hit{m.SessionID, m.Ordinal})
			}
		}
		return out
	}
	published := func() []db.ImageTextRow {
		t.Helper()
		tx, err := pg.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		rows, err := publishedImageText(ctx, tx, "shot")
		require.NoError(t, err)
		return rows
	}
	staleUpdatedAt := func() {
		t.Helper()
		_, err := pg.ExecContext(ctx, `UPDATE sessions SET updated_at='2000-01-01T00:00:00Z' WHERE id='shot'`)
		require.NoError(t, err)
	}
	updatedAtStale := func() bool {
		t.Helper()
		var stale bool
		require.NoError(t, pg.QueryRowContext(ctx, `SELECT updated_at < '2001-01-01' FROM sessions WHERE id='shot'`).Scan(&stale))
		return stale
	}
	require.Empty(t, searches())
	require.Empty(t, published())

	// OCR-only change: the ordinary incremental push publishes it.
	staleUpdatedAt()
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	require.NoError(t, local.SaveImageText(ctx, hash, ocr.Identity{Fingerprint: "proc-a", Provider: "glm-ocr", Model: "glm-ocr", Revision: "r1"}, "invoice 4827", false))
	push()
	want := []hit{{"shot", 1}}
	assert.Equal(t, map[string][]hit{"global": want, "substring": want, "regex": want}, searches())
	assert.Equal(t, []db.ImageTextRow{{
		SessionID: "shot", SourceKind: "tool_call", MessageOrdinal: 1, ImageHash: hash,
		Processor: "proc-a", Provider: "glm-ocr", Model: "glm-ocr", Revision: "r1", Text: "invoice 4827",
	}}, published())
	assert.False(t, updatedAtStale(), "an OCR-only change must advance updated_at")
	// The image representative wins over the session's name match.
	_, err = pg.ExecContext(ctx, `UPDATE sessions SET display_name='invoice 4827 notes' WHERE id='shot'`)
	require.NoError(t, err)
	assert.Equal(t, want, searches()["global"])
	_, err = pg.ExecContext(ctx, `UPDATE sessions SET display_name=NULL WHERE id='shot'`)
	require.NoError(t, err)

	// An unchanged push keeps the rows and leaves updated_at alone.
	staleUpdatedAt()
	before := published()
	push()
	assert.Equal(t, before, published())
	assert.True(t, updatedAtStale(), "an unchanged push must not rewrite image text")

	// A new processor replaces the old one's row and metadata.
	require.NoError(t, local.SaveImageText(ctx, hash, ocr.Identity{Fingerprint: "proc-b", Provider: "glm-ocr", Model: "glm-ocr-2", Revision: "r2"}, "invoice 4827 total", false))
	push()
	assert.Equal(t, map[string][]hit{"global": want, "substring": want, "regex": want}, searches())
	got := published()
	require.Len(t, got, 1)
	assert.Equal(t, []string{hash, "proc-b", "glm-ocr", "glm-ocr-2", "r2", "invoice 4827 total"},
		[]string{got[0].ImageHash, got[0].Processor, got[0].Provider, got[0].Model, got[0].Revision, got[0].Text})

	// Removing the image removes the match after the next push.
	require.NoError(t, local.ReplaceSessionMessages(ctx, "shot", messages("no image here")))
	push()
	assert.Empty(t, searches())
	assert.Empty(t, published())
}
