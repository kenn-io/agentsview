//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestRateLimitSnapshotsPushToClickHouse(t *testing.T) {
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	dbtest.SeedRateLimitSnapshots(t, local)
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err, "push to ClickHouse")

	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertLatestRateLimits(t, store)
}

// A rebuilt archive renumbers its rows below the stored high-water id and
// forces a full push, which must send every report again.
func TestRateLimitSnapshotsFullPushReplaysReports(t *testing.T) {
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	dbtest.SeedRateLimitSnapshots(t, local)
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err, "first push")
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.DB().ExecContext(ctx, "TRUNCATE TABLE rate_limit_snapshots")
	require.NoError(t, err)

	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err, "full push")
	dbtest.AssertLatestRateLimits(t, store)
}
