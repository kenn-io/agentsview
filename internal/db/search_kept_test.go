//go:build fts5

package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	_, err = d.getWriter().ExecContext(ctx, `INSERT INTO usage_events(session_id,source,model,output_tokens) VALUES('session','session','model-x',7)`)
	require.NoError(t, err)
	repeat, err = d.Search(ctx, f)
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
	original := simpleFTSRuntimeConfig.fingerprint
	t.Cleanup(func() { simpleFTSRuntimeConfig.fingerprint = original })
	simpleFTSRuntimeConfig.fingerprint = original + "-changed"
	_, err = d.getWriter().ExecContext(ctx, `UPDATE stats SET value = ? WHERE key = ?`, simpleFTSRuntimeConfig.fingerprint, cjkFTSFingerprintStatsKey)
	require.NoError(t, err)
	require.True(t, d.HasCJKFTS(ctx))
	page, err = d.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.EqualValues(t, 2, loads.Load())
	_, err = d.getWriter().ExecContext(ctx, `UPDATE stats SET value = 'stale' WHERE key = ?`, cjkFTSFingerprintStatsKey)
	require.NoError(t, err)
	require.False(t, d.HasCJKFTS(ctx))
	_, err = d.Search(ctx, filter)
	require.NoError(t, err)
	require.EqualValues(t, 3, loads.Load())
}

func TestCJKRepairPublishesSearchTokenWithIndex(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS runtime is not installed")
	}
	ctx := t.Context()
	d := testDB(t)
	for _, id := range []string{"first", "second"} {
		seedSearchSession(t, d, id, "project", [][2]string{{"user", "法国的首都是巴黎。"}})
	}
	_, err := d.getWriter().ExecContext(ctx, `INSERT INTO messages_cjk_fts(messages_cjk_fts, rowid, rendered, content)
 SELECT 'delete', id, content, COALESCE(dialogue_text, content) FROM messages WHERE session_id='second'`)
	require.NoError(t, err)
	filter := SearchFilter{Query: "巴黎"}
	page, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	var token string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='search_history_token'`).Scan(&token))
	_, err = d.getWriter().ExecContext(ctx, `DELETE FROM stats WHERE key=?`, cjkFTSFingerprintStatsKey)
	require.NoError(t, err)
	d.searchIndexRepairPending.Store(true)
	_, err = d.getWriter().ExecContext(ctx, `CREATE TRIGGER fail_search_token BEFORE INSERT ON archive_metadata WHEN NEW.key='search_history_token' BEGIN SELECT RAISE(ABORT, 'token failure'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, d.RepairMessageFTS(ctx), "token failure")
	require.False(t, d.HasCJKFTS(ctx), "a failed token update must roll back the rebuilt index")
	var fingerprint, after string
	require.ErrorIs(t, d.getReader().QueryRowContext(ctx, `SELECT value FROM stats WHERE key=?`, cjkFTSFingerprintStatsKey).Scan(&fingerprint), sql.ErrNoRows)
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='search_history_token'`).Scan(&after))
	require.Equal(t, token, after)
	_, err = d.getWriter().ExecContext(ctx, `DROP TRIGGER fail_search_token`)
	require.NoError(t, err)
	require.NoError(t, d.RepairMessageFTS(ctx))
	require.True(t, d.HasCJKFTS(ctx))
	page, err = d.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 2)
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='search_history_token'`).Scan(&after))
	require.NotEqual(t, token, after)
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

