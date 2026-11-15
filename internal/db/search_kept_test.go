//go:build fts5

package db

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

func TestSearchKeptSelections(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project")
	insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "needle", Timestamp: "2024-06-01T09:00:00Z"})
	var loads atomic.Int32
	d.searchKept.Size = func(SearchPage) int64 { loads.Add(1); return 1 }
	f := SearchFilter{Query: "needle"}
	page, err := d.Search(ctx, f)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	repeat, err := d.Search(ctx, f)
	require.NoError(t, err)
	require.Equal(t, page, repeat)
	require.EqualValues(t, 1, loads.Load())
	for _, other := range []SearchFilter{{Query: "needle", Sort: "recency"}, {Query: "needle", Cursor: 1}, {Query: "needle", Limit: 1}, {Query: "absent"}} {
		_, err = d.Search(ctx, other)
		require.NoError(t, err)
		_, err = d.Search(ctx, other)
		require.NoError(t, err)
	}
	require.EqualValues(t, 5, loads.Load())
}

func TestSearchKeptImageText(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.BackfillImageTextOccurrences(ctx))
	insertSession(t, d, "image", "project")
	content := testInlineImageContent()
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "image", Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	f := SearchFilter{Query: "invoice"}
	page, err := d.Search(ctx, f)
	require.NoError(t, err)
	require.Empty(t, page.Results)
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, "invoice", false))
	page, err = d.Search(ctx, f)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.NoError(t, d.SaveImageText(ctx, RetainedToolResultImages(content)[0].SHA256, identity, "receipt", false))
	page, err = d.Search(ctx, f)
	require.NoError(t, err)
	require.Empty(t, page.Results)
	page, err = d.Search(ctx, SearchFilter{Query: "receipt"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.NoError(t, d.ClearImageText(ctx))
	page, err = d.Search(ctx, SearchFilter{Query: "receipt"})
	require.NoError(t, err)
	require.Empty(t, page.Results)
	require.NoError(t, d.BackfillImageTextOccurrences(ctx))
	pending, err := d.PendingImageText(ctx, identity.Fingerprint, time.Now(), 10)
	require.NoError(t, err)
	require.Equal(t, []string{RetainedToolResultImages(content)[0].SHA256}, pending)
}

func TestSearchKeptReadOnlyBypassesCache(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "session", "project")
	insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "needle"})
	readOnly, err := OpenReadOnly(t.Context(), d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readOnly.Close()) })
	var loads atomic.Int32
	readOnly.searchKept.Size = func(SearchPage) int64 { loads.Add(1); return 1 }
	for range 2 {
		page, err := readOnly.Search(t.Context(), SearchFilter{Query: "needle"})
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
	}
	require.Zero(t, loads.Load())
}

func TestSearchKeptCJKAvailability(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS runtime is not installed")
	}
	d := testDB(t)
	ctx := t.Context()
	seedSearchSession(t, d, "chinese", "project", [][2]string{{"user", "法国的首都是巴黎。"}})
	require.True(t, d.HasCJKFTS(ctx))
	var loads atomic.Int32
	d.searchKept.Size = func(SearchPage) int64 { loads.Add(1); return 1 }
	filter := SearchFilter{Query: "巴黎"}
	page, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE stats SET value = 'stale' WHERE key = ?`, cjkFTSFingerprintStatsKey)
	require.NoError(t, err)
	require.False(t, d.HasCJKFTS(ctx))
	_, err = d.Search(ctx, filter)
	require.NoError(t, err)
	require.EqualValues(t, 2, loads.Load())
}

func TestSearchKeptSearchableEdits(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, *DB)
		edit func(*testing.T, *DB)
	}{
		{"message", func(t *testing.T, d *DB) {
			insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "needle first"})
		}, func(t *testing.T, d *DB) {
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session", []Message{{SessionID: "session", Role: "user", Content: "other"}}))
		}},
		{"name", func(t *testing.T, d *DB) {
			insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "other"})
			require.NoError(t, d.RenameSession(t.Context(), "session", new("needle first")))
		}, func(t *testing.T, d *DB) {
			require.NoError(t, d.RenameSession(t.Context(), "session", new("other")))
		}},
		{"first message", func(t *testing.T, d *DB) {
			insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "other"})
			_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET first_message='needle first' WHERE id='session'`)
			require.NoError(t, err)
		}, func(t *testing.T, d *DB) {
			_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET first_message='other' WHERE id='session'`)
			require.NoError(t, err)
		}},
		{"deleted", func(t *testing.T, d *DB) {
			insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "needle first"})
		}, func(t *testing.T, d *DB) {
			_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET deleted_at='2024-06-02' WHERE id='session'`)
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "session", "project")
			tc.seed(t, d)
			filter := SearchFilter{Query: "needle"}
			page, err := d.Search(t.Context(), filter)
			require.NoError(t, err)
			require.Len(t, page.Results, 1)
			tc.edit(t, d)
			page, err = d.Search(t.Context(), filter)
			require.NoError(t, err)
			require.Empty(t, page.Results)
		})
	}
}
