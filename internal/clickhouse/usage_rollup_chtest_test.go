//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

// usageRollupFilters cover zone, bound, filter, and breakdown shapes.
var usageRollupFilters = []db.UsageFilter{
	{Timezone: "UTC", From: "2026-01-12", To: "2026-01-12", Breakdowns: true},
	{Timezone: "America/New_York", From: "2026-01-11", To: "2026-01-14", Breakdowns: true},
	{Timezone: "Asia/Kathmandu", Breakdowns: true},
	{Timezone: "UTC", From: "2026-01-13"},
	{Timezone: "UTC", To: "2026-01-12", Breakdowns: true},
	{Timezone: "UTC", From: "2026-01-01", To: "2026-01-31", Agent: "claude", ExcludeOneShot: true},
	{Timezone: "UTC", Project: "delta", Breakdowns: true},
	{Timezone: "UTC", Model: "claude-test", Breakdowns: true},
	{Timezone: "UTC", ExcludeModel: "claude-test"},
	{Timezone: "UTC", Breakdowns: true},
}

const (
	usageRollupClaudeID = "ch-rollup-claude"
	usageRollupCopyID   = "ch-rollup-copy"
)

type usageRollupReads struct {
	daily  []db.DailyUsageResult
	top    [][]db.TopSessionEntry
	counts []db.UsageSessionCounts
}

// readUsageRollupFilters reads every filter through a new store, so no
// memo from an earlier read answers it.
func readUsageRollupFilters(t *testing.T, syncer *Sync) usageRollupReads {
	t.Helper()
	store := newTestStore(t, syncer.conn, syncer.target)
	var out usageRollupReads
	for _, f := range usageRollupFilters {
		rows := readPreparedUsage(t, store, f)
		out.daily = append(out.daily, rows.daily)
		out.top = append(out.top, rows.top)
		out.counts = append(out.counts, rows.counts)
	}
	return out
}

// usageRollupPlan reports how a priced read under f uses the rollup, or nil
// when it reads without it.
func usageRollupPlan(t *testing.T, syncer *Sync, f db.UsageFilter) *chUsageRollupRead {
	t.Helper()
	ctx := t.Context()
	store := newTestStore(t, syncer.conn, syncer.target)
	state, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	pricing, err := store.pricingSnapshot(ctx)
	require.NoError(t, err)
	plan, err := store.usageRollupRead(ctx, state, f, pricing.catalog.digest, nil, true)
	require.NoError(t, err)
	return plan
}

// newUsageRollupSync pushes the price fixture plus a Claude message in its
// own 15-minute bucket, refreshes prepared usage, and builds the rollup. The
// refresh view stays stopped.
func newUsageRollupSync(t *testing.T) (*db.DB, *Sync) {
	t.Helper()
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	claude := usagePriceMessage(usageRollupClaudeID, 0, "2026-01-14T05:00:00.000Z", "claude-test",
		`{"input_tokens":40,"output_tokens":4}`)
	claude.ClaudeMessageID = "msg_rollup_1"
	claude.ClaudeRequestID = "req_rollup_1"
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{
		{
			Session:  fixtureSession(usageRollupClaudeID, "delta", "rollup claude", "2026-01-14T05:00:00.000Z", 1),
			Messages: []db.Message{claude}, ReplaceMessages: true, DataVersion: 1,
		},
	})
	require.NoError(t, err)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := newTestStore(t, syncer.conn, syncer.target)
	execPreparedUsage(t, store, "SYSTEM STOP VIEW prepare_usage")
	pushPreparedUsage(t, syncer)
	runPreparedUsageRefresh(t, store)
	require.NoError(t, refreshUsageRollup(ctx, store.DB(), false))
	return local, syncer
}

// Reads that add the rollup's sums return what the same reads return from
// the prepared rows alone, and SQLite's daily usage, for every filter.
func TestUsageRollupReadsMatchPreparedRows(t *testing.T) {
	t.Parallel()
	local, syncer := newUsageRollupSync(t)
	for _, f := range usageRollupFilters {
		require.NotNil(t, usageRollupPlan(t, syncer, f), "filter %+v must use the rollup", f)
	}
	rolled := readUsageRollupFilters(t, syncer)
	assertDailyUsageParity(t, local, newTestStore(t, syncer.conn, syncer.target))

	execPreparedUsage(t, newTestStore(t, syncer.conn, syncer.target), "TRUNCATE TABLE "+usageRollupTable)
	require.Nil(t, usageRollupPlan(t, syncer, db.UsageFilter{Timezone: "UTC"}),
		"reads must not use a rollup without the stamp's partition")
	require.Equal(t, readUsageRollupFilters(t, syncer), rolled)
}