func TestSearchKeptDiskPages(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d := openSearchArchive(t, path)
	insertSession(t, d, "session", "project")
	require.NoError(t, d.Close())
	emptyBackup, err := os.ReadFile(path)
	require.NoError(t, err)
	d = openSearchArchive(t, path)
	insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "needle"})
	filter := SearchFilter{Query: "needle"}
	page, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.NoError(t, d.Close())
	d = openSearchArchive(t, path)
	loads := 0
	d.afterSearchRead = func() { loads++ }
	repeat, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, page, repeat)
	require.Zero(t, loads)
	require.NoError(t, d.Close())

	// Restoring the archive can repeat revision counts from the saved page.
	require.NoError(t, os.WriteFile(path, emptyBackup, 0o600))
	d = openSearchArchive(t, path)
	insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "other"})
	page, err = d.Search(ctx, filter)
	require.NoError(t, err)
	require.Empty(t, page.Results)
	require.NoError(t, d.Close())
	otherBackup, err := os.ReadFile(path)
	require.NoError(t, err)

	d = openSearchArchive(t, path)
	version := d.searchVersion(ctx, false)
	var hookErr error
	d.afterSearchRead = func() {
		_, hookErr = d.getWriter().ExecContext(ctx, `UPDATE sessions SET first_message = 'target' WHERE id = 'session'`)
	}
	filter = SearchFilter{Query: "target"}
	page, err = d.Search(ctx, filter)
	require.NoError(t, err)
	require.NoError(t, hookErr)
	require.Len(t, page.Results, 1)
	normalized := filter
	normalized.Limit = DefaultSearchLimit
	_, ok := d.searchKept.Get(normalized.Slot()+"|false", version)
	require.False(t, ok)
	require.NoError(t, d.Close())
	require.NoError(t, os.WriteFile(path, otherBackup, 0o600))
	d = openSearchArchive(t, path)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	loads = 0
	d.afterSearchRead = func() { loads++ }
	page, err = d.Search(ctx, filter)
	require.NoError(t, err)
	require.Empty(t, page.Results)
	require.Equal(t, 1, loads)
}

func TestSearchKeptHistoryToken(t *testing.T) {
	cases := []struct {
		name, setup, query string
		wantChange         bool
	}{
		{"listed column", "", `UPDATE sessions SET project='new' WHERE id='session'`, true},
		{"unlisted column", "", `UPDATE sessions SET health_score=42 WHERE id='session'`, false},
		{"image", `INSERT INTO image_text_occurrences(session_id,image_index,image_hash) VALUES('session',0,'hash')`, `INSERT INTO image_text_cache(image_hash,processor,provider,model,revision,text) VALUES('hash','processor','provider','model','revision','text')`, true},
		{"session insert", "", `INSERT INTO sessions(id,project,agent,machine) VALUES('other','project','agent','machine')`, true},
		{"session delete", "", `DELETE FROM sessions WHERE id='session'`, true},
		{"old writer", "", `UPDATE sessions SET session_name='needlename' WHERE id='session'`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "session", "project")
			if tc.name == "old writer" {
				const name = "trg_sessions_usage_change_update"
				creates := usageChangeJournalTriggerSQL
				start := strings.Index(creates, "CREATE TRIGGER "+name+" ")
				require.GreaterOrEqual(t, start, 0)
				end := strings.Index(creates[start:], "END;\n")
				require.GreaterOrEqual(t, end, 0)
				older := strings.ReplaceAll(creates[start:start+end+len("END;\n")], searchTokenSQL, "")
				_, err := d.getWriter().ExecContext(ctx, "DROP TRIGGER IF EXISTS "+name+";\n"+older)
				require.NoError(t, err)
			}
			if tc.setup != "" {
				_, err := d.getWriter().ExecContext(ctx, tc.setup)
				require.NoError(t, err)
			}
			var before, after string
			require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='search_history_token'`).Scan(&before))
			_, err := d.getWriter().ExecContext(ctx, tc.query)
			require.NoError(t, err)
			require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='search_history_token'`).Scan(&after))
			if tc.wantChange {
				require.NotEqual(t, before, after)
			} else {
				require.Equal(t, before, after)
			}
		})
	}
}

func TestSearchKeptRejectsOldRanking(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project")
	insertMessages(t, d, Message{SessionID: "session", Role: "user", Content: "needle"})
	var id, token string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT
		(SELECT value FROM archive_metadata WHERE key = ?),
		(SELECT value FROM archive_metadata WHERE key = 'search_history_token')`, archiveMetadataDatabaseIDKey).Scan(&id, &token))
	filter := SearchFilter{Query: "needle", Limit: 1}
	oldPage, err := json.Marshal(SearchPage{Results: []SearchResult{{SessionID: "old"}}})
	require.NoError(t, err)
	d.trendsPartsDisk()
	require.True(t, d.trendsDisk.Put("search|"+filter.Slot()+"|false", fmt.Sprintf("%s|%s|", id, token), oldPage))
	page, err := d.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Equal(t, "session", page.Results[0].SessionID)
}
