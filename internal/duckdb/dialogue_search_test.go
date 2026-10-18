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
	dbtest.AssertDialogueSearchParity(t, local, store, "", "substring", "regex", "fts")
}

func TestDialogueDuckDBRebuildsOldSchemaMirror(t *testing.T) {
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	messages, err := local.GetAllMessages(ctx, "sess-1")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	messages[1].Content = "answerneedle\n[Thinking] reasoningneedle\n[Bash] $ toolneedle"
	messages[1].ThinkingText = "reasoningneedle"
	messages[1].DialogueText = new("answerneedle")
	require.NoError(t, local.ReplaceSessionMessages(ctx, "sess-1", messages))
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)

	conn, err := Open(ctx, path)
	require.NoError(t, err)
	for _, index := range []string{"idx_messages_session_ordinal", "idx_messages_session_role", "idx_messages_timestamp"} {
		_, err = conn.ExecContext(ctx, "DROP INDEX "+index)
		require.NoError(t, err)
	}
	_, err = conn.ExecContext(ctx, "ALTER TABLE messages DROP COLUMN dialogue_text")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	result, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, result.Diagnostics.Full)
	assert.Contains(t, result.Diagnostics.RebuildReason, "dialogue_text")
	conn, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	var dialogue string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT dialogue_text FROM messages WHERE session_id = 'sess-1' AND ordinal = 1`).Scan(&dialogue))
	assert.Equal(t, "answerneedle", dialogue)
}

func TestDuckDBFingerprintTracksNullableDialogue(t *testing.T) {
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	syncer := newTestSync(t, path, local, storage.MirrorPushOptions{})
	session, err := local.GetSession(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, session)
	fingerprint := func() string {
		t.Helper()
		got, err := syncer.sessionFingerprints(ctx, []db.Session{*session})
		require.NoError(t, err)
		return got[session.ID]
	}
	setDialogue := func(ordinal int, dialogue *string) {
		t.Helper()
		require.NoError(t, local.Update(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE messages SET dialogue_text = ? WHERE session_id = 'sess-1' AND ordinal = ?`, dialogue, ordinal)
			return err
		}))
	}
	legacy := fingerprint()
	setDialogue(1, new("answerneedle"))
	first := fingerprint()
	assert.NotEqual(t, legacy, first)
	setDialogue(1, new(""))
	assert.NotEqual(t, legacy, fingerprint(), "empty dialogue differs from NULL")
	setDialogue(1, nil)
	setDialogue(0, new("answerneedle"))
	assert.NotEqual(t, first, fingerprint(), "dialogue belongs to its message")
	setDialogue(0, nil)
	assert.Equal(t, legacy, fingerprint())
}

func TestDialogueOnlyDuckDBChangeRepushesSession(t *testing.T) {
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)

	for _, dialogue := range []string{"answerneedle", "updatedneedle", ""} {
		t.Run(dialogue, func(t *testing.T) {
			// Keep every fingerprinted session scalar unchanged. Rewind only the
			// mirror cursor so this session is considered an incremental candidate.
			require.NoError(t, local.Update(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE messages SET dialogue_text = ? WHERE session_id = 'sess-1' AND ordinal = 1`, dialogue)
				return err
			}))
			setMirrorMetadataValue(t, path, lastPushCutoffMetadataKey, "2000-01-01T00:00:00.000Z")
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
			assert.Equal(t, dialogue, got.String)
		})
	}
}
