package db

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// corruptTokenUsage writes a malformed token_usage blob straight to the
// row. Nothing validates token_usage on the way in, so this is a state the
// database can genuinely hold; the read path is what has to cope with it.
func corruptTokenUsage(t *testing.T, d *DB, sessionID string, ordinal int, raw string) {
	t.Helper()
	_, err := d.getWriter().Exec(
		`UPDATE messages SET token_usage = ? WHERE session_id = ? AND ordinal = ?`,
		raw, sessionID, ordinal,
	)
	require.NoError(t, err)
}

func seedMessageWithUsage(t *testing.T, d *DB) {
	t.Helper()
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, Message{
		SessionID:     "s1",
		Ordinal:       0,
		Role:          "assistant",
		Content:       "hi",
		ContentLength: 2,
		Model:         "claude-opus-4",
		TokenUsage:    jsontext.Value(`{"input_tokens":50}`),
	})
}

// The read path must not hand an unmarshalable row to the API. Without a
// guard the invalid blob reaches the response encoder, which panics with
// "cannot marshal from Go jsontext.Value: unexpected EOF" and, since
// internal/server installs no recover(), drops the connection outright
// (ERR_EMPTY_RESPONSE in the browser) while the HTML export of the same
// session still renders.
func TestReadPathDropsInvalidTokenUsage(t *testing.T) {
	truncated := `{"input_tokens":4,"cache_read_input_tokens":123`

	for _, tc := range []struct {
		name string
		read func(t *testing.T, d *DB) []Message
	}{
		{
			name: "GetMessages",
			read: func(t *testing.T, d *DB) []Message {
				got, err := d.GetMessages(context.Background(), "s1", 0, 100, true)
				require.NoError(t, err)
				return got
			},
		},
		{
			name: "GetAllMessages",
			read: func(t *testing.T, d *DB) []Message {
				got, err := d.GetAllMessages(context.Background(), "s1")
				require.NoError(t, err)
				return got
			},
		},
		{
			name: "GetMessagesWindow",
			read: func(t *testing.T, d *DB) []Message {
				from := 0
				got, err := d.GetMessagesWindow(context.Background(), "s1",
					MessageWindow{Limit: 100, Asc: true, From: &from})
				require.NoError(t, err)
				return got
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			seedMessageWithUsage(t, d)
			corruptTokenUsage(t, d, "s1", 0, truncated)

			got := tc.read(t, d)
			require.Len(t, got, 1)
			assert.Empty(t, string(got[0].TokenUsage),
				"invalid token_usage must not reach the caller")

			// The real regression: the row must be marshalable.
			_, err := json.Marshal(struct {
				Messages []Message `json:"messages"`
			}{Messages: got})
			require.NoError(t, err)

			// Everything else on the row survives.
			assert.Equal(t, "hi", got[0].Content)
			assert.Equal(t, "claude-opus-4", got[0].Model)
		})
	}
}

// Valid usage must still round-trip untouched.
func TestReadPathPreservesValidTokenUsage(t *testing.T) {
	d := testDB(t)
	seedMessageWithUsage(t, d)

	got, err := d.GetAllMessages(context.Background(), "s1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.JSONEq(t, `{"input_tokens":50}`, string(got[0].TokenUsage))
}
