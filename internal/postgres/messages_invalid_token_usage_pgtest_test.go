//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	json "encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// Nothing validates token_usage on the way in, so a malformed blob can sit
// in the mirror. Without the guard this test pins, it would reach pg
// serve's response encoder and fail there, the way it did on SQLite before
// DecodeStoredTokenUsage:
//
//	json: cannot marshal from Go jsontext.Value: unexpected EOF within
//	"/messages/0/token_usage"
//
// The row is written directly rather than through a push so the read path
// is exercised on its own, independent of what any parser emits.
func TestPGGetMessagesDropsInvalidStoredTokenUsage(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	seedInvalidUsageRow(t, pg, "pg-invalid-usage",
		`{"input_tokens":4,"cache_read_input_tokens":123`)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	msgs, err := store.GetMessages(ctx, "pg-invalid-usage", 0, 10, true)
	require.NoError(t, err, "GetMessages")
	require.Len(t, msgs, 1)
	assert.Empty(t, string(msgs[0].TokenUsage),
		"invalid token_usage must not reach the caller")

	// The real regression: the row must be marshalable.
	_, err = json.Marshal(struct {
		Messages []db.Message `json:"messages"`
	}{Messages: msgs})
	require.NoError(t, err)

	// Everything else on the row survives.
	assert.Equal(t, "hello", msgs[0].Content)
	assert.Equal(t, "claude-sonnet-4-20250514", msgs[0].Model)
}

// Valid usage must still round-trip through the mirror untouched.
func TestPGGetMessagesPreservesValidStoredTokenUsage(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	seedInvalidUsageRow(t, pg, "pg-valid-usage", `{"input_tokens":7}`)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	msgs, err := store.GetMessages(context.Background(), "pg-valid-usage", 0, 10, true)
	require.NoError(t, err, "GetMessages")
	require.Len(t, msgs, 1)
	assert.JSONEq(t, `{"input_tokens":7}`, string(msgs[0].TokenUsage))
}

// seedInvalidUsageRow writes one session and one message carrying the given
// raw token_usage text, so the read path is what gets exercised.
func seedInvalidUsageRow(t *testing.T, pg *sql.DB, sessionID, rawUsage string) {
	t.Helper()
	_, err := pg.Exec(`
		INSERT INTO sessions (
			id, machine, project, agent, message_count, user_message_count
		) VALUES ($1, 'test-machine', 'test-project', 'claude', 1, 1)
		ON CONFLICT (id) DO UPDATE SET message_count = EXCLUDED.message_count
	`, sessionID)
	require.NoError(t, err, "insert session")

	_, err = pg.Exec(`
		INSERT INTO messages (
			session_id, ordinal, role, content,
			content_length, model, token_usage
		) VALUES ($1, 0, 'assistant', 'hello', 5,
			'claude-sonnet-4-20250514', $2)
		ON CONFLICT (session_id, ordinal) DO UPDATE SET
			token_usage = EXCLUDED.token_usage
	`, sessionID, rawUsage)
	require.NoError(t, err, "insert message")
}
