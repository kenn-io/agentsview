package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReasoningEffortMessageRoundTripAndBatchBoundary(t *testing.T) {
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
	require.NoError(t, d.InsertMessages(t.Context(), messages))

	got, err := d.GetAllMessages(t.Context(), "effort-session")
	require.NoError(t, err)
	require.Len(t, got, 39)
	assert.Empty(t, got[0].ReasoningEffort)
	assert.Equal(t, "high", got[38].ReasoningEffort)

	byOrdinal, err := d.GetMessageByOrdinal(t.Context(), "effort-session", 38)
	require.NoError(t, err)
	require.NotNil(t, byOrdinal)
	assert.Equal(t, "high", byOrdinal.ReasoningEffort)
}

func TestReasoningEffortMigrationDefaultsLegacyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	d, err := Open(t.Context(), path)
	require.NoError(t, err)
	insertSession(t, d, "legacy-effort", "proj")
	insertMessages(t, d, Message{
		SessionID:       "legacy-effort",
		Ordinal:         0,
		Role:            "assistant",
		Content:         "legacy row",
		Model:           "model-test",
		ReasoningEffort: "high",
	})
	require.NoError(t, d.Close())

	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `ALTER TABLE messages DROP COLUMN reasoning_effort`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	migrated, err := Open(t.Context(), path)
	require.NoError(t, err)

	var columnCount int
	require.NoError(t, migrated.getReader().QueryRow(t.Context(),
		`SELECT count(*) FROM pragma_table_info('messages')
		 WHERE name = 'reasoning_effort'`,
	).Scan(&columnCount))
	assert.Equal(t, 1, columnCount)

	messages, err := migrated.GetAllMessages(
		t.Context(), "legacy-effort",
	)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Empty(t, messages[0].ReasoningEffort)

	require.NoError(t, migrated.Close())
	reopened, err := Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}

func TestReasoningEffortChangesMessageFingerprint(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "fingerprint-session", "proj")
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{
		SessionID: "fingerprint-session",
		Ordinal:   0,
		Role:      "assistant",
		Content:   "neutral fixture",
		Model:     "model-test",
	}}))

	before, err := d.MessageTokenFingerprint(t.Context(), "fingerprint-session")
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), "UPDATE messages SET reasoning_effort = ? WHERE session_id = ?", "medium", "fingerprint-session")
	require.NoError(t, err)
	after, err := d.MessageTokenFingerprint(t.Context(), "fingerprint-session")
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
}
