package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

func TestBunWritesStayBarredAfterPoolReopen(t *testing.T) {
	write := func(store bun.IDB) error {
		_, err := store.NewRaw(
			"UPDATE sessions SET project = ? WHERE id = ?", "after", "barrier-session",
		).Exec(context.Background())
		return err
	}
	for _, scenario := range []struct {
		name string
		run  func(*DB) error
	}{
		{"transaction", func(database *DB) error {
			tx, err := database.beginBunWriteTx(t.Context())
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := write(tx); err != nil {
				return err
			}
			return tx.Commit()
		}},
		{"connection", func(database *DB) error {
			conn, err := database.acquireBunWriteConn(t.Context())
			if err != nil {
				return err
			}
			defer conn.Close()
			return write(conn)
		}},
		{"backend", func(database *DB) error {
			return (&sqliteBunBackend{store: database}).Update(t.Context(), write)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			database := testDB(t)
			insertSession(t, database, "barrier-session", "before")
			require.NoError(t, database.CloseWriter())
			database.mu.Lock()
			err := database.reopenLockedWithBarrier(true)
			database.mu.Unlock()
			require.NoError(t, err)

			require.ErrorIs(t, scenario.run(database), ErrWriterClosed)
			session, err := database.GetSession(t.Context(), "barrier-session")
			require.NoError(t, err)
			require.Equal(t, "before", session.Project)

			require.NoError(t, database.Reopen())
			require.NoError(t, scenario.run(database))
			session, err = database.GetSession(t.Context(), "barrier-session")
			require.NoError(t, err)
			require.Equal(t, "after", session.Project)
		})
	}
}

func TestMetadataCopyStaysBarredAfterPoolReopen(t *testing.T) {
	source, destination := testDB(t), testDB(t)
	for _, database := range []*DB{source, destination} {
		insertSession(t, database, "metadata-session", "project")
	}
	require.NoError(t, source.RenameSession("metadata-session", Ptr("copied name")))
	require.NoError(t, destination.RenameSession("metadata-session", Ptr("original name")))
	require.NoError(t, source.CloseWriter())
	require.NoError(t, destination.CloseWriter())
	destination.mu.Lock()
	err := destination.reopenLockedWithBarrier(true)
	destination.mu.Unlock()
	require.NoError(t, err)

	copyErr := destination.CopySessionMetadataFrom(source.Path())
	session, err := destination.GetSession(t.Context(), "metadata-session")
	require.NoError(t, err)
	require.NotNil(t, session.DisplayName)
	require.Equal(t, "original name", *session.DisplayName)
	require.ErrorIs(t, copyErr, ErrWriterClosed)

	require.NoError(t, destination.Reopen())
	require.NoError(t, destination.CopySessionMetadataFrom(source.Path()))
	session, err = destination.GetSession(t.Context(), "metadata-session")
	require.NoError(t, err)
	require.NotNil(t, session.DisplayName)
	require.Equal(t, "copied name", *session.DisplayName)
}
