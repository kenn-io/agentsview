package server_test

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A malformed token_usage blob used to take the whole endpoint down.
// jsontext.Value defers parsing to marshal time, so the bad value survived
// the query and detonated in the response encoder. internal/server
// installs no recover(), so net/http closed the connection without a
// response -- ERR_EMPTY_RESPONSE in the browser, a blank transcript in the
// UI, while exporting the same session to HTML still rendered its content
// because the export path never marshals this field.
func TestGetMessages_InvalidStoredTokenUsageDoesNotBreakResponse(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 3)
	te.seedMessages(t, "s1", 3)

	// Write the malformed value straight to the row: nothing validates
	// token_usage on the way in, so this is a state the DB can hold.
	corruptStoredTokenUsage(t, filepath.Join(te.dataDir, "test.db"), "s1", 0,
		`{"input_tokens":4,"cache_read_input_tokens":123`)

	w := te.get(t, "/api/v1/sessions/s1/messages")

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decode[messageListResponse](t, w)
	assert.Len(t, resp.Messages, 3,
		"every message must still be served; only the bad metadata is dropped")
}

func corruptStoredTokenUsage(
	t *testing.T, dbPath, sessionID string, ordinal int, raw string,
) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()

	res, err := conn.Exec(
		`UPDATE messages SET token_usage = ? WHERE session_id = ? AND ordinal = ?`,
		raw, sessionID, ordinal,
	)
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "expected to corrupt exactly one row")
}
