//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestRateLimitSnapshotsPushToPostgres(t *testing.T) {
	const schema = "agentsview_rate_limits_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	local := testDB(t)
	dbtest.SeedRateLimitSnapshots(t, local)
	syncer, err := New(
		t.Context(), pgURL, schema, local, "rate-limit-machine", true, storage.PusherOptions{},
	)
	require.NoError(t, err, "create PostgreSQL sync")
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err, "push rate limits")
	// A full push sends every row again; the identity index keeps one copy.
	_, err = syncer.Push(t.Context(), true, nil)
	require.NoError(t, err, "full push rate limits")

	remote, err := NewStore(t.Context(), pgURL, schema, true)
	require.NoError(t, err, "open PostgreSQL store")
	t.Cleanup(func() { require.NoError(t, remote.Close()) })
	dbtest.AssertLatestRateLimits(t, remote)
	var rows int
	require.NoError(t, remote.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM rate_limit_snapshots`).Scan(&rows))
	require.Equal(t, 4, rows)
}
