//go:build chtest

package clickhouse

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

const imageSessionID = "ch-image"

func inlineImage(payload string) string {
	data := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + payload))
	return `[{"type":"input_image","image_url":"data:image/png;base64,` + data + `"}]`
}

// writeImageSession stores a session whose call at ordinal 1 returns
// callContent and whose call at ordinal 3 returns eventContent as a result
// event. Its name matches the event image's text, so that image's anchor
// must win over the name match.
func writeImageSession(t *testing.T, local *db.DB, callContent, eventContent, ts string) {
	t.Helper()
	sess := fixtureSession(imageSessionID, "image", "show screenshots", ts, 4)
	sess.UserMessageCount = 2
	name := "receipt review"
	sess.SessionName = &name
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session: sess,
		Messages: []db.Message{
			fixtureMessage(imageSessionID, 0, "user", "show screenshots", ts),
			fixtureMessage(imageSessionID, 1, "assistant", "reading", ts,
				db.ToolCall{ToolName: "Read", Category: "Read", ToolUseID: "img-call", ResultContent: callContent}),
			fixtureMessage(imageSessionID, 2, "user", "and the other", ts),
			fixtureMessage(imageSessionID, 3, "assistant", "viewing", ts,
				db.ToolCall{ToolName: "View", Category: "Read", ToolUseID: "img-event", ResultEvents: []db.ToolResultEvent{{
					Source: "tool", Status: "complete", Content: eventContent, Timestamp: ts,
				}}}),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
}

type searchHit struct {
	Session, Location, Tool string
	Ordinal                 int
}

// Image text pushed after OCR must search the same as the archive does: one
// global result anchored at the call's message, and tool_result rows in
// substring and regex modes.
func TestClickHouseImageTextPushAndSearch(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	callImage, eventImage := inlineImage("call"), inlineImage("event")
	writeImageSession(t, local, callImage, eventImage, "2026-01-12T00:00:00.000Z")
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	search := func(t *testing.T, read interface {
		Search(ctx context.Context, f db.SearchFilter) (db.SearchPage, error)
		SearchContent(ctx context.Context, f db.ContentSearchFilter) (db.ContentSearchPage, error)
	}, query, pattern string) (global, substring, regex []searchHit) {
		t.Helper()
		page, err := read.Search(ctx, db.SearchFilter{Query: query, Limit: 20})
		require.NoError(t, err)
		for _, r := range page.Results {
			global = append(global, searchHit{Session: r.SessionID, Ordinal: r.Ordinal})
		}
		for mode, p := range map[string]string{"substring": query, "regex": pattern} {
			content, err := read.SearchContent(ctx, db.ContentSearchFilter{
				Pattern: p, Mode: mode, Sources: []string{"tool_result"}, Limit: 20,
				IncludeChildren: true, IncludeAutomated: true, IncludeOneShot: true,
			})
			require.NoError(t, err)
			var hits []searchHit
			for _, m := range content.Matches {
				hits = append(hits, searchHit{m.SessionID, m.Location, m.ToolName, m.Ordinal})
			}
			if mode == "regex" {
				regex = hits
			} else {
				substring = hits
			}
		}
		return global, substring, regex
	}
	// assertSearch checks the expected image anchor in the mirror.
	assertSearch := func(t *testing.T, query, pattern string, ordinal int, tool string) {
		t.Helper()
		gotGlobal, gotSubstring, gotRegex := search(t, store, query, pattern)
		var expected, expectedContent []searchHit
		if ordinal >= 0 {
			expected = []searchHit{{Session: imageSessionID, Ordinal: ordinal}}
			expectedContent = []searchHit{{imageSessionID, "tool_result", tool, ordinal}}
		}
		assert.Equal(t, expected, gotGlobal, "mirror global %q", query)
		assert.Equal(t, expectedContent, gotSubstring, "mirror substring %q", query)
		assert.Equal(t, expectedContent, gotRegex, "mirror regex %q", pattern)
	}
	assertRows := func(t *testing.T) {
		t.Helper()
		want, err := local.SessionImageTextRows(ctx, imageSessionID)
		require.NoError(t, err)
		require.NotEmpty(t, want)
		rows, err := store.DB().QueryContext(ctx, `SELECT session_id, source_kind, message_ordinal,
			call_index, event_index, image_index, image_hash, processor, provider, model, revision, text
			FROM image_text ORDER BY session_id, source_kind, message_ordinal, call_index, event_index, image_index, processor`)
		require.NoError(t, err)
		defer rows.Close()
		var got []db.ImageTextRow
		for rows.Next() {
			var r db.ImageTextRow
			require.NoError(t, rows.Scan(&r.SessionID, &r.SourceKind, &r.MessageOrdinal, &r.CallIndex,
				&r.EventIndex, &r.ImageIndex, &r.ImageHash, &r.Processor, &r.Provider, &r.Model, &r.Revision, &r.Text))
			got = append(got, r)
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, want, got)
	}

	// Before OCR nothing matches.
	assertSearch(t, "invoice", `invoice \d+`, -1, "")
	identity := ocr.Identity{Fingerprint: "proc-a", Provider: "glm-ocr", Model: "glm-ocr", Revision: "r1"}
	callHash := db.RetainedToolResultImages(callImage)[0].SHA256
	eventHash := db.RetainedToolResultImages(eventImage)[0].SHA256
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	require.NoError(t, local.SaveImageText(ctx, callHash, identity, "invoice 4827", false))
	require.NoError(t, local.SaveImageText(ctx, eventHash, identity, "receipt 9931", false))

	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, result.Full)
	assert.Equal(t, 1, result.SessionsPushed, "OCR alone republishes the session")
	assertRows(t)
	assertSearch(t, "invoice", `invoice \d+`, 1, "Read")
	assertSearch(t, "receipt", `rec.ipt 99`, 3, "")
	page, err := store.Search(ctx, db.SearchFilter{Query: "invoice", Limit: 5})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, "invoice 4827", page.Results[0].Snippet)
	for project, want := range map[string]int{"image": 1, "alpha": 0} {
		page, err := store.Search(ctx, db.SearchFilter{Query: "invoice", Project: project,
			DateFrom: "2026-01-01", DateTo: "2026-12-31", Limit: 5})
		require.NoError(t, err)
		assert.Len(t, page.Results, want, "project %s", project)
	}

	t.Run("unchanged push keeps rows", func(t *testing.T) {
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		assert.Zero(t, result.SessionsPushed)
		assertRows(t)
		assertSearch(t, "invoice", `invoice \d+`, 1, "Read")
	})

	t.Run("processor change", func(t *testing.T) {
		next := ocr.Identity{Fingerprint: "proc-b", Provider: "other-ocr", Model: "other-model", Revision: "r2"}
		require.NoError(t, local.SaveImageText(ctx, callHash, next, "invoice 4827 total", false))
		_, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		assertRows(t)
		var processor string
		require.NoError(t, store.DB().QueryRowContext(ctx,
			"SELECT processor FROM image_text WHERE source_kind = 'tool_call'").Scan(&processor))
		assert.Equal(t, "proc-b", processor)
		assertSearch(t, "invoice", `invoice \d+ total`, 1, "Read")
	})

	t.Run("backfill after archive rebuild", func(t *testing.T) {
		_, err := store.DB().ExecContext(ctx, `DELETE FROM image_text WHERE 1 SETTINGS mutations_sync=2`)
		require.NoError(t, err)
		require.NoError(t, local.SetDatabaseIDForTest(ctx, "rebuilt-archive"))
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		assert.True(t, result.Full)
		assert.Zero(t, result.Errors)
		assertRows(t)
		assertSearch(t, "invoice", `invoice \d+ total`, 1, "Read")
	})

	t.Run("image removed", func(t *testing.T) {
		writeImageSession(t, local, "no screenshot here", eventImage, "2026-01-12T00:10:00.000Z")
		_, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		assertRows(t)
		assertSearch(t, "invoice", `invoice \d+`, -1, "")
		assertSearch(t, "receipt", `rec.ipt 99`, 3, "")
	})

	t.Run("session deleted", func(t *testing.T) {
		require.NoError(t, local.DeleteSession(ctx, imageSessionID))
		_, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		var count uint64
		require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT count() FROM image_text").Scan(&count))
		assert.Zero(t, count)
	})
}

