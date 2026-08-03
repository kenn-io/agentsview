package db

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSoftDeleteSessionsReturnsZeroWhenTransactionRollsBack(t *testing.T) {
	database := testDB(t)
	ids := make([]string, 401)
	for index := range ids {
		ids[index] = fmt.Sprintf("rollback-soft-delete-%03d", index)
		insertSession(t, database, ids[index], "rollback")
	}
	require.NoError(t, database.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			CREATE TRIGGER fail_second_soft_delete_batch
			BEFORE UPDATE OF deleted_at ON sessions
			WHEN NEW.id = 'rollback-soft-delete-400'
			BEGIN
				SELECT RAISE(ABORT, 'forced second batch failure');
			END`)
		return err
	}))

	count, err := database.SoftDeleteSessions(ids)

	require.Error(t, err)
	assert.Zero(t, count)
	first, readErr := database.GetSessionFull(t.Context(), ids[0])
	require.NoError(t, readErr)
	require.NotNil(t, first)
	assert.Nil(t, first.DeletedAt)
}

func TestRestoreSessionReturnsZeroWhenTransactionRollsBack(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "rollback-restore", "rollback")
	require.NoError(t, database.SoftDeleteSession("rollback-restore"))
	require.NoError(t, database.Update(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO local_session_source_baselines
				(session_id, machine, agent, file_path)
			VALUES ('rollback-restore', 'host', 'codex', '/tmp/rollback')`); err != nil {
			return err
		}
		_, err := tx.Exec(`
			CREATE TRIGGER fail_restore_baseline_delete
			BEFORE DELETE ON local_session_source_baselines
			BEGIN
				SELECT RAISE(ABORT, 'forced restore failure');
			END`)
		return err
	}))

	restored, err := database.RestoreSession("rollback-restore")

	require.Error(t, err)
	assert.Zero(t, restored)
	session, readErr := database.GetSessionFull(t.Context(), "rollback-restore")
	require.NoError(t, readErr)
	require.NotNil(t, session)
	assert.NotNil(t, session.DeletedAt)
}

func TestDeleteSessionIfTrashedReturnsZeroWhenCommitFails(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "rollback-delete", "rollback")
	require.NoError(t, database.SoftDeleteSession("rollback-delete"))
	installDeferredSessionDeleteFailure(t, database)

	deleted, err := database.DeleteSessionIfTrashed("rollback-delete")

	require.Error(t, err)
	assert.Zero(t, deleted)
	session, readErr := database.GetSessionFull(t.Context(), "rollback-delete")
	require.NoError(t, readErr)
	require.NotNil(t, session)
	assert.NotNil(t, session.DeletedAt)
	assert.False(t, database.IsSessionExcluded("rollback-delete"))
}

func TestEmptyTrashReturnsZeroWhenCommitFails(t *testing.T) {
	database := testDB(t)
	for _, id := range []string{"rollback-empty-a", "rollback-empty-b"} {
		insertSession(t, database, id, "rollback")
		require.NoError(t, database.SoftDeleteSession(id))
	}
	installDeferredSessionDeleteFailure(t, database)

	deleted, err := database.EmptyTrash()

	require.Error(t, err)
	assert.Zero(t, deleted)
	for _, id := range []string{"rollback-empty-a", "rollback-empty-b"} {
		session, readErr := database.GetSessionFull(t.Context(), id)
		require.NoError(t, readErr)
		require.NotNil(t, session)
		assert.NotNil(t, session.DeletedAt)
		assert.False(t, database.IsSessionExcluded(id))
	}
}

func TestDeleteSessionIfTrashedPreDeletesSQLiteFTSContent(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "fts-delete", "rollback")
	insertMessages(t, database, asstMsg("fts-delete", 0, "large searchable transcript"))
	require.NoError(t, database.SoftDeleteSession("fts-delete"))
	require.NoError(t, database.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			CREATE TRIGGER require_bulk_fts_delete
			BEFORE DELETE ON messages
			WHEN EXISTS (
				SELECT 1 FROM sqlite_schema
				WHERE type = 'trigger' AND name = 'messages_ad'
			)
			BEGIN
				SELECT RAISE(ABORT, 'messages_ad still active');
			END`)
		return err
	}))

	deleted, err := database.DeleteSessionIfTrashed("fts-delete")

	require.NoError(t, err)
	assert.EqualValues(t, 1, deleted)
	session, readErr := database.GetSessionFull(t.Context(), "fts-delete")
	require.NoError(t, readErr)
	assert.Nil(t, session)
}

func installDeferredSessionDeleteFailure(t *testing.T, database *DB) {
	t.Helper()
	require.NoError(t, database.Update(func(tx *sql.Tx) error {
		for _, statement := range []string{
			`CREATE TABLE mutation_commit_parent (id INTEGER PRIMARY KEY)`,
			`CREATE TABLE mutation_commit_child (
				parent_id INTEGER NOT NULL,
				FOREIGN KEY (parent_id) REFERENCES mutation_commit_parent(id)
					DEFERRABLE INITIALLY DEFERRED
			)`,
			`CREATE TRIGGER fail_session_delete_commit
			 AFTER DELETE ON sessions
			 BEGIN
				 INSERT INTO mutation_commit_child(parent_id) VALUES (1);
			 END`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		return nil
	}))
}

func TestBunStoreSessionMutationsRejectBeforeBackendUpdate(t *testing.T) {
	backend := &recordingBunBackend{}
	store := NewBunStore(backend)
	name := "forbidden"

	require.ErrorIs(t, store.RenameSession("session", &name), ErrReadOnly)
	require.ErrorIs(t, store.SoftDeleteSession("session"), ErrReadOnly)
	count, err := store.SoftDeleteSessions([]string{"session"})
	assert.Zero(t, count)
	require.ErrorIs(t, err, ErrReadOnly)
	restored, err := store.RestoreSession("session")
	assert.Zero(t, restored)
	require.ErrorIs(t, err, ErrReadOnly)
	deleted, err := store.DeleteSessionIfTrashed("session")
	assert.Zero(t, deleted)
	require.ErrorIs(t, err, ErrReadOnly)
	emptied, err := store.EmptyTrash()
	assert.Zero(t, emptied)
	require.ErrorIs(t, err, ErrReadOnly)

	assert.Zero(t, backend.updateCalls)
}
