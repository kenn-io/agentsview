//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestStoreSessionManagementCRUD(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	project := "session-mgmt"
	_, err = store.DB().Exec(`
		INSERT INTO sessions (
			id, machine, project, agent, first_message,
			started_at, ended_at, message_count,
			user_message_count
		) VALUES
			('mgmt-rename', 'machine', $1, 'claude', 'rename me',
			 '2026-03-12T10:00:00Z'::timestamptz,
			 '2026-03-12T10:30:00Z'::timestamptz, 2, 1),
			('mgmt-trash', 'machine', $1, 'claude', 'trash me',
			 '2026-03-12T11:00:00Z'::timestamptz,
			 '2026-03-12T11:30:00Z'::timestamptz, 2, 1),
			('mgmt-delete', 'machine', $1, 'claude', 'delete me',
			 '2026-03-12T12:00:00Z'::timestamptz,
			 '2026-03-12T12:30:00Z'::timestamptz, 2, 1),
			('mgmt-empty-a', 'machine', $1, 'claude', 'empty a',
			 '2026-03-12T13:00:00Z'::timestamptz,
			 '2026-03-12T13:30:00Z'::timestamptz, 2, 1),
			('mgmt-empty-b', 'machine', $1, 'claude', 'empty b',
			 '2026-03-12T14:00:00Z'::timestamptz,
			 '2026-03-12T14:30:00Z'::timestamptz, 2, 1)
	`, project)
	require.NoError(t, err, "inserting session rows")

	renamed := "Renamed by PG store"
	require.NoError(t, store.RenameSession("mgmt-rename", &renamed),
		"RenameSession")
	sess, err := store.GetSession(ctx, "mgmt-rename")
	require.NoError(t, err, "GetSession after rename")
	require.NotNil(t, sess)
	require.NotNil(t, sess.DisplayName)
	assert.Equal(t, renamed, *sess.DisplayName)

	require.NoError(t, store.SoftDeleteSession("mgmt-trash"),
		"SoftDeleteSession")
	trashed, err := store.ListTrashedSessions(ctx)
	require.NoError(t, err, "ListTrashedSessions after soft delete")
	assert.Contains(t, sessionIDs(trashed), "mgmt-trash")

	restored, err := store.RestoreSession("mgmt-trash")
	require.NoError(t, err, "RestoreSession")
	assert.EqualValues(t, 1, restored)
	sess, err = store.GetSession(ctx, "mgmt-trash")
	require.NoError(t, err, "GetSession after restore")
	require.NotNil(t, sess)

	require.NoError(t, store.SoftDeleteSession("mgmt-delete"),
		"SoftDeleteSession delete target")
	deleted, err := store.DeleteSessionIfTrashed("mgmt-delete")
	require.NoError(t, err, "DeleteSessionIfTrashed")
	assert.EqualValues(t, 1, deleted)
	sess, err = store.GetSessionFull(ctx, "mgmt-delete")
	require.NoError(t, err, "GetSessionFull after delete")
	assert.Nil(t, sess)

	deletedCount, err := store.SoftDeleteSessions([]string{
		"mgmt-empty-a", "mgmt-empty-b",
	})
	require.NoError(t, err, "SoftDeleteSessions")
	assert.Equal(t, 2, deletedCount)

	count, err := store.EmptyTrash()
	require.NoError(t, err, "EmptyTrash")
	assert.Equal(t, 2, count)
	trashed, err = store.ListTrashedSessions(ctx)
	require.NoError(t, err, "ListTrashedSessions after empty trash")
	assert.NotContains(t, sessionIDs(trashed), "mgmt-empty-a")
	assert.NotContains(t, sessionIDs(trashed), "mgmt-empty-b")

	assert.Equal(t, db.ErrReadOnly, store.UpsertSession(db.Session{}))
	assert.Equal(t, db.ErrReadOnly,
		store.ReplaceSessionMessages("mgmt-rename", nil))
	_, err = store.WriteSessionBatchAtomic(nil)
	assert.ErrorIs(t, err, db.ErrReadOnly)
}
