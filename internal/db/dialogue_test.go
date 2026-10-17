package db

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDialogueColumnUpgradeKeepsArchiveInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	insertSession(t, d, "pi:kept", "project", func(s *Session) { s.Agent = "pi"; s.UserMessageCount = 1 })
	const content = "[Thinking]\nreasoningneedle\n[/Thinking]\nAnswer"
	insertMessages(t, d,
		Message{SessionID: "pi:kept", Ordinal: 0, Role: "user", Content: "Question"},
		Message{SessionID: "pi:kept", Ordinal: 1, Role: "assistant", Content: content, ThinkingText: "reasoningneedle", HasThinking: true},
	)
	before, err := d.GetAllMessages(t.Context(), "pi:kept")
	require.NoError(t, err)
	require.NoError(t, d.DropFTS(t.Context()))
	_, err = d.getWriter().Exec(t.Context(), fmt.Sprintf(`DROP VIEW message_search_content;
 ALTER TABLE messages DROP COLUMN dialogue_text;
 CREATE VIRTUAL TABLE messages_fts USING fts5(content, content='messages', content_rowid='id', tokenize='porter unicode61');
 CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN INSERT INTO messages_fts(rowid,content) VALUES(new.id,new.content); END;
 CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN INSERT INTO messages_fts(messages_fts,rowid,content) VALUES('delete',old.id,old.content); END;
 CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN
 INSERT INTO messages_fts(messages_fts,rowid,content) VALUES('delete',old.id,old.content);
 INSERT INTO messages_fts(rowid,content) VALUES(new.id,new.content); END;
 INSERT INTO messages_fts(messages_fts) VALUES('rebuild');
 PRAGMA user_version=%d; UPDATE sessions SET data_version=%d;`, CurrentDataVersion()-1, CurrentDataVersion()-1))
	require.NoError(t, err)
	require.NoError(t, d.Close())
	d, err = Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	assert.False(t, d.NeedsResync())
	stored, err := d.GetAllMessages(t.Context(), "pi:kept")
	require.NoError(t, err)
	require.Len(t, stored, 2)
	assert.Equal(t, before[1].ID, stored[1].ID)
	assert.Equal(t, content, stored[1].Content)
	assert.Nil(t, stored[1].DialogueText)
	assert.Equal(t, "reasoningneedle", stored[1].ThinkingText)
	session, err := d.GetSession(t.Context(), "pi:kept")
	require.NoError(t, err)
	assert.Equal(t, CurrentDataVersion()-1, session.DataVersion)
	for _, mode := range []string{"fts", "terms"} {
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{SessionID: "pi:kept", Pattern: "reasoningneedle", Mode: mode, IncludeOneShot: true})
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		assert.Equal(t, "message", page.Matches[0].Location)
	}
	// A source-backed reparse replaces the projection without changing message IDs.
	stored[1].DialogueText = new("Answer")
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "pi:kept", stored))
	for _, mode := range []string{"fts", "terms"} {
		for _, query := range []struct {
			term    string
			matches int
		}{{"reasoningneedle", 0}, {"Answer", 1}} {
			page, err := d.SearchContent(t.Context(), ContentSearchFilter{SessionID: "pi:kept", Pattern: query.term, Mode: mode, Sources: []string{"messages"}, IncludeOneShot: true})
			require.NoError(t, err)
			assert.Len(t, page.Matches, query.matches, mode)
		}
	}
}

