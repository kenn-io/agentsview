//go:build chtest

package clickhouse

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

// Preparing usage must preserve pricing and deduplication, keep a failed push
// invisible, and replace corrected or removed facts after the next refresh.
// Between a push and its refresh, reads use the raw rows rather than the
// previous push's prepared usage.
func TestPreparedUsagePublicationAndPricing(t *testing.T) {
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := NewStoreFromDB(syncer.conn)
	// A stopped view remembers the refresh each push requests and runs it
	// once resumed, so the test controls when prepared usage catches up.
	_, err := store.DB().ExecContext(ctx, "ALTER TABLE prepare_usage MODIFY REFRESH EVERY 1 DAY")
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "SYSTEM STOP VIEW prepare_usage")
	require.NoError(t, err)
	refresh := func() {
		t.Helper()
		for _, stmt := range []string{
			"SYSTEM START VIEW prepare_usage", "SYSTEM REFRESH VIEW prepare_usage",
			"SYSTEM WAIT VIEW prepare_usage", "SYSTEM STOP VIEW prepare_usage",
		} {
			_, err := store.DB().ExecContext(ctx, stmt)
			require.NoError(t, err, stmt)
		}
	}
	requireReady := func(want bool) {
		t.Helper()
		ready, err := store.preparedUsageReady(ctx)
		require.NoError(t, err)
		require.Equal(t, want, ready)
	}
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	ids := []string{usagePriceRoundID, usagePriceTierID, usagePriceSnapAID, usagePriceSnapBID, usagePriceMixedID, usagePriceCopilotID}
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-12", Timezone: "UTC"},
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	start, end := activityReportRangeBoundsUTC(q)
	read := func() []activity.UsageRow {
		t.Helper()
		rows, _, err := store.activityReportUsage(ctx, chSessionSetFromIDs(ids), ids, start, end, q)
		require.NoError(t, err)
		return rows
	}
	requireReady(false)
	before := read()
	require.NotEmpty(t, before)
	probeBefore, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.Empty(t, probeBefore.PreparedUsageFingerprint)
	refresh()
	requireReady(true)
	require.Equal(t, before, read(), "prepared usage must match the raw rows it replaces")
	probeAfter, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, probeAfter.PreparedUsageFingerprint)
	require.NotEqual(t, probeBefore, probeAfter)
	refresh()
	probeUnchanged, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.Equal(t, probeAfter, probeUnchanged, "rebuilding unchanged usage must preserve report tokens")
	_, err = store.DB().ExecContext(ctx, "SYSTEM STOP MERGES prepared_usage")
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "INSERT INTO prepared_usage SELECT * FROM prepared_usage WHERE session_id=?", usagePriceRoundID)
	require.NoError(t, err)
	probeMultipleParts, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.NotEqual(t, probeUnchanged, probeMultipleParts, "duplicate rows must affect the content fingerprint")
	var parts uint64
	require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT count() FROM system.parts WHERE database=currentDatabase() AND table='prepared_usage' AND active").Scan(&parts))
	require.Greater(t, parts, uint64(1))
	_, err = store.DB().ExecContext(ctx, "SYSTEM START MERGES prepared_usage")
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "OPTIMIZE TABLE prepared_usage FINAL")
	require.NoError(t, err)
	probeMerged, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.Equal(t, probeMultipleParts, probeMerged, "merging unchanged usage must preserve report tokens")
	refresh()
	probeRestored, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.Equal(t, probeUnchanged, probeRestored, "the same usage rows must restore the same fingerprint")

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "round-test",
		InputPerMTok: money.MustParseDollars("2"), OutputPerMTok: money.MustParseDollars("3")}}))
	require.NoError(t, syncer.syncModelPricing(ctx))
	requireReady(true)
	repriced := read()
	require.NotEqual(t, before, repriced, "prepared facts must use the current pricing catalog")

	session, err := local.GetSessionFull(ctx, usagePriceRoundID)
	require.NoError(t, err)
	messages, err := local.GetAllMessages(ctx, usagePriceRoundID)
	require.NoError(t, err)
	messages[0].TokenUsage = []byte(`{"input_tokens":100}`)
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: *session, Messages: messages[:1], ReplaceMessages: true, DataVersion: 1,
	}})
	require.NoError(t, err)
	syncer.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error { return errors.New("injected publication failure") }}
	result, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.Errors)
	refresh()
	require.Equal(t, repriced, read())
	syncer.hooks = nil
	result, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	requireReady(false)
	corrected := read()
	require.NotEqual(t, repriced, corrected, "reads must not serve the previous push's prepared usage")
	refresh()
	requireReady(true)
	require.Equal(t, corrected, read())
	var roundRows []activity.UsageRow
	for _, row := range corrected {
		if row.SessionID == usagePriceRoundID {
			roundRows = append(roundRows, row)
		}
	}
	require.Len(t, roundRows, 1)
	require.Equal(t, 100, roundRows[0].InputTokens)
	probeCorrected, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	require.NotEqual(t, probeRestored.PreparedUsageFingerprint, probeCorrected.PreparedUsageFingerprint)

	require.NoError(t, local.SoftDeleteSession(ctx, usagePriceRoundID))
	_, err = local.DeleteSessionIfTrashed(ctx, usagePriceRoundID)
	require.NoError(t, err)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	refresh()
	requireReady(true)
	for _, row := range read() {
		require.NotEqual(t, usagePriceRoundID, row.SessionID)
	}
}

