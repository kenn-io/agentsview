//go:build pgtest

package postgres

import (
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

// userBlocksFetchedSQL sums the buffer requests, hits and reads, that the
// current transaction has made against user tables, indexes, and their TOAST
// relations. Relations with OIDs below FirstNormalObjectId (16384) are the
// system catalogs. The counters are this backend's pending statistics, which
// PostgreSQL flushes only between transactions.
const userBlocksFetchedSQL = `SELECT coalesce(sum(pg_stat_get_xact_blocks_fetched(oid)), 0) FROM pg_class WHERE oid >= 16384`

// explainUserBlocks runs EXPLAIN ANALYZE for query in tx and returns its plan
// and the number of user table and index pages the statement fetched.
//
// Bound a query's reads with this count, not EXPLAIN's buffer totals. Those
// totals include system catalog pages. Concurrent DDL anywhere on the server
// can overflow the shared invalidation queue and reset this backend's catalog
// caches. When execution handles such a reset, it rebuilds the cache entry of
// every open relation, and EXPLAIN charges those catalog pages to whichever
// plan node was running: a few pages, or hundreds, for the same plan and data.
//
// The count includes planning. After a reset, the planner rereads one metapage
// per btree index on each table in the query, so allow that many pages of
// slack in any bound.
func explainUserBlocks(t *testing.T, tx *sql.Tx, query string, args ...any) (map[string]any, float64) {
	t.Helper()
	var before, after float64
	require.NoError(t, tx.QueryRowContext(t.Context(), userBlocksFetchedSQL).Scan(&before))
	var encoded []byte
	require.NoError(t, tx.QueryRowContext(t.Context(), `EXPLAIN (ANALYZE, FORMAT JSON) `+query, args...).Scan(&encoded))
	require.NoError(t, tx.QueryRowContext(t.Context(), userBlocksFetchedSQL).Scan(&after))
	var plans []struct{ Plan map[string]any }
	require.NoError(t, json.Unmarshal(encoded, &plans))
	require.Len(t, plans, 1)
	return plans[0].Plan, after - before
}
