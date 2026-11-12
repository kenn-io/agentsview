//go:build pgtest

package postgres

import (
	"encoding/base64"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
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
	_, err = pg.ExecContext(ctx, `
		INSERT INTO image_text (session_id, source_kind, message_ordinal, call_index, event_index, image_index,
			image_hash, processor, provider, model, revision, text)
		SELECT session_id, source_kind, message_ordinal, call_index, event_index, image_index,
			image_hash, 'zzzz', provider, model, revision, 'invoice 4827 losing'
		FROM image_text WHERE session_id = 'shot'
	`)
	require.NoError(t, err)
	page, err := store.Search(ctx, db.SearchFilter{Query: "invoice 4827", Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, "invoice 4827", page.Results[0].Snippet)
	_, err = pg.ExecContext(ctx, `DELETE FROM image_text WHERE session_id = 'shot' AND processor = 'zzzz'`)
	require.NoError(t, err)
	wantRows, err := local.SessionImageTextRows(ctx, "shot")
	require.NoError(t, err)
	assert.Equal(t, wantRows, published())
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
	// A non-image change pushes the session while leaving its image rows intact.
	var beforeCTID, afterCTID string
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT ctid::text FROM image_text WHERE session_id='shot'`).Scan(&beforeCTID))
	name := "renamed"
	require.NoError(t, local.RenameSession(ctx, "shot", &name))
	push()
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT ctid::text FROM image_text WHERE session_id='shot'`).Scan(&afterCTID))
	assert.Equal(t, before, published())
	assert.Equal(t, beforeCTID, afterCTID)
	assert.False(t, updatedAtStale())

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

