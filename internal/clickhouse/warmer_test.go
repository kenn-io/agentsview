package clickhouse

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

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
