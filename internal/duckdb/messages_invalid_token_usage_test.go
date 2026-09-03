//go:build !(windows && arm64)

package duckdb

import (
	"context"
	json "encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// A DuckDB mirror built before the write-side guard existed still holds
// whatever malformed token_usage blob was pushed into it, and duckdb serve
// reaches the same response encoder that panicked on SQLite:
//
//	json: cannot marshal from Go jsontext.Value: unexpected EOF within
//	"/messages/0/token_usage"
//
// The mirror is corrupted directly here because the SQLite write path now
// sanitizes the value before it could ever be pushed.
func TestDuckGetMessagesDropsInvalidStoredTokenUsage(t *testing.T) {
	ctx := context.Background()
	store := newDuckWindowStore(t, func(local *db.DB) {
		seedDuckWindowMessages(t, local, "sInvalidUsage")
	})

	_, err := store.DB().ExecContext(ctx,
		`UPDATE messages SET token_usage = ?
		 WHERE session_id = ? AND ordinal = ?`,
		`{"input_tokens":4,"cache_read_input_tokens":123`, "sInvalidUsage", 0,
	)
	require.NoError(t, err)

	msgs, err := store.GetMessages(ctx, "sInvalidUsage", 0, 10, true)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	assert.Empty(t, string(msgs[0].TokenUsage),
		"invalid token_usage must not reach the caller")

	// The real regression: the row must be marshalable.
	_, err = json.Marshal(struct {
		Messages []db.Message `json:"messages"`
	}{Messages: msgs})
	require.NoError(t, err)

	// Everything else on the row survives.
	assert.Equal(t, 0, msgs[0].Ordinal)
	assert.NotEmpty(t, msgs[0].Content)
}

// Valid usage must still round-trip through the mirror untouched.
func TestDuckGetMessagesPreservesValidStoredTokenUsage(t *testing.T) {
	ctx := context.Background()
	store := newDuckWindowStore(t, func(local *db.DB) {
		seedDuckWindowMessages(t, local, "sValidUsage")
	})

	_, err := store.DB().ExecContext(ctx,
		`UPDATE messages SET token_usage = ?
		 WHERE session_id = ? AND ordinal = ?`,
		`{"input_tokens":7}`, "sValidUsage", 0,
	)
	require.NoError(t, err)

	msgs, err := store.GetMessages(ctx, "sValidUsage", 0, 10, true)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	assert.JSONEq(t, `{"input_tokens":7}`, string(msgs[0].TokenUsage))
}