func TestPGStampedCodexPageMigration(t *testing.T) {
	const thread = "codex:11111111-1111-4111-8111-111111111111"
	const rollout = "22222222-2222-4222-8222-222222222222"
	const page = thread + "_" + rollout
	for _, tc := range []struct {
		name, old string
		version   int
	}{{"thread", thread, 130}, {"rollout", "codex:" + rollout, 130}} {
		t.Run(tc.name, func(t *testing.T) {
			old := tc.old
			ctx := t.Context()
			syncer, local, pg, _ := newSessionProvenancePushSync(t, "agentsview_stamped_codex_test")
			path := filepath.Join(t.TempDir(), "rollout-2026-09-25T12-00-00-"+page[len("codex:"):]+".jsonl")
			sess := db.Session{ID: old, Agent: "codex", Project: "proj", Machine: "workstation", FilePath: &path, MessageCount: 1}
			image := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
			msg := db.Message{SessionID: old, Role: "assistant", Content: "retained answer", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: image}}}
			require.NoError(t, local.UpsertSession(ctx, sess))
			require.NoError(t, local.InsertMessages(ctx, []db.Message{msg}))
			require.NoError(t, local.SetSessionDataVersion(ctx, old, tc.version))
			require.NoError(t, local.BackfillImageTextOccurrences(ctx))
			require.NoError(t, local.SaveImageText(ctx, db.RetainedToolResultImages(image)[0].SHA256, ocr.Identity{Fingerprint: "processor", Provider: "test", Model: "model", Revision: "r1"}, "invoice", false))
			result, err := syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			var version int
			require.NoError(t, pg.QueryRowContext(ctx, `SELECT data_version FROM sessions WHERE id=$1`, old).Scan(&version))
			require.Equal(t, db.ImageTextDataVersion, version)
			result, err = syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			var exists bool
			require.NoError(t, pg.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1)`, old).Scan(&exists))
			require.True(t, exists, "a missing canonical page must preserve the old session")
			store := &Store{pg: pg}
			_, err = store.PinMessage(ctx, old, 0, new("note"))
			require.NoError(t, err)
			rebuilt := testDB(t)
			require.NoError(t, rebuilt.CopyArchiveIdentityFrom(ctx, local.Path()))
			require.NoError(t, rebuilt.CopySyncStateFrom(ctx, local.Path()))
			sess.ID, sess.ParentSessionID, sess.RelationshipType = page, new(thread), "continuation"
			msg.SessionID = page
			require.NoError(t, rebuilt.UpsertSession(ctx, sess))
			require.NoError(t, rebuilt.InsertMessages(ctx, []db.Message{msg}))
			require.NoError(t, rebuilt.SetSessionDataVersion(ctx, page, 169))
			require.NoError(t, rebuilt.BackfillImageTextOccurrences(ctx))
			require.NoError(t, rebuilt.SaveImageText(ctx, db.RetainedToolResultImages(image)[0].SHA256, ocr.Identity{Fingerprint: "processor", Provider: "test", Model: "model", Revision: "r1"}, "invoice", false))
			upgraded, err := New(ctx, testPGURL(t), syncer.schema, rebuilt, "workstation", true, storage.PusherOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
			upgraded.archiveID, err = rebuilt.GetArchiveID(ctx)
			require.NoError(t, err)
			var owner string
			require.NoError(t, pg.QueryRowContext(ctx, `SELECT owner_marker FROM sessions WHERE id=$1`, old).Scan(&owner))
			require.NoError(t, upgraded.reconcilePGMissingSessions(ctx, owner, nil))
			require.NoError(t, pg.QueryRowContext(ctx, `SELECT data_version FROM sessions WHERE id=$1`, page).Scan(&version))
			require.Equal(t, db.ImageTextDataVersion, version)
			var imageCount int
			require.NoError(t, pg.QueryRowContext(ctx, `SELECT count(*) FROM image_text WHERE session_id=$1`, page).Scan(&imageCount))
			require.Equal(t, 1, imageCount)
			result, err = upgraded.Push(ctx, true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			pins, err := store.ListPinnedMessages(ctx, page, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			retired, err := store.GetSessionFull(ctx, old)
			require.NoError(t, err)
			require.Nil(t, retired)
		})
	}
}

func TestPGImageTextPendingSessionVersion(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(t, "agentsview_image_text_version_test")
	image := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "pending", Project: "proj", Machine: "workstation", Agent: "test"}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: "pending", Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: image}}}}))
	require.NoError(t, local.SetSessionDataVersion(ctx, "pending", 169))
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	require.NoError(t, local.SaveImageText(ctx, db.RetainedToolResultImages(image)[0].SHA256, ocr.Identity{Fingerprint: "processor", Provider: "test", Model: "model", Revision: "r1"}, "invoice", false))
	push := func() {
		t.Helper()
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
	}
	version := func() int {
		t.Helper()
		var v int
		require.NoError(t, pg.QueryRowContext(ctx, `SELECT data_version FROM sessions WHERE id='pending'`).Scan(&v))
		return v
	}
	push()
	require.Equal(t, db.ImageTextDataVersion, version())
	require.Equal(t, 169, local.GetSessionDataVersion(ctx, "pending"))
	push()
	store := &Store{pg: pg}
	require.NoError(t, store.SoftDeleteSession(ctx, "pending"))
	_, err := store.RestoreSession(ctx, "pending")
	require.NoError(t, err)
	require.Equal(t, max(db.CurrentDataVersion()-1, db.ImageTextDataVersion), version())
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "plain", Project: "proj", Machine: "workstation", Agent: "test"}))
	push()
	require.NoError(t, store.SoftDeleteSession(ctx, "plain"))
	_, err = store.RestoreSession(ctx, "plain")
	require.NoError(t, err)
	var plainVersion int
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT data_version FROM sessions WHERE id='plain'`).Scan(&plainVersion))
	require.Equal(t, db.CurrentDataVersion()-1, plainVersion)
	require.NoError(t, local.ClearImageText(ctx))
	push()
	require.Equal(t, 169, version())
	var count int
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT count(*) FROM image_text WHERE session_id='pending'`).Scan(&count))
	require.Zero(t, count)
}
