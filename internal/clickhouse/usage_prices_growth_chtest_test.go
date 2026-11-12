//go:build chtest

package clickhouse

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

// Stored prices grew to hundreds of millions of rows when pushes repriced
// rows they had already priced (#2813). A push writes a record only for a
// price key its digest lacks, so a full push of unchanged usage writes
// nothing, and machines whose catalogs alternate between two rates for a
// used model switch between two digests without repricing either again.
// Merges are stopped and the counts read without FINAL, so a duplicate insert
// stays visible: the store's connections read with final=1, which hides it.
func TestPriceRecordsStayBoundedAcrossRepeatedPushes(t *testing.T) {
	t.Parallel()
	store, syncer, local := newUsagePriceStore(t)
	ctx := t.Context()
	for _, table := range []string{"usage_event_prices", "usage_price_contexts"} {
		_, err := store.conn.ExecContext(ctx, "SYSTEM STOP MERGES "+table)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := store.conn.ExecContext(context.WithoutCancel(t.Context()), "SYSTEM START MERGES "+table)
			require.NoError(t, err)
		})
	}
	contextRows := func() int {
		t.Helper()
		var n int
		require.NoError(t, store.conn.QueryRowContext(ctx,
			"SELECT count() FROM usage_price_contexts SETTINGS final = 0").Scan(&n))
		return n
	}
	push := func(full bool) {
		t.Helper()
		result, err := syncer.Push(ctx, full, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
	}
	setRoundRate := func(input string) {
		t.Helper()
		require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
			ModelPattern:  "round-test",
			InputPerMTok:  money.MustParseDollars(input),
			OutputPerMTok: money.MustParseDollars("0.6"),
		}}))
	}

	digestA := pricedDigest(t, store.conn)
	initial := physicalDigestRows(t, store.conn)
	// The first push also leaves the digest it priced under before it
	// adopted the fixture's models; retention keeps it for a day.
	require.NotZero(t, initial[digestA])
	initialContexts := contextRows()

	push(true)
	push(false)
	assert.Equal(t, initial, physicalDigestRows(t, store.conn), "pushes of unchanged usage must write no price records")
	assert.Equal(t, initialContexts, contextRows())
	assert.Equal(t, digestA, pricedDigest(t, store.conn))

	setRoundRate("0.8")
	push(false)
	digestB := pricedDigest(t, store.conn)
	require.NotEqual(t, digestA, digestB, "a new rate for a used model must reprice under a new digest")
	assertDailyUsageParity(t, local, store)
	both := physicalDigestRows(t, store.conn)
	require.Equal(t, initial[digestA], both[digestA], "switching digests must keep the previous one's records")
	require.Equal(t, initial[digestA], both[digestB], "the new digest must price every key the old one did, once")
	bothContexts := contextRows()

	for _, rate := range []string{"0.4", "0.8", "0.4", "0.8"} {
		setRoundRate(rate)
		push(false)
		want := digestA
		if rate == "0.8" {
			want = digestB
		}
		assert.Equal(t, want, pricedDigest(t, store.conn), "rate %s", rate)
		assert.Equal(t, both, physicalDigestRows(t, store.conn), "returning to rate %s must reprice nothing", rate)
		assert.Equal(t, bothContexts, contextRows(), "rate %s", rate)
		assertDailyUsageParity(t, local, store)
	}
}

// physicalDigestRows counts the stored price records under each digest,
// duplicates included.
func physicalDigestRows(t *testing.T, conn *sql.DB) map[string]int {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(), `SELECT pricing_digest, count() FROM usage_event_prices
		GROUP BY pricing_digest SETTINGS final = 0`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var digest string
		var count int
		require.NoError(t, rows.Scan(&digest, &count))
		out[digest] = count
	}
	require.NoError(t, rows.Err())
	return out
}