// An archive with an existing push cursor must republish unchanged sessions
// once when the new complete-snapshot tables are introduced.
func TestPreparedUsageBackfillsExistingPushCursor(t *testing.T) {
	ctx := t.Context()
	store, syncer, _ := newPushedStore(t)
	_, err := store.DB().ExecContext(ctx, "TRUNCATE TABLE usage_session_snapshots")
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "DELETE FROM sync_metadata WHERE key=? SETTINGS mutations_sync=2",
		syncer.archiveKey(usageSnapshotReadyKeyBase))
	require.NoError(t, err)
	ready, err := store.preparedUsageReady(ctx)
	require.NoError(t, err)
	require.False(t, ready)
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.True(t, result.Full)
	require.Zero(t, result.Errors)
	var missing uint64
	require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT count() FROM sessions
		WHERE id NOT IN (SELECT id FROM usage_session_snapshots)`).Scan(&missing))
	require.Zero(t, missing)
	result, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.False(t, result.Full)
	require.Zero(t, result.Errors)
}

// Range usage reads must return the same result from prepared usage as
// from the raw messages and usage events they replace.
func TestPreparedUsageServesRangeUsageReads(t *testing.T) {
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := NewStoreFromDB(syncer.conn)
	// The stopped view holds the push's refresh until the raw rows are read.
	_, err := store.DB().ExecContext(ctx, "SYSTEM STOP VIEW prepare_usage")
	require.NoError(t, err)
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	// The project and model filters select sessions whose Claude snapshot
	// facts are attributed to a session outside the filter.
	filters := []db.UsageFilter{
		{Timezone: "UTC", From: "2026-01-12", To: "2026-01-12", Breakdowns: true},
		{Timezone: "America/New_York", From: "2026-01-11", To: "2026-01-13", Breakdowns: true},
		{Timezone: "UTC", From: "2026-01-01", To: "2026-01-31", Agent: "claude", ExcludeOneShot: true},
		{Timezone: "UTC", Project: "delta", Breakdowns: true},
		{Timezone: "UTC", Model: "claude-test", Breakdowns: true},
		{Timezone: "UTC", ExcludeModel: "claude-test"},
		// Unfiltered reads union the cursor rows, which the per-request join
		// still prices.
		{Timezone: "UTC", Breakdowns: true},
	}
	type reads struct {
		daily  []db.DailyUsageResult
		top    [][]db.TopSessionEntry
		counts []db.UsageSessionCounts
	}
	read := func() reads {
		t.Helper()
		var out reads
		for _, f := range filters {
			daily, err := store.GetDailyUsage(ctx, f)
			require.NoError(t, err)
			out.daily = append(out.daily, daily)
			top, err := store.GetTopSessionsByCost(ctx, f, 10)
			require.NoError(t, err)
			out.top = append(out.top, top)
			counts, err := store.GetUsageSessionCounts(ctx, f)
			require.NoError(t, err)
			out.counts = append(out.counts, counts)
		}
		return out
	}
	raw := read()
	require.NotEmpty(t, raw.daily[0].Daily)
	ready, err := store.preparedUsageReady(ctx)
	require.NoError(t, err)
	require.False(t, ready)
	for _, stmt := range []string{
		"SYSTEM START VIEW prepare_usage", "SYSTEM REFRESH VIEW prepare_usage", "SYSTEM WAIT VIEW prepare_usage",
	} {
		_, err := store.DB().ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	state, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.True(t, state.ready)
	// The stored price records serve the reads, not the per-request join.
	pricing, err := store.pricingSnapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, pricing.catalog.digest, state.pricingDigest)
	require.Equal(t, raw, read())
	// A mirror whose price records were cleared still reads the prepared
	// rows, but prices them through the join like the raw rows.
	_, err = store.DB().ExecContext(ctx, "TRUNCATE TABLE usage_event_prices")
	require.NoError(t, err)
	state, err = store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.True(t, state.ready)
	require.Empty(t, state.pricingDigest)
	cleared := read()
	for i := range filters {
		require.Equal(t, dailyUsageWire(t, raw.daily[i], false), dailyUsageWire(t, cleared.daily[i], false))
	}
	require.Equal(t, raw.top, cleared.top)
	require.Equal(t, raw.counts, cleared.counts)
}

// A rebuild that stopped after dropping the view but before the table must
// complete on the next start instead of failing to stop the missing view.
func TestEnsurePreparedUsageRecoversFromInterruptedRebuild(t *testing.T) {
	ctx := t.Context()
	store, _, _ := newPushedStore(t)
	conn := store.DB()
	for _, query := range []string{
		"SYSTEM STOP VIEW prepare_usage",
		"DROP VIEW prepare_usage SYNC",
		"ALTER TABLE prepared_usage MODIFY COMMENT 'prepared by an earlier query'",
	} {
		_, err := conn.ExecContext(ctx, query)
		require.NoError(t, err, query)
	}
	require.NoError(t, ensurePreparedUsage(ctx, conn))
	var comment string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT comment FROM system.tables
		WHERE database = currentDatabase() AND name = 'prepared_usage'`).Scan(&comment))
	require.Equal(t, chPreparedUsageComment(), comment)
	var views uint64
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count() FROM system.tables
		WHERE database = currentDatabase() AND name = 'prepare_usage'`).Scan(&views))
	require.Equal(t, uint64(1), views)
}

// An installation whose prepared usage table was made by an earlier query,
// here one sorted by the nullable timestamp expression that ClickHouse
// cannot prune through, must be rebuilt for the current query.
func TestEnsurePreparedUsageRebuildsOutdatedTable(t *testing.T) {
	ctx := t.Context()
	store, _, _ := newPushedStore(t)
	conn := store.DB()
	selection := clickUsageNormalizedQueryFrom("", chUsageStoredMessageEligibility, chUsageEventEligibility,
		"usage_session_snapshots s ARRAY JOIN s.usage_facts AS m",
		"usage_session_snapshots s ARRAY JOIN s.usage_events AS ue", "1")
	for _, query := range []string{
		"SYSTEM STOP VIEW prepare_usage",
		"DROP VIEW prepare_usage SYNC",
		"DROP TABLE prepared_usage SYNC",
		`CREATE TABLE prepared_usage ENGINE=MergeTree
		ORDER BY (ifNull(ts,toDateTime64(0,6,'UTC')),session_id) AS ` + selection + " LIMIT 0",
		"CREATE MATERIALIZED VIEW prepare_usage REFRESH EVERY 1 DAY TO prepared_usage EMPTY AS " + selection,
	} {
		_, err := conn.ExecContext(ctx, query)
		require.NoError(t, err, query)
	}
	sortingKey := func() string {
		t.Helper()
		var key string
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT sorting_key FROM system.tables
			WHERE database = currentDatabase() AND name = 'prepared_usage'`).Scan(&key))
		return key
	}
	require.NotEqual(t, chPreparedUsageSortingKey, sortingKey())
	ready, err := store.preparedUsageReady(ctx)
	require.NoError(t, err)
	require.False(t, ready, "a table prepared by another query must not serve reads")
	require.NoError(t, ensurePreparedUsage(ctx, conn))
	require.Equal(t, chPreparedUsageSortingKey, sortingKey())
	_, err = conn.ExecContext(ctx, "SYSTEM REFRESH VIEW prepare_usage")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)
	var rows, keyed uint64
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(), countIf(ts_key = ifNull(ts,toDateTime64(0,6,'UTC')))
		FROM prepared_usage`).Scan(&rows, &keyed))
	require.NotZero(t, rows)
	require.Equal(t, rows, keyed)
	// A current table is left alone, including its rows.
	require.NoError(t, ensurePreparedUsage(ctx, conn))
	var after uint64
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT count() FROM prepared_usage").Scan(&after))
	require.Equal(t, rows, after)
}

// A process that starts while the prepared usage refresh swaps in its new
// table must still find the schema in place: every refresh replaces the
// table, so a schema step that alters it can meet the old table.
func TestEnsureSchemaDuringPreparedUsageRefresh(t *testing.T) {
	ctx := t.Context()
	store, syncer, _ := newPushedStore(t)
	for range 20 {
		_, err := store.DB().ExecContext(ctx, "SYSTEM REFRESH VIEW prepare_usage")
		require.NoError(t, err)
		require.NoError(t, EnsureSchema(ctx, syncer.target))
	}
}

// A projection step that fails after it stopped the refresh must start the
// refresh again. Otherwise prepared usage stays stale, and a later start that
// finds the projections present never restarts it.
func TestEnsurePreparedUsageProjectionsRestartsRefreshAfterFailure(t *testing.T) {
	ctx := t.Context()
	store, _, _ := newPushedStore(t)
	conn := store.DB()
	saved := chPreparedUsageProjections
	t.Cleanup(func() { chPreparedUsageProjections = saved })
	chPreparedUsageProjections = append(slices.Clone(saved),
		struct{ name, query string }{"broken", "SELECT no_such_column"})
	require.Error(t, ensurePreparedUsageProjections(ctx, conn))
	var status string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT toString(status) FROM system.view_refreshes
		WHERE database = currentDatabase() AND view = 'prepare_usage'`).Scan(&status))
	require.NotEqual(t, "Disabled", status)
}
