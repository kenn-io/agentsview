//go:build fts5

package db

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
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
	readOnly, err := OpenReadOnly(ctx, d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readOnly.Close()) })
	var readOnlyLoads atomic.Int32
	readOnly.searchKept.Size = func(SearchPage) int64 { readOnlyLoads.Add(1); return 1 }
	for range 2 {
		page, err := readOnly.Search(ctx, f)
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
	}
	require.Zero(t, readOnlyLoads.Load())
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

func TestSearchKeptFirstMessageEdit(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	session := Session{ID: "session", Project: "project", Machine: defaultMachine, Agent: defaultAgent, MessageCount: 1, FirstMessage: new("needle")}
	require.NoError(t, d.UpsertSession(ctx, session))
	insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "other"})
	var loads atomic.Int32
	d.searchKept.Size = func(SearchPage) int64 { loads.Add(1); return 1 }
	filter := SearchFilter{Query: "needle"}
	page, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	_, err = d.Search(ctx, filter)
	require.NoError(t, err)
	require.EqualValues(t, 1, loads.Load())
	session.FirstMessage = new("other")
	require.NoError(t, d.UpsertSession(ctx, session))
	page, err = d.Search(ctx, filter)
	require.NoError(t, err)
	require.Empty(t, page.Results)
}

func TestSearchKeptSearchableEdits(t *testing.T) {
	dialogue := func(content string, text *string, messagesOnly bool) func(*testing.T, *DB) {
		return func(t *testing.T, d *DB) {
			t.Helper()
			messages := []Message{{SessionID: "session", Role: "assistant", Content: content, DialogueText: text}}
			if messagesOnly {
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session", messages))
			} else {
				require.NoError(t, d.ReplaceSessionContent(t.Context(), "session", messages, SessionSignalUpdate{}, nil))
			}
		}
	}
	for _, tc := range []struct {
		name  string
		seed  func(*testing.T, *DB)
		edit  func(*testing.T, *DB)
		query string
	}{
		{"message", func(t *testing.T, d *DB) {
			t.Helper()
			insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "needle first"})
		}, func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session", []Message{{SessionID: "session", Role: "user", Content: "other"}}))
		}, ""},
		{"dialogue full content replacement", dialogue("raw words", new("needle"), false), dialogue("raw words", new("different"), false), `"raw" AND content:needle`},
		{"dialogue full message replacement", dialogue("raw words", new("needle"), true), dialogue("raw words", new("different"), true), `"raw" AND content:needle`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "session", "project")
			tc.seed(t, d)
			query := tc.query
			if query == "" {
				query = "needle"
			}
			filter := SearchFilter{Query: query}
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

func TestSearchKeptRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := Open(ctx, path)
	require.NoError(t, err)
	insertSession(t, d, "session", "project")
	insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "needle"})
	filter := SearchFilter{Query: "needle"}
	page, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.NoError(t, d.Close())
	d, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	// An unavailable index proves the reopened search uses the saved page.
	_, err = d.getWriter().ExecContext(ctx, `DROP TABLE messages_fts`)
	require.NoError(t, err)
	repeat, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, page, repeat)
	_, err = d.Search(ctx, SearchFilter{Query: "needle", Cursor: 1})
	require.Error(t, err)
}

func TestSearchKeptRestore(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	open := func() *DB {
		t.Helper()
		d, err := Open(ctx, path)
		require.NoError(t, err)
		return d
	}
	d := open()
	insertSession(t, d, "session", "project")
	require.NoError(t, d.Close())
	backup, err := os.ReadFile(path)
	require.NoError(t, err)
	write := func(content string) SearchPage {
		t.Helper()
		d := open()
		insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: content})
		page, err := d.Search(ctx, SearchFilter{Query: "needle"})
		require.NoError(t, err)
		require.NoError(t, d.Close())
		return page
	}
	require.Len(t, write("needle").Results, 1)
	require.NoError(t, os.WriteFile(path, backup, 0o600))
	require.Empty(t, write("other").Results)
}

func TestSearchKeptRestartDialogueEdit(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := Open(ctx, path)
	require.NoError(t, err)
	insertSession(t, d, "session", "project")
	insertMessages(t, d, Message{SessionID: "session", Role: "assistant", Content: "raw words", DialogueText: new("needle")})
	filter := SearchFilter{Query: `"raw" AND content:needle`}
	page, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE messages SET dialogue_text = 'other' WHERE session_id = 'session'`)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	d, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	page, err = d.Search(ctx, filter)
	require.NoError(t, err)
	require.Empty(t, page.Results)
}

func TestSearchKeptCJKFingerprint(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS runtime is not installed")
	}
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	seedSearchSession(t, d, "chinese", "project", [][2]string{{"user", "法国的首都是巴黎。"}})
	filter := SearchFilter{Query: "巴黎"}
	page, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.NotEmpty(t, page.Results)
	original := simpleFTSRuntimeConfig.fingerprint
	t.Cleanup(func() { simpleFTSRuntimeConfig.fingerprint = original })
	simpleFTSRuntimeConfig.fingerprint = original + "-changed"
	_, err = d.getWriter().ExecContext(ctx, `UPDATE stats SET value = ? WHERE key = ?`, simpleFTSRuntimeConfig.fingerprint, cjkFTSFingerprintStatsKey)
	require.NoError(t, err)
	require.True(t, d.HasCJKFTS(ctx))
	_, err = d.getWriter().ExecContext(ctx, `DROP TABLE messages_cjk_fts`)
	require.NoError(t, err)
	_, err = d.Search(ctx, filter)
	require.Error(t, err)
}
