//go:build !(windows && arm64)

package duckdb

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestDuckDBDialogueSearchParity(t *testing.T) {
	local := newLocalDB(t)
	dbtest.SeedDialogueSearch(t, local)
	path := filepath.Join(t.TempDir(), "dialogue.duckdb")
	_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store := openPushedStore(t, path)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertDialogueSearchParity(t, local, store, "substring", "regex", "fts")
}

func TestDialogueOnlyDuckDBChangeRepushesSession(t *testing.T) {
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	require.NoError(t, local.SetSessionDataVersion(ctx, "sess-1", 139))
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)

	// Ordinary sync writes the reparse, then stamps its parser version.
	// The stamp advances local_modified_at even for dialogue-only changes.
	messages, err := local.GetAllMessages(ctx, "sess-1")
	require.NoError(t, err)
	messages[1].DialogueText = new("answerneedle")
	require.NoError(t, local.ReplaceSessionMessages(ctx, "sess-1", messages))
	require.NoError(t, local.SetSessionDataVersion(ctx, "sess-1", db.CurrentDataVersion()))
	result, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, result.Diagnostics.Full)
	assert.Equal(t, 1, result.SessionsPushed)
	conn, err := Open(ctx, path)
	require.NoError(t, err)
	defer conn.Close()
	var got sql.NullString
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT dialogue_text FROM messages WHERE session_id = 'sess-1' AND ordinal = 1`).Scan(&got))
	assert.True(t, got.Valid)
	assert.Equal(t, "answerneedle", got.String)
}
