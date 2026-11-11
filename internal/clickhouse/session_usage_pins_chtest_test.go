//go:build chtest

package clickhouse

import (
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

// TestClickHouseSessionUsagePins pins the mirror's session usage responses
// for sessions with and without usage. One store serves both reads of the
// growing session, so usage pushed after its first read must reach the
// second.
func TestClickHouseSessionUsagePins(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	dbtest.SeedSessionUsagePins(t, local)
	dsn, database := chtest.FreshDatabase(t)
	syncer := newTestSync(t, local, Target{URL: dsn, Database: database}, storage.PusherOptions{})
	push := func() {
		t.Helper()
		_, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		require.NoError(t, syncer.syncModelPricing(ctx))
	}
	push()
	store, err := NewStore(ctx, Target{URL: dsn, Database: database})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	dbtest.AssertSessionUsagePins(t, store)
	dbtest.GrowSessionUsagePin(t, local)
	push()
	dbtest.AssertGrownSessionUsagePin(t, store)
}

// TestClickHouseSessionUsageWithoutUsageSkipsUsageQuery checks that a
// session with no usage rows is answered without the usage query, while a
// session whose usage comes only from usage events, which leaves the session
// row without token totals, still runs it.
func TestClickHouseSessionUsageWithoutUsageSkipsUsageQuery(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	dbtest.SeedSessionUsagePins(t, local)
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	conn := chtest.Open(t, dsn, database)

	for _, tc := range []struct {
		id   string
		want uint64
	}{{dbtest.UsagePinEmptyID, 0}, {dbtest.UsagePinEventsID, 1}} {
		comment := "session-usage-skip-" + tc.id + "-" + database
		readCtx := chdriver.Context(ctx, chdriver.WithSettings(chdriver.Settings{"log_comment": comment}))
		_, err := store.GetSessionUsage(readCtx, tc.id, false)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `SYSTEM FLUSH LOGS`)
		require.NoError(t, err)
		var queries uint64
		require.NoError(t, conn.QueryRowContext(ctx, `
			SELECT count() FROM system.query_log
			WHERE type = 'QueryFinish' AND log_comment = ?
				AND startsWith(trimLeft(query, ' \n\t\r'), 'WITH usage_raw AS')`, comment).Scan(&queries))
		assert.Equal(t, tc.want, queries, "usage queries for %s", tc.id)
	}
}
