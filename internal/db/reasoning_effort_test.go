package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReasoningEffortMessageRoundTripAndBatchBoundary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "effort-session", "proj")

	messages := make([]Message, 39)
	for i := range messages {
		messages[i] = Message{
			SessionID:       "effort-session",
			Ordinal:         i,
			Role:            "assistant",
			Content:         "neutral fixture",
			ContentLength:   15,
			Model:           "model-test",
			ReasoningEffort: "",
		}
	}
	messages[38].ReasoningEffort = "high"
	require.NoError(d.InsertMessages(messages))

	got, err := d.GetAllMessages(t.Context(), "effort-session")
	require.NoError(err)
	require.Len(got, 39)
	assert.Empty(got[0].ReasoningEffort)
	assert.Equal("high", got[38].ReasoningEffort)

	byOrdinal, err := d.GetMessageByOrdinal("effort-session", 38)
	require.NoError(err)
	require.NotNil(byOrdinal)
	assert.Equal("high", byOrdinal.ReasoningEffort)
}

func TestReasoningEffortMigrationDefaultsLegacyRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "legacy.db")
	d, err := Open(path)
	require.NoError(err)
	insertSession(t, d, "legacy-effort", "proj")
	insertMessages(t, d, Message{
		SessionID:       "legacy-effort",
		Ordinal:         0,
		Role:            "assistant",
		Content:         "legacy row",
		Model:           "model-test",
		ReasoningEffort: "high",
	})
	require.NoError(d.Close())

	conn, err := sql.Open("sqlite3", path)
	require.NoError(err)
	_, err = conn.ExecContext(t.Context(), `ALTER TABLE messages DROP COLUMN reasoning_effort`)
	require.NoError(err)
	require.NoError(conn.Close())

	migrated, err := Open(path)
	require.NoError(err)

	var columnCount int
	require.NoError(migrated.getReader().QueryRow(
		`SELECT count(*) FROM pragma_table_info('messages')
		 WHERE name = 'reasoning_effort'`,
	).Scan(&columnCount))
	assert.Equal(1, columnCount)

	messages, err := migrated.GetAllMessages(
		t.Context(), "legacy-effort",
	)
	require.NoError(err)
	require.Len(messages, 1)
	assert.Empty(messages[0].ReasoningEffort)

	require.NoError(migrated.Close())
	reopened, err := Open(path)
	require.NoError(err)
	require.NoError(reopened.Close())
}

func TestReasoningEffortChangesMessageFingerprint(t *testing.T) {
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "fingerprint-session", "proj")
	require.NoError(d.InsertMessages([]Message{{
		SessionID: "fingerprint-session",
		Ordinal:   0,
		Role:      "assistant",
		Content:   "neutral fixture",
		Model:     "model-test",
	}}))

	before, err := d.MessageTokenFingerprint("fingerprint-session")
	require.NoError(err)
	_, err = d.getWriter().Exec("UPDATE messages SET reasoning_effort = ? WHERE session_id = ?", "medium", "fingerprint-session")
	require.NoError(err)
	after, err := d.MessageTokenFingerprint("fingerprint-session")
	require.NoError(err)
	assert.NotEqual(t, before, after)
}
