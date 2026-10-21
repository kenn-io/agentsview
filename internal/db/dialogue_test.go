package db

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
)

func TestDialogueFTSUpgradePreservesMessages(t *testing.T) {
	const total, needle = 21, 12
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	insertSession(t, d, "legacy", "project", func(s *Session) { s.Agent = "pi"; s.UserMessageCount = 2 })
	require.NoError(t, d.DropFTS(t.Context()))
	_, err := d.getWriter().Exec(t.Context(), fmt.Sprintf(`DROP VIEW message_search_content;
 ALTER TABLE messages DROP COLUMN dialogue_text;
 WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < %d)
 INSERT INTO messages (session_id, ordinal, role, content, content_length)
 SELECT 'legacy', i, CASE i %% 2 WHEN 0 THEN 'user' ELSE 'assistant' END,
  CASE i WHEN %d THEN 'answerneedle [Thinking] reasoningneedle' ELSE 'filler ' || i END, 0 FROM n;
 DELETE FROM stats WHERE key = 'message_search_corpus_version';
 CREATE VIRTUAL TABLE messages_fts USING fts5(content, content='messages', content_rowid='id', tokenize='porter unicode61');
 UPDATE messages SET thinking_text = 'reasoningneedle', has_thinking = 1 WHERE ordinal = %d;
 INSERT INTO messages_fts(messages_fts) VALUES('rebuild');
 PRAGMA user_version=%d; UPDATE sessions SET data_version=%d;`, total, needle, needle, CurrentDataVersion()-1, CurrentDataVersion()-1))
	require.NoError(t, err)
	var beforeID int64
	require.NoError(t, d.getReader().QueryRow(t.Context(),
		"SELECT id FROM messages WHERE session_id = 'legacy' AND ordinal = ?", needle).Scan(&beforeID))
	require.NoError(t, d.Close())

	var details []string
	d, err = OpenWithProgress(t.Context(), path, config.ArchiveContentFull, func(progress OpenProgress) {
		details = append(details, progress.Detail)
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	stored, err := d.GetAllMessages(t.Context(), "legacy")
	require.NoError(t, err)
	require.Len(t, stored, total)
	assert.Equal(t, "answerneedle [Thinking] reasoningneedle", stored[needle].Content)
	assert.Nil(t, stored[needle].DialogueText)
	assert.Equal(t, beforeID, stored[needle].ID)
	assert.Equal(t, "reasoningneedle", stored[needle].ThinkingText)
	for _, term := range []string{"answerneedle", "reasoningneedle"} {
		ordinary, err := d.Search(t.Context(), SearchFilter{Query: term})
		require.NoError(t, err)
		assert.Len(t, ordinary.Results, 1, term)
	}

	assert.Contains(t, details, "Updating full-text search")
}

func TestDialogueFTSCorpusVersion(t *testing.T) {
	for _, tc := range []struct {
		name, marker string
		rebuild      bool
	}{
		{"older", "0", true},
		{"current", searchCorpusVersion, false},
		{"newer", "10", false},
		{"invalid", "invalid", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			seedSearchSession(t, d, "version", "proj", nil)
			insertMessages(t, d, Message{SessionID: "version", Role: "user", Content: "needle"})
			_, err := d.getWriter().Exec(t.Context(), "UPDATE stats SET value = ? WHERE key = ?", tc.marker, searchCorpusVersionStatsKey)
			require.NoError(t, err)
			var details []string
			err = refreshDialogueFTS(t.Context(), d.getWriter(), func(progress OpenProgress) {
				details = append(details, progress.Detail)
			})
			require.NoError(t, err)
			var marker string
			require.NoError(t, d.getReader().QueryRow(t.Context(), "SELECT value FROM stats WHERE key = ?", searchCorpusVersionStatsKey).Scan(&marker))
			if tc.rebuild {
				assert.Contains(t, details, "Updating full-text search")
				assert.Equal(t, searchCorpusVersion, marker)
			} else {
				assert.Empty(t, details)
				assert.Equal(t, tc.marker, marker)
			}
		})
	}
}

func TestDropFTSInterruptedKeepsIndexConsistent(t *testing.T) {
	d := testDB(t)
	_, err := d.getWriter().Exec(t.Context(), `CREATE TRIGGER stats_fail BEFORE DELETE ON stats
 BEGIN SELECT RAISE(ABORT, 'interrupted'); END`)
	require.NoError(t, err)
	require.Error(t, d.DropFTS(t.Context()))

	var objects, marker int
	require.NoError(t, d.getReader().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('messages_fts', 'messages_ai', 'messages_ad', 'messages_au')`,
	).Scan(&objects))
	require.NoError(t, d.getReader().QueryRow(t.Context(),
		"SELECT COUNT(*) FROM stats WHERE key = ?", searchCorpusVersionStatsKey,
	).Scan(&marker))
	assert.Equal(t, 4, objects)
	assert.Equal(t, 1, marker)
}

func TestDialogueProjectionSeparatesMessagesAndThinking(t *testing.T) {
	for _, classified := range []bool{true, false} {
		t.Run(strconv.FormatBool(classified), func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "search", "project", func(s *Session) { s.UserMessageCount = 2 })
			message := Message{
				SessionID: "search", Ordinal: 1, Role: "assistant",
				Content:      "answerneedle [Thinking] reasoningneedle [/Thinking] [Read: file.txt]",
				ThinkingText: "reasoningneedle",
			}
			if classified {
				message.DialogueText = new("answerneedle")
			}
			insertMessages(t, d, Message{SessionID: "search", Role: "user", Content: "Question"}, message)
			for _, mode := range []string{"substring", "regex", "fts", "terms"} {
				for _, term := range []string{"answerneedle", "reasoningneedle", "file.txt"} {
					page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: term, Mode: mode, Sources: []string{"messages"}})
					require.NoError(t, err)
					if classified && term != "answerneedle" {
						assert.Empty(t, page.Matches, "%s %s", mode, term)
					} else {
						require.Len(t, page.Matches, 1, "%s %s", mode, term)
						assert.Equal(t, "message", page.Matches[0].Location)
					}
				}
			}
			if classified {
				for _, mode := range []string{"substring", "regex"} {
					for _, sources := range [][]string{nil, {"thinking"}} {
						page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "reasoningneedle", Mode: mode, Sources: sources})
						require.NoError(t, err)
						require.Len(t, page.Matches, 1)
						assert.Equal(t, "thinking", page.Matches[0].Location)
					}
				}
			}
		})
	}
	d := testDB(t)
	for _, mode := range []string{"terms", "semantic", "hybrid"} {
		_, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "reasoningneedle", Mode: mode, Sources: []string{"thinking"}})
		var inputErr *SearchInputError
		require.ErrorAs(t, err, &inputErr)
	}
}

func TestDialogueFTSTriggerSync(t *testing.T) {
	for _, tc := range []struct{ name, dialogue, hidden, replacement string }{
		{"porter", "answerneedle", "reasoningneedle", "replacementneedle"},
		{"cjk", "中文搜索", "工具输出", "秘密推理"},
		{"empty", "", "reasoningneedle", "replacementneedle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "cjk" && !simpleFTSRuntimeConfig.available() {
				t.Skip("simple FTS5 runtime is not installed for this test process")
			}
			d := testDB(t)
			insertSession(t, d, "search", "project", func(s *Session) { s.UserMessageCount = 2 })
			messages := []Message{
				{SessionID: "search", Ordinal: 0, Role: "user", Content: "Question"},
				{SessionID: "search", Ordinal: 1, Role: "assistant", Content: tc.dialogue + " [Thinking] " + tc.hidden, DialogueText: new(tc.dialogue)},
				{SessionID: "search", Ordinal: 2, Role: "assistant", Content: "Done"},
			}
			insertMessages(t, d, messages...)
			terms := []string{tc.hidden, tc.replacement}
			if tc.dialogue != "" {
				terms = append(terms, tc.dialogue)
			}
			for _, phase := range []string{"insert", "update", "delete"} {
				switch phase {
				case "update":
					messages[1].Content = tc.dialogue + " [Thinking] " + tc.replacement
					require.NoError(t, d.ReplaceSessionMessages(t.Context(), "search", messages))
				case "delete":
					require.NoError(t, d.ReplaceSessionMessages(t.Context(), "search", nil))
				}
				_, err := d.getWriter().Exec(t.Context(), "INSERT INTO messages_fts(messages_fts, rank) VALUES('integrity-check', 1)")
				require.NoError(t, err, phase)
				if tc.name == "cjk" {
					_, err := d.getWriter().Exec(t.Context(), "INSERT INTO messages_cjk_fts(messages_cjk_fts, rank) VALUES('integrity-check', 1)")
					require.NoError(t, err, phase)
				}
				for _, term := range terms {
					page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: term, Mode: "fts", Sources: []string{"messages"}})
					require.NoError(t, err, "%s %s", phase, term)
					if phase != "delete" && tc.dialogue != "" && term == tc.dialogue {
						require.Len(t, page.Matches, 1, "%s %s", phase, term)
						assert.Equal(t, tc.dialogue, page.Matches[0].Snippet, phase)
					} else {
						assert.Empty(t, page.Matches, "%s %s", phase, term)
					}
				}
				for _, term := range []string{"Question", tc.hidden, tc.replacement} {
					page, err := d.Search(t.Context(), SearchFilter{Query: term})
					require.NoError(t, err, "%s %s", phase, term)
					if phase != "delete" && (term == "Question" || phase == "insert" && term == tc.hidden || phase == "update" && term == tc.replacement) {
						require.Len(t, page.Results, 1, "%s %s", phase, term)
						assert.Equal(t, "search", page.Results[0].SessionID, "%s %s", phase, term)
					} else {
						assert.Empty(t, page.Results, "%s %s", phase, term)
					}
				}
			}
		})
	}
}

func TestProjectionOnlyReparsePreservesTranscriptFreshness(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "projection", "project")
	messages := []Message{{SessionID: "projection", Role: "assistant", Content: "Answer [Thinking] reasoning"}}
	insertMessages(t, d, messages...)
	_, err := d.getWriter().Exec(t.Context(), `UPDATE sessions SET
		secrets_rules_version = 'secrets-current', friction_rules_version = 'friction-current',
		local_modified_at = '2026-05-01T10:00:00Z' WHERE id = 'projection'`)
	require.NoError(t, err)
	storedBefore, err := d.GetAllMessages(t.Context(), "projection")
	require.NoError(t, err)
	require.Len(t, storedBefore, 1)
	before, err := d.GetSession(t.Context(), "projection")
	require.NoError(t, err)
	for _, dialogue := range []string{"Answer", ""} {
		messages[0].DialogueText = new(dialogue)
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), "projection", messages))
		stored, err := d.GetAllMessages(t.Context(), "projection")
		require.NoError(t, err)
		require.Len(t, stored, 1)
		assert.Equal(t, storedBefore[0].ID, stored[0].ID)
		assert.Equal(t, new(dialogue), stored[0].DialogueText)
		after, err := d.GetSession(t.Context(), "projection")
		require.NoError(t, err)
		assert.Equal(t, before.TranscriptRevision, after.TranscriptRevision)
		var secretVersion, frictionVersion, modified string
		require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT secrets_rules_version,
			friction_rules_version, local_modified_at FROM sessions WHERE id = 'projection'`).Scan(&secretVersion, &frictionVersion, &modified))
		assert.Equal(t, "secrets-current", secretVersion)
		assert.Equal(t, "friction-current", frictionVersion)
		assert.Equal(t, "2026-05-01T10:00:00Z", modified)
	}
}