func TestDialogueCJKProjectionRefresh(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	d := testDB(t)
	insertSession(t, d, "cjk", "project", func(s *Session) { s.UserMessageCount = 2 })
	insertMessages(t, d, Message{SessionID: "cjk", Role: "assistant", Content: "中文搜索 工具输出", DialogueText: new("中文搜索")})
	// A stale fingerprint uses the same refresh path as the new index expression.
	_, err := d.getWriter().Exec(t.Context(), "UPDATE stats SET value='old-runtime' WHERE key=?", cjkFTSFingerprintStatsKey)
	require.NoError(t, err)
	require.NoError(t, ensureCJKFTS(t.Context(), d.getWriter(), false))
	for _, query := range []struct {
		term    string
		matches int
	}{{"中文搜索", 1}, {"工具输出", 0}} {
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{SessionID: "cjk", Pattern: query.term, Mode: "fts", Sources: []string{"messages"}})
		require.NoError(t, err)
		assert.Len(t, page.Matches, query.matches)
	}
	messages, err := d.GetAllMessages(t.Context(), "cjk")
	require.NoError(t, err)
	messages[0].DialogueText = new("新的回答")
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "cjk", messages))
	page, err := d.SearchContent(t.Context(), ContentSearchFilter{SessionID: "cjk", Pattern: "新的回答", Mode: "fts", Sources: []string{"messages"}})
	require.NoError(t, err)
	assert.Len(t, page.Matches, 1)
}

func TestDialogueProjectionKeepsCitationMembers(t *testing.T) {
	d := testDB(t)
	seedUnitSession(t, d, "members", nil, []Message{
		{Ordinal: 0, Role: "user", Content: "question"},
		{Ordinal: 1, Role: "assistant", Content: "[Read: file.txt]", DialogueText: new("")},
		{Ordinal: 2, Role: "assistant", Content: "[Thinking]\nreasoningneedle\n[/Thinking]", ThinkingText: "reasoningneedle", DialogueText: new("")},
		{Ordinal: 3, Role: "assistant", Content: "answerneedle", DialogueText: new("answerneedle")},
	})
	units, _ := scanUnits(t, d, "", true)
	require.Len(t, units, 2)
	assert.Equal(t, [2]int{1, 3}, [2]int{units[1].Ordinal, units[1].OrdinalEnd})
	assert.Equal(t, []int{1, 2, 3}, unitMembers(units[1]))
	assert.Contains(t, units[1].Content, "[Read: file.txt]")
	assert.Contains(t, units[1].Content, "reasoningneedle")
	for _, mode := range []string{"substring", "regex"} {
		for _, source := range []struct {
			term, location string
			sources        []string
		}{
			{"answerneedle", "message", []string{"messages"}},
			{"reasoningneedle", "thinking", []string{"thinking"}},
			{"reasoningneedle", "thinking", nil},
			{"reasoningneedle", "", []string{"messages"}},
			{"file.txt", "", []string{"messages"}},
		} {
			page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: source.term, Mode: mode, Sources: source.sources})
			require.NoError(t, err)
			if source.location == "" {
				assert.Empty(t, page.Matches, "%s messages matched %s", mode, source.term)
				continue
			}
			require.Len(t, page.Matches, 1)
			assert.Equal(t, source.location, page.Matches[0].Location)
			assert.Equal(t, [2]int{1, 3}, page.Matches[0].OrdinalRange)
		}
	}
}

func TestThinkingSourceSearchesUnclassifiedArchiveRows(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "archived", "project", func(s *Session) { s.UserMessageCount = 2 })
	insertMessages(t, d, Message{SessionID: "archived", Role: "assistant", Content: "[Thinking]\narchivedneedle\n[/Thinking]\nAnswer", ThinkingText: "archivedneedle"})
	for _, mode := range []string{"substring", "regex"} {
		explicit, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "archivedneedle", Mode: mode, Sources: []string{"thinking"}})
		require.NoError(t, err)
		require.Len(t, explicit.Matches, 1)
		assert.Equal(t, "thinking", explicit.Matches[0].Location)
		all, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "archivedneedle", Mode: mode})
		require.NoError(t, err)
		require.Len(t, all.Matches, 1)
		assert.Equal(t, "message", all.Matches[0].Location)
	}
	for _, mode := range []string{"terms", "semantic", "hybrid"} {
		_, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "archivedneedle", Mode: mode, Sources: []string{"thinking"}})
		var inputErr *SearchInputError
		require.ErrorAs(t, err, &inputErr)
	}
}