func TestClickHouseImageTextContentPageOrder(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	content := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"input_image","image_url":"data:image/png;base64,AwQF"}]`
	writeImageSession(t, local, content, "no image", "2026-01-12T00:00:00.000Z")
	images := db.RetainedToolResultImages(content)
	require.Len(t, images, 2)
	identity := ocr.Identity{Fingerprint: "proc-a", Provider: "glm-ocr", Model: "glm-ocr", Revision: "r1"}
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	require.NoError(t, local.SaveImageText(ctx, images[0].SHA256, identity, "needle zebra", false))
	require.NoError(t, local.SaveImageText(ctx, images[1].SHA256, identity, "needle apple "+strings.Repeat("界", 250)+" invoice example", false))
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	filter := db.ContentSearchFilter{Pattern: "needle", Mode: "substring", Sources: []string{"tool_result"}, Limit: 1, IncludeChildren: true, IncludeOneShot: true, IncludeAutomated: true}
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
	require.Equal(t, imageSessionID, search.Results[0].SessionID)
	require.Equal(t, 1, search.Results[0].Ordinal)
	require.Contains(t, search.Results[0].Snippet, "invoice")
}

func TestClickHouseImageTextPendingSessionVersion(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	image := inlineImage("pending")
	writeImageSession(t, local, image, "no image", "2026-01-12T00:00:00.000Z")
	require.NoError(t, local.SetSessionDataVersion(ctx, imageSessionID, 169))
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	require.NoError(t, local.SaveImageText(ctx, db.RetainedToolResultImages(image)[0].SHA256, ocr.Identity{Fingerprint: "processor", Provider: "test", Model: "model", Revision: "r1"}, "invoice", false))
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	syncer.hooks = &pushHooks{beforeSessionRows: func(batch []db.Session) error {
		for _, session := range batch {
			if session.ID == imageSessionID {
				var images, sessions uint64
				require.NoError(t, syncer.conn.QueryRowContext(ctx, `SELECT count() FROM image_text WHERE session_id=?`, imageSessionID).Scan(&images))
				require.NoError(t, syncer.conn.QueryRowContext(ctx, `SELECT count() FROM sessions FINAL WHERE id=?`, imageSessionID).Scan(&sessions))
				require.EqualValues(t, 1, images)
				require.Zero(t, sessions)
				syncer.hooks = nil
				return errors.New("interrupted before session rows")
			}
		}
		return nil
	}}
	push := func() {
		t.Helper()
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
	}
	version := func() int {
		t.Helper()
		var v int
		require.NoError(t, syncer.conn.QueryRowContext(ctx, `SELECT data_version FROM sessions FINAL WHERE id=?`, imageSessionID).Scan(&v))
		return v
	}
	push()
	require.Equal(t, db.ImageTextDataVersion, version())
	require.Equal(t, 169, local.GetSessionDataVersion(ctx, imageSessionID))
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	require.Zero(t, result.SessionsPushed)
	require.Equal(t, db.ImageTextDataVersion, version())
	require.NoError(t, local.ClearImageText(ctx))
	push()
	require.Equal(t, 169, version())
	var count uint64
	require.NoError(t, syncer.conn.QueryRowContext(ctx, `SELECT count() FROM image_text WHERE session_id=?`, imageSessionID).Scan(&count))
	require.Zero(t, count)
}

func TestClickHouseImageTextHiddenAfterNewerSessionRow(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	image := inlineImage("interrupted")
	writeImageSession(t, local, image, "no image", "2026-01-12T00:00:00.000Z")
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	require.NoError(t, local.SaveImageText(ctx, db.RetainedToolResultImages(image)[0].SHA256, ocr.Identity{Fingerprint: "processor", Provider: "test", Model: "model", Revision: "r1"}, "invoice", false))
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	check := func(want int) {
		t.Helper()
		global, err := store.Search(ctx, db.SearchFilter{Query: "invoice"})
		require.NoError(t, err)
		assert.Len(t, global.Results, want)
		content, err := store.SearchContent(ctx, db.ContentSearchFilter{Pattern: "invoice", Mode: "substring", Sources: []string{"tool_result"}, IncludeChildren: true, IncludeAutomated: true, IncludeOneShot: true})
		require.NoError(t, err)
		assert.Len(t, content.Matches, want)
	}
	check(1)
	_, err = syncer.conn.ExecContext(ctx, `INSERT INTO sessions SELECT * REPLACE (push_version + 1 AS push_version, 172 AS data_version, 'older' AS agentsview_push_fingerprint) FROM sessions FINAL WHERE id=?`, imageSessionID)
	require.NoError(t, err)
	check(0)
	result, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	check(1)
}
