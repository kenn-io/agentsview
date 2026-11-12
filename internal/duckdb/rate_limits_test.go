//go:build !(windows && arm64)

package duckdb

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestRateLimitSnapshotsMirrorToDuckDB(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	dbtest.SeedRateLimitSnapshots(t, local)

	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(t, err, "push to DuckDB")
	// A second push sends nothing new and must not duplicate rows.
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err, "repeat push to DuckDB")
	dbtest.AssertLatestRateLimits(t, NewStoreFromDB(syncer.DB()))
	var rows int
	require.NoError(t, syncer.DB().QueryRowContext(ctx, `SELECT count(*) FROM rate_limit_snapshots`).Scan(&rows))
	require.Equal(t, 4, rows)
}
