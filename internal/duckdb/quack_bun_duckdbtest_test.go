//go:build duckdbtest && !(windows && arm64)

package duckdb

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

func TestQuackBunScansQuotedPredicateJSONAndTimestampAfterReattach(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "quack-bun.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-bun"

	server := openQuackMirrorServer(t, ctx, path, uri, token)
	_, err := server.ExecContext(ctx, `
		CREATE TABLE messages (
			session_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			content TEXT NOT NULL,
			timestamp TIMESTAMP,
			token_usage TEXT NOT NULL,
			has_thinking BOOLEAN NOT NULL,
			PRIMARY KEY (session_id, ordinal)
		)`)
	require.NoError(t, err)
	_, err = server.ExecContext(ctx, `
		INSERT INTO messages VALUES (?, ?, ?, ?, ?, ?)`,
		"session-1", 3, "O'Reilly", time.Date(2026, 8, 2, 16, 30, 0, 0, time.UTC),
		`{"output_tokens":7}`, true,
	)
	require.NoError(t, err)

	store, err := NewQuackStore(uri, token, false, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	read := func() bunmodel.Message {
		var got bunmodel.Message
		err := store.viewBun(ctx, func(q bun.IDB) error {
			return q.NewSelect().Model(&got).
				Column("session_id", "ordinal", "content", "timestamp", "token_usage", "has_thinking").
				Where("content = ?", "O'Reilly").Scan(ctx)
		})
		require.NoError(t, err)
		return got
	}

	got := read()
	assert.Equal(t, "session-1", got.SessionID)
	assert.True(t, got.HasThinking)
	require.NotNil(t, got.Timestamp)
	assert.Equal(t, time.Date(2026, 8, 2, 16, 30, 0, 0, time.UTC), got.Timestamp.Time)
	assert.JSONEq(t, `{"output_tokens":7}`, string(got.TokenUsage))

	_, err = server.ExecContext(ctx, `CALL quack_stop(?)`, uri)
	require.NoError(t, err)
	_, err = server.ExecContext(ctx, `CALL quack_serve(?, token => ?)`, uri, token)
	require.NoError(t, err)
	assert.Equal(t, "session-1", read().SessionID)
}
