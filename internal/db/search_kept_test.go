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

func openSearchArchive(t *testing.T, path string) *DB {
	t.Helper()
	d, err := Open(t.Context(), path)
	require.NoError(t, err)
	return d
}

func TestSearchKeptRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d := openSearchArchive(t, path)
	insertSession(t, d, "session", "project")
	insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "needle"})
	filter := SearchFilter{Query: "needle"}
	page, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.NoError(t, d.Close())
	d = openSearchArchive(t, path)
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
	d := openSearchArchive(t, path)
	insertSession(t, d, "session", "project")
	require.NoError(t, d.Close())
	backup, err := os.ReadFile(path)
	require.NoError(t, err)
	write := func(content string) SearchPage {
		t.Helper()
		d := openSearchArchive(t, path)
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
	fallback, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.Empty(t, fallback.Results)
}

func TestSearchKeptRejectsChangedSource(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d := openSearchArchive(t, path)
	insertSession(t, d, "session", "project")
	insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "other"})
	require.NoError(t, d.Close())
	backup, err := os.ReadFile(path)
	require.NoError(t, err)
	d = openSearchArchive(t, path)
	version := d.searchVersion(ctx, false)
	var hookErr error
	d.afterSearchRead = func() {
		_, hookErr = d.getWriter().ExecContext(ctx, `UPDATE sessions SET first_message = 'needle' WHERE id = 'session'`)
	}
	filter := SearchFilter{Query: "needle"}
	page, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.NoError(t, hookErr)
	require.Len(t, page.Results, 1)
	normalized := filter
	normalized.Limit = DefaultSearchLimit
	_, ok := d.searchKept.Get(normalized.Slot()+"|false", version)
	require.False(t, ok)
	require.NoError(t, d.Close())
	require.NoError(t, os.WriteFile(path, backup, 0o600))
	d = openSearchArchive(t, path)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	_, err = d.getWriter().ExecContext(ctx, `DROP TABLE messages_fts`)
	require.NoError(t, err)
	_, err = d.Search(ctx, filter)
	require.Error(t, err)
}

func TestSearchKeptHistoryTokenCounts(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project")
	insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "other"})
	_, err := d.getWriter().ExecContext(ctx, `CREATE TABLE token_writes (n INTEGER);
CREATE TRIGGER count_token_writes AFTER UPDATE ON archive_metadata WHEN NEW.key = 'search_history_token'
BEGIN INSERT INTO token_writes VALUES (1); END;`)
	require.NoError(t, err)
	cases := []struct {
		name, query string
		want        int
	}{
		{"group", `UPDATE sessions SET group_key='group' WHERE id='session'`, 1},
		{"grouped rename", `UPDATE sessions SET session_name='name' WHERE id='session'`, 1},
		{"listed column", `UPDATE sessions SET project='new' WHERE id='session'`, 1},
		{"unlisted column", `UPDATE sessions SET health_score=42 WHERE id='session'`, 0},
		{"first message", `UPDATE sessions SET first_message='first' WHERE id='session'`, 1},
		{"dialogue", `UPDATE messages SET dialogue_text='changed' WHERE session_id='session'`, 1},
		{"image", `INSERT INTO image_text_cache(id,image_hash,processor,provider,model,revision,text) VALUES(-1,'hash','processor','provider','model','revision','text')`, 1},
		{"usage insert", `INSERT INTO usage_events(session_id,source,model,output_tokens) VALUES('session','session','model-x',7)`, 0},
		{"session insert", `INSERT INTO sessions(id,project,agent,machine) VALUES('other','project','agent','machine')`, 1},
		{"session delete", `DELETE FROM sessions WHERE id='session'`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.getWriter().ExecContext(ctx, `DELETE FROM token_writes`)
			require.NoError(t, err)
			_, err = d.getWriter().ExecContext(ctx, tc.query)
			require.NoError(t, err)
			var count int
			require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT count(*) FROM token_writes`).Scan(&count))
			require.Equal(t, tc.want, count)
		})
	}
}

func TestSearchKeptOldWriterTriggers(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project")
	insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "other"})
	_, err := d.getWriter().ExecContext(ctx, `
DROP TRIGGER trg_sessions_usage_change_update;
CREATE TRIGGER trg_sessions_usage_change_update AFTER UPDATE OF session_name ON sessions
WHEN OLD.session_name IS NOT NEW.session_name BEGIN
 INSERT INTO archive_metadata(key,value) VALUES('usage_change_revision','1') ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1;
END;
DROP TRIGGER trg_sessions_search_first_message;
CREATE TRIGGER trg_sessions_search_first_message AFTER UPDATE OF first_message ON sessions
WHEN OLD.first_message IS NOT NEW.first_message BEGIN
 INSERT INTO archive_metadata(key,value) VALUES('search_change_revision','1') ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1;
END;
DROP TRIGGER trg_messages_search_dialogue;
CREATE TRIGGER trg_messages_search_dialogue AFTER UPDATE OF dialogue_text ON messages
WHEN OLD.dialogue_text IS NOT NEW.dialogue_text BEGIN
 INSERT INTO archive_metadata(key,value) VALUES('search_change_revision','1') ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1;
END;`)
	require.NoError(t, err)
	for _, tc := range []struct{ update, search string }{
		{`UPDATE sessions SET session_name='needlename' WHERE id='session'`, "needlename"},
		{`UPDATE sessions SET first_message='needlefirst' WHERE id='session'`, "needlefirst"},
		{`UPDATE messages SET dialogue_text='needledialogue' WHERE session_id='session'`, `"other" AND content:needledialogue`},
	} {
		filter := SearchFilter{Query: tc.search}
		version := d.searchVersion(ctx, false)
		var token string
		require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='search_history_token'`).Scan(&token))
		page, err := d.Search(ctx, filter)
		require.NoError(t, err)
		require.Empty(t, page.Results)
		_, err = d.getWriter().ExecContext(ctx, tc.update)
		require.NoError(t, err)
		require.NotEqual(t, version, d.searchVersion(ctx, false))
		var changedToken string
		require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='search_history_token'`).Scan(&changedToken))
		require.NotEqual(t, token, changedToken)
		page, err = d.Search(ctx, filter)
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
	}
}
