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

// Nothing validates token_usage on the way in, so a malformed blob can sit
// in the mirror. Without the guard this test pins, it would reach duckdb
// serve's response encoder and fail there, the way it did on SQLite before
// DecodeStoredTokenUsage:
//
//	json: cannot marshal from Go jsontext.Value: unexpected EOF within
//	"/messages/0/token_usage"
//
// The mirror is corrupted directly rather than through a push so the read
// path is exercised on its own, independent of what any parser emits.
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

// The bug that actually made every duckdb serve transcript blank in
// v0.42.0. No corruption is involved: token_usage is
// "TEXT NOT NULL DEFAULT ”", so nearly every row holds "". This scanner
// assigned it unconditionally, producing a NON-NIL, ZERO-LENGTH
// jsontext.Value, while the SQLite and PostgreSQL scanners guarded with
// `if tokenUsage != ""` and left the field nil.
//
// A zero-length jsontext.Value is not valid JSON and `omitempty` does not
// skip it, so marshaling the response failed for every message:
//
//	json: cannot marshal from Go jsontext.Value: unexpected EOF within
//	"/messages/0/token_usage"
//
// It was latent until b85eae5a (#1475) moved the field from
// json.RawMessage to jsontext.Value: v1's omitempty dropped a zero-length
// slice, v2's does not. Asserting on a pristine mirror is the point -- a
// test that plants corruption cannot catch this.
func TestDuckCleanMirrorMessagesMarshal(t *testing.T) {
	ctx := context.Background()
	store := newDuckWindowStore(t, func(local *db.DB) {
		seedDuckWindowMessages(t, local, "sCleanMirror")
	})

	var invalid int
	require.NoError(t, store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages
		 WHERE token_usage <> '' AND NOT json_valid(token_usage)`,
	).Scan(&invalid))
	require.Zero(t, invalid, "precondition: the mirror must be uncorrupted")

	msgs, err := store.GetMessages(ctx, "sCleanMirror", 0, 100, true)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)

	// An absent usage value must be nil, never a zero-length value.
	for _, m := range msgs {
		if len(m.TokenUsage) == 0 {
			assert.Nil(t, m.TokenUsage,
				"ordinal %d: empty token_usage must be nil, not zero-length",
				m.Ordinal)
		}
	}

	// What the API does when serving the transcript.
	_, err = json.Marshal(struct {
		Messages []db.Message `json:"messages"`
	}{Messages: msgs})
	require.NoError(t, err,
		"a clean mirror must marshal; this is what duckdb serve does per request")
}
