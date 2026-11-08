package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func storedDialogueColumn(t *testing.T, d *DB, sessionID string) []sql.NullString {
	t.Helper()
	rows, err := d.getReader().Query(t.Context(),
		"SELECT dialogue_text FROM messages WHERE session_id = ? ORDER BY ordinal", sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var out []sql.NullString
	for rows.Next() {
		var v sql.NullString
		require.NoError(t, rows.Scan(&v))
		out = append(out, v)
	}
	require.NoError(t, rows.Err())
	return out
}

func dialogueValues(t *testing.T, d *DB, sessionID string) []*string {
	t.Helper()
	msgs, err := d.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	out := make([]*string, len(msgs))
	for i, m := range msgs {
		out[i] = m.DialogueText
	}
	return out
}

// Messages covering each dialogue shape: a copy of content, an unclassified
// legacy row, a copy beside thinking text, and a projection that differs.
func dialogueStorageMessages(sessionID string) []Message {
	return []Message{
		{SessionID: sessionID, Ordinal: 0, Role: "user", Content: "plainneedle", DialogueText: new("plainneedle")},
		{SessionID: sessionID, Ordinal: 1, Role: "assistant", Content: "legacyneedle"},
		{SessionID: sessionID, Ordinal: 2, Role: "assistant", Content: "thinkcopy", DialogueText: new("thinkcopy"), ThinkingText: "why", HasThinking: true},
		{SessionID: sessionID, Ordinal: 3, Role: "assistant", Content: "said\n[Bash] $ true\ndone", DialogueText: new("said\ndone"), HasToolUse: true},
	}
}

func TestDuplicateDialogueStoredOnceAndRestoredOnRead(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s", "proj")
	insertMessages(t, d, dialogueStorageMessages("s")...)

	assert.Equal(t, []sql.NullString{
		{}, {}, {String: "thinkcopy", Valid: true}, {String: "said\ndone", Valid: true},
	}, storedDialogueColumn(t, d, "s"))
	assert.Equal(t, []*string{new("plainneedle"), nil, new("thinkcopy"), new("said\ndone")}, dialogueValues(t, d, "s"))

	single, err := d.GetMessageByOrdinal(t.Context(), "s", 0)
	require.NoError(t, err)
	require.NotNil(t, single)
	assert.Equal(t, new("plainneedle"), single.DialogueText)
}

func TestOpenMovesExistingDuplicateDialogue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	insertSession(t, d, "s", "proj")
	insertMessages(t, d, dialogueStorageMessages("s")...)
	// Rebuild the shape an older release wrote: the copy in dialogue_text and
	// no marker column.
	_, err := d.getWriter().Exec(t.Context(), `UPDATE messages SET dialogue_text = content WHERE dialogue_is_content = 1;
 ALTER TABLE messages DROP COLUMN dialogue_is_content;`)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	d, err = Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	assert.Equal(t, []sql.NullString{
		{}, {}, {String: "thinkcopy", Valid: true}, {String: "said\ndone", Valid: true},
	}, storedDialogueColumn(t, d, "s"))
	assert.Equal(t, []*string{new("plainneedle"), nil, new("thinkcopy"), new("said\ndone")}, dialogueValues(t, d, "s"))
	found, err := d.Search(t.Context(), SearchFilter{Query: "plainneedle"})
	require.NoError(t, err)
	assert.Len(t, found.Results, 1)

	// The full-text update trigger is back: a later edit reaches the index.
	_, err = d.getWriter().Exec(t.Context(),
		"UPDATE messages SET content = 'freshneedle', dialogue_text = 'freshneedle', dialogue_is_content = 0 WHERE session_id = 's' AND ordinal = 0")
	require.NoError(t, err)
	found, err = d.Search(t.Context(), SearchFilter{Query: "freshneedle"})
	require.NoError(t, err)
	assert.Len(t, found.Results, 1)
	found, err = d.Search(t.Context(), SearchFilter{Query: "plainneedle"})
	require.NoError(t, err)
	assert.Empty(t, found.Results)
}