// Between a push and the next prepared refresh, reads take the pushed
// sessions from the delta. A pushed session that shares a deduplication
// identity with a summed group puts that group's rows back through the
// ranking, so the shared request is counted once.
func TestUsageRollupReadsMatchWithPushedSessions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, syncer := newUsageRollupSync(t)
	republishRoundSession(t, local, 9)
	copied := usagePriceMessage(usageRollupCopyID, 0, "2026-01-14T05:01:00.000Z", "claude-test",
		`{"input_tokens":40,"output_tokens":9}`)
	copied.ClaudeMessageID = "msg_rollup_1"
	copied.ClaudeRequestID = "req_rollup_1"
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session:  fixtureSession(usageRollupCopyID, "gamma", "rollup copy", "2026-01-14T05:01:00.000Z", 1),
		Messages: []db.Message{copied}, ReplaceMessages: true, DataVersion: 1,
	}})
	require.NoError(t, err)
	pushPreparedUsage(t, syncer)
	plan := usageRollupPlan(t, syncer, db.UsageFilter{Timezone: "UTC"})
	require.NotNil(t, plan)
	require.True(t, plan.delta)
	require.True(t, plan.replaced)
	rolled := readUsageRollupFilters(t, syncer)
	assertDailyUsageParity(t, local, newTestStore(t, syncer.conn, syncer.target))

	execPreparedUsage(t, newTestStore(t, syncer.conn, syncer.target), "TRUNCATE TABLE "+usageRollupTable)
	require.Equal(t, readUsageRollupFilters(t, syncer), rolled)

	// A refresh prepares the pushed rows under a new stamp, and the next
	// push that writes builds the rollup for it.
	runPreparedUsageRefresh(t, newTestStore(t, syncer.conn, syncer.target))
	require.Nil(t, usageRollupPlan(t, syncer, db.UsageFilter{Timezone: "UTC"}))
	republishRoundSession(t, local, 11)
	pushPreparedUsage(t, syncer)
	require.NotNil(t, usageRollupPlan(t, syncer, db.UsageFilter{Timezone: "UTC"}))
	assertDailyUsageParity(t, local, newTestStore(t, syncer.conn, syncer.target))
}

// A custom rate for a model the rollup summed at catalog prices sends reads
// back to the prepared rows, which price with the custom rate.
func TestUsageRollupSkippedForCustomRates(t *testing.T) {
	t.Parallel()
	_, syncer := newUsageRollupSync(t)
	store := newTestStore(t, syncer.conn, syncer.target)
	store.SetCustomPricing(map[string]config.CustomModelRate{
		"round-test": {InputMicrodollarsPerMTok: 5_000_000},
	})
	got, err := store.GetDailyUsage(t.Context(), db.UsageFilter{Timezone: "UTC", Agent: "hermes"})
	require.NoError(t, err)
	// $5/MTok input: 11 tokens over eight requests cost 55 microdollars.
	require.Equal(t, money.Money{Microdollars: 55}, modelCost(t, got, "round-test"))
}

// A summed bucket that a zone's offset splits across two local dates sends
// that zone's reads back to the prepared rows. Archived timestamps start in
// 2000, when every zone's offset was a multiple of 15 minutes, so the test
// copies a summed row into 1971, when Monrovia was 44 minutes 30 seconds
// behind UTC.
func TestUsageRollupSkippedForMisalignedZone(t *testing.T) {
	t.Parallel()
	_, syncer := newUsageRollupSync(t)
	monrovia := db.UsageFilter{Timezone: "Africa/Monrovia"}
	require.NotNil(t, usageRollupPlan(t, syncer, monrovia))
	// 1971-06-01 00:30 UTC was 1971-05-31 23:45:30 in Monrovia, and the
	// bucket's last second was 1971-06-01 00:00:29.
	execPreparedUsage(t, newTestStore(t, syncer.conn, syncer.target), `INSERT INTO `+usageRollupTable+`
		SELECT * REPLACE (44584200 AS bucket_key) FROM `+usageRollupTable+`
		WHERE row_kind = 'aggregate' AND sort_key = 0 AND NOT ts_null LIMIT 1`)
	require.Nil(t, usageRollupPlan(t, syncer, monrovia))
	require.NotNil(t, usageRollupPlan(t, syncer, db.UsageFilter{Timezone: "UTC"}))
}
