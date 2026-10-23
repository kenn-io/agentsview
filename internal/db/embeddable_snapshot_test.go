package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmbeddableSnapshotReservesArchiveWriter(t *testing.T) {
	archive := testDB(t)
	other, err := Open(t.Context(), archive.Path())
	require.NoError(t, err)
	defer other.Close()
	conn, err := other.getWriter().Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), "PRAGMA busy_timeout=1")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	err = archive.WithEmbeddableSnapshot(ctx, func(s EmbeddableSnapshot) error {
		_, err := s.ScanEmbeddableUnits(ctx, "", false, 4000, func(EmbeddableUnit) error { return nil })
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		require.Error(t, err, "a second archive handle must not write during installation")
		cancel()
		return ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	_, err = conn.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	require.NoError(t, err, "cancellation must release the reservation")
	_, err = conn.ExecContext(t.Context(), "ROLLBACK")
	require.NoError(t, err)
}
