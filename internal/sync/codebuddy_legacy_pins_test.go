package sync

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// cbcHiddenUsageRows returns the session's hidden assistant rows that carry
// only usage and no text.
func cbcHiddenUsageRows(t *testing.T, msgs []db.Message) []db.Message {
	t.Helper()
	var rows []db.Message
	for _, m := range msgs {
		if m.Role == "assistant" && m.IsSystem && m.Content == "" {
			rows = append(rows, m)
		}
	}
	return rows
}

// A pinned reply whose request total came from its 4.x folder keeps its pin,
// note, text, and total after the folder is deleted, through a sync, a
// forced parse, and a rebuild.
func TestSyncCodeBuddyConvertedKeepsPinAfterFolderDeleted(t *testing.T) {
	database := openTestDB(t)
	convDir, _, engine, _ := cbcConvertedImageSync(t, database, map[string]int{"a1": 40})
	require.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens)

	const sessionID = "codebuddy:conv1"
	msgs, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	var reply db.Message
	for _, m := range msgs {
		if m.Role == "assistant" && !m.IsSystem && m.Content != "" {
			reply = m
		}
	}
	require.Equal(t, "a pixel", reply.Content)
	require.NotEmpty(t, reply.SourceUUID, "the reply carries its 4.x request identity")
	require.Empty(t, cbcHiddenUsageRows(t, msgs))
	note := "keep this answer"
	_, err = database.PinMessage(t.Context(), sessionID, reply.ID, &note)
	require.NoError(t, err)

	require.NoError(t, os.RemoveAll(convDir))
	for _, step := range []struct {
		name string
		run  func()
	}{
		{"sync", func() { engine.SyncAll(t.Context(), nil) }},
		{"forced parse", func() { engine.SyncAllForceParse(t.Context(), nil) }},
		{"rebuild", func() {
			st := engine.ResyncAll(t.Context(), nil)
			require.False(t, st.Aborted, "%v", st.Warnings)
		}},
	} {
		step.run()
		pins, err := database.ListPinnedMessages(t.Context(), sessionID, "")
		require.NoError(t, err)
		require.Len(t, pins, 1, "%s keeps the pin", step.name)
		require.NotNil(t, pins[0].Note, step.name)
		assert.Equal(t, note, *pins[0].Note, "%s keeps the note", step.name)
		msgs, err := database.GetAllMessages(t.Context(), sessionID)
		require.NoError(t, err)
		idx := -1
		for i, m := range msgs {
			if m.ID == pins[0].MessageID {
				idx = i
			}
		}
		require.GreaterOrEqual(t, idx, 0, "%s: the pin points at a stored message", step.name)
		pinned := msgs[idx]
		assert.Equal(t, reply.Content, pinned.Content, "%s: the pin stays on the reply text", step.name)
		assert.False(t, pinned.IsSystem, step.name)
		assert.Equal(t, reply.SourceUUID, pinned.SourceUUID, "%s keeps the reply identity", step.name)
		assert.Empty(t, cbcHiddenUsageRows(t, msgs), "%s adds no hidden usage row", step.name)
		assert.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens,
			"%s keeps the archived 4.x total", step.name)
		sess, err := database.GetSessionFull(t.Context(), sessionID)
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Equal(t, 40, sess.TotalOutputTokens, step.name)
	}
}

// When the reply that carried a 4.x total no longer matches after the folder
// is deleted, the total stays as a hidden usage row.
func TestSyncCodeBuddyConvertedKeepsTotalWhenReplyChanged(t *testing.T) {
	database := openTestDB(t)
	convDir, transcript, engine, _ := cbcConvertedImageSync(t, database, map[string]int{"a1": 40})
	require.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens)

	require.NoError(t, os.RemoveAll(convDir))
	turn := append([]string(nil), cbcConvertedImageTurn[:2]...)
	turn = append(turn,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225603000,"content":[{"type":"output_text","text":"a pixel, retimed"}]}`)
	writeCBCSync(t, transcript, turn...)
	engine.SyncAll(t.Context(), nil)

	assert.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens,
		"the archived 4.x total stays")
	msgs, err := database.GetAllMessages(t.Context(), "codebuddy:conv1")
	require.NoError(t, err)
	hidden := cbcHiddenUsageRows(t, msgs)
	require.Len(t, hidden, 1, "the total stays as a hidden usage row")
	assert.Equal(t, 40, hidden[0].OutputTokens)
	for _, m := range msgs {
		if m.Content == "a pixel, retimed" {
			assert.Empty(t, m.SourceUUID, "the changed reply does not take the 4.x identity")
		}
	}
}
