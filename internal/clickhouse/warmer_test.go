package clickhouse

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

// A warmer with no recent client read does not query the mirror. The
// store points at a port nothing listens on, so any query fails.
func TestUsageWarmerQueriesOnlyAfterClientReads(t *testing.T) {
	conn, err := sql.Open("clickhouse", "clickhouse://127.0.0.1:1/default?dial_timeout=1s")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	store := NewStoreFromDB(conn)

	require.NoError(t, store.warmUsageReads(t.Context()), "an idle warmer must not query")

	store.recordUsageRead("daily", db.UsageFilter{Timezone: "UTC"}, 0)
	require.Error(t, store.warmUsageReads(t.Context()), "a recent read must be warmed")
}

// An activity read over more than a month is not rebuilt after pushes;
// a month's read is.
func TestUsageWarmerSkipsLongActivityReads(t *testing.T) {
	conn, err := sql.Open("clickhouse", "clickhouse://127.0.0.1:1/default?dial_timeout=1s")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	store := NewStoreFromDB(conn)
	now := time.Now()
	resolve := func(from, to time.Time) activity.Query {
		q, err := activity.ResolveQuery(activity.QueryInput{
			Preset: "custom", From: from.Format(time.RFC3339), To: to.Format(time.RFC3339), Timezone: "UTC",
		}, now)
		require.NoError(t, err)
		return q
	}

	store.recordActivityRead(db.AnalyticsFilter{Timezone: "UTC"}, resolve(now.AddDate(0, -3, 0), now))
	require.NoError(t, store.warmUsageReads(t.Context()), "a long read must not be warmed")

	store.recordActivityRead(db.AnalyticsFilter{Timezone: "UTC"}, resolve(now.AddDate(0, 0, -30), now))
	require.Error(t, store.warmUsageReads(t.Context()), "a month's read must be warmed")
}
