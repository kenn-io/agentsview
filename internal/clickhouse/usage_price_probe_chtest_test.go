//go:build chtest

package clickhouse

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
)

// insertRetiredPrices adds price records under digests no read prices with,
// as each pricing catalog change leaves behind on a long-lived mirror.
func insertRetiredPrices(t *testing.T, store *Store, digests, perDigest int) {
	t.Helper()
	_, err := store.DB().ExecContext(t.Context(), fmt.Sprintf(`
		INSERT INTO usage_event_prices (pricing_digest, price_key, priced, push_version)
		SELECT concat('retired-', toString(intDiv(number, %[1]d))), toString(number), 1, 1
		FROM numbers(%[2]d)`, perDigest, digests*perDigest))
	require.NoError(t, err)
}

// usageIdentityProbeReadRows returns the rows read by the latest identity
// probe.
func usageIdentityProbeReadRows(t *testing.T, store *Store) uint64 {
	t.Helper()
	execPreparedUsage(t, store, "SYSTEM FLUSH LOGS")
	var rows uint64
	require.NoError(t, store.DB().QueryRowContext(t.Context(), `
		SELECT argMax(read_rows, event_time_microseconds) FROM system.query_log
		WHERE current_database = currentDatabase() AND type = 'QueryFinish' AND is_initial_query = 1
			AND startsWith(query, 'SELECT [assumeNotNull(')
	`).Scan(&rows))
	return rows
}

// Price records of digests no read uses must neither be read by the row
// identity probe nor discard the kept usage results.
func TestUsageVersionIgnoresOtherDigestsPrices(t *testing.T) {
	_, syncer, store := newPreparedUsageStore(t)
	require.NoError(t, syncer.syncModelPricing(t.Context()))
	runPreparedUsageRefresh(t, store)
	f := db.UsageFilter{Timezone: "UTC", Breakdowns: true}
	before := readPreparedUsage(t, store, f)
	_, results := usageQueryCounts(t, store)

	const retired = 100_000
	insertRetiredPrices(t, store, 4, retired/4)
	require.Equal(t, before, readPreparedUsage(t, store, f))
	_, after := usageQueryCounts(t, store)
	require.Equal(t, results, after, "records of other digests must not discard kept usage results")
	require.Less(t, usageIdentityProbeReadRows(t, store), uint64(retired),
		"the identity probe must read only the read digest's price records")
}

// BenchmarkColdUsageVersionWithRetiredPrices measures the row identity
// probe that the first Activity or Usage request after serve starts, and
// the first after a push adds price records, runs before it reads any
// usage. The price table holds 100 retired digests of 200,000 records each
// beside the one the read prices with.
func BenchmarkColdUsageVersionWithRetiredPrices(b *testing.B) {
	ctx := b.Context()
	dsn, database := chtest.FreshDatabase(b)
	target := Target{URL: dsn, Database: database}
	require.NoError(b, EnsureSchema(ctx, target))
	store, err := NewStore(ctx, target)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	const current = "current"
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO usage_event_prices (pricing_digest, price_key, priced, push_version)
		SELECT if(number < 200000, ?, concat('retired-', toString(intDiv(number, 200000)))),
			toString(number), 1, 1
		FROM numbers(101 * 200000)`, current)
	require.NoError(b, err, "seed price records")
	_, err = store.DB().ExecContext(ctx, "OPTIMIZE TABLE usage_event_prices FINAL")
	require.NoError(b, err)
	for b.Loop() {
		_, ok, err := NewStoreFromDB(store.DB()).usageRowsVersion(ctx, chUsageReadTables, current)
		require.NoError(b, err)
		require.True(b, ok)
	}
}
