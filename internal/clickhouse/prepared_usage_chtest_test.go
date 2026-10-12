//go:build chtest

package clickhouse

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
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

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "round-test",
		InputPerMTok: money.MustParseDollars("2"), OutputPerMTok: money.MustParseDollars("3"),
	}}))
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
	// The push republished one session; until the refresh, its rows are
	// prepared at read time while the other sessions keep their stored rows.
	state, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.True(t, state.ready)
	require.Equal(t, []string{usagePriceRoundID}, state.changed)
	require.Equal(t, []string{usagePriceRoundID}, state.stale)
	corrected := read()
	require.NotEqual(t, repriced, corrected, "reads must not serve the previous push's prepared usage")
	refresh()
	state, err = store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.True(t, state.ready)
	require.Empty(t, state.changed)
	require.Empty(t, state.stale)
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
		"usage_session_snapshots s ARRAY JOIN s.usage_events AS ue", "1", "")
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

// Between a refresh and the next, a push republishes, adds, and removes
// sessions. Reads keep the prepared rows of the untouched sessions and
// prepare the pushed sessions' rows themselves, so they match both the
// refresh that follows and the raw rows.
func TestPreparedUsageServesReadsBetweenRefreshes(t *testing.T) {
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := NewStoreFromDB(syncer.conn)
	exec := func(stmts ...string) {
		t.Helper()
		for _, stmt := range stmts {
			_, err := store.DB().ExecContext(ctx, stmt)
			require.NoError(t, err, stmt)
		}
	}
	push := func() {
		t.Helper()
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
	}
	exec("SYSTEM STOP VIEW prepare_usage")
	push()
	exec("SYSTEM START VIEW prepare_usage", "SYSTEM REFRESH VIEW prepare_usage",
		"SYSTEM WAIT VIEW prepare_usage", "SYSTEM STOP VIEW prepare_usage")
	filters := []db.UsageFilter{
		{Timezone: "UTC", From: "2026-01-12", To: "2026-01-12", Breakdowns: true},
		{Timezone: "UTC", From: "2026-01-01", To: "2026-01-31", Agent: "claude", ExcludeOneShot: true},
		{Timezone: "UTC", Project: "delta", Breakdowns: true},
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
	state, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.True(t, state.ready)
	require.Empty(t, state.changed)
	require.Empty(t, state.stale)
	prepared := read()

	// Republish one session with different usage, remove another, and add
	// a third; the stopped view holds the refresh the push requests.
	session, err := local.GetSessionFull(ctx, usagePriceRoundID)
	require.NoError(t, err)
	messages, err := local.GetAllMessages(ctx, usagePriceRoundID)
	require.NoError(t, err)
	messages[0].TokenUsage = []byte(`{"input_tokens":100}`)
	const addedID = "ch-price-added"
	added := fixtureSession(addedID, "delta", "added first", "2026-01-12T03:45:00.000Z", 1)
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{
		{Session: *session, Messages: messages[:1], ReplaceMessages: true, DataVersion: 1},
		{Session: added, Messages: []db.Message{usagePriceMessage(addedID, 0, "2026-01-12T03:45:00.000Z",
			"tier-test", `{"input_tokens":1500,"output_tokens":200}`)}, ReplaceMessages: true, DataVersion: 1},
	})
	require.NoError(t, err)
	require.NoError(t, local.SoftDeleteSession(ctx, usagePriceSnapBID))
	deleted, err := local.DeleteSessionIfTrashed(ctx, usagePriceSnapBID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	push()
	state, err = store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.True(t, state.ready)
	require.ElementsMatch(t, []string{usagePriceRoundID, addedID}, state.changed)
	require.ElementsMatch(t, []string{usagePriceRoundID, usagePriceSnapBID}, state.stale)
	between := read()
	require.NotEqual(t, prepared, between, "reads must serve the pushed sessions")

	exec("SYSTEM START VIEW prepare_usage", "SYSTEM REFRESH VIEW prepare_usage",
		"SYSTEM WAIT VIEW prepare_usage", "SYSTEM STOP VIEW prepare_usage")
	state, err = store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.True(t, state.ready)
	require.Empty(t, state.changed)
	require.Empty(t, state.stale)
	require.Equal(t, between, read(), "rows prepared at read time must match the refresh")
	// An empty table is not ready, so the same reads use the raw rows.
	exec("TRUNCATE TABLE prepared_usage")
	ready, err := store.preparedUsageReady(ctx)
	require.NoError(t, err)
	require.False(t, ready)
	require.Equal(t, between, read(), "rows prepared at read time must match the raw rows")
}

// A read that took its prepared state before a refresh and runs after it
// counts a new session once: the refresh already holds the rows the
// state's delta supplies.
func TestPreparedUsageReadAcrossRefreshCountsNewSessionOnce(t *testing.T) {
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := NewStoreFromDB(syncer.conn)
	exec := func(stmts ...string) {
		t.Helper()
		for _, stmt := range stmts {
			_, err := store.DB().ExecContext(ctx, stmt)
			require.NoError(t, err, stmt)
		}
	}
	refresh := func() {
		t.Helper()
		exec("SYSTEM START VIEW prepare_usage", "SYSTEM REFRESH VIEW prepare_usage",
			"SYSTEM WAIT VIEW prepare_usage", "SYSTEM STOP VIEW prepare_usage")
	}
	push := func() {
		t.Helper()
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
	}
	exec("SYSTEM STOP VIEW prepare_usage")
	push()
	refresh()
	const addedID = "ch-price-added"
	added := fixtureSession(addedID, "delta", "added first", "2026-01-12T03:45:00.000Z", 1)
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: added, Messages: []db.Message{usagePriceMessage(addedID, 0, "2026-01-12T03:45:00.000Z",
			"tier-test", `{"input_tokens":1500,"output_tokens":200}`)}, ReplaceMessages: true, DataVersion: 1,
	}})
	require.NoError(t, err)
	push()
	count := func(state preparedUsageState) uint64 {
		t.Helper()
		query, args := chPreparedUsageSourceSQL(state, "session_id = ?", []any{addedID})
		readCtx, err := withUsageDeltaTables(ctx, state)
		require.NoError(t, err)
		var n uint64
		require.NoError(t, store.queryRowContext(readCtx, "SELECT count() FROM ("+query+")", args...).Scan(&n))
		return n
	}
	taken, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{addedID}, taken.changed)
	want := count(taken)
	require.NotZero(t, want)
	refresh()
	require.Equal(t, want, count(taken), "a state taken before the refresh must not add the delta to the refreshed rows")
}

// A push that stops after writing a session's messages, before it prices
// them or writes the snapshot and session rows, must not leave that
// session's usage read from before the push.
func TestSessionUsageReadsMessagesOfInterruptedPush(t *testing.T) {
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := NewStoreFromDB(syncer.conn)
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	_, err = store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)
	ready, err := store.preparedUsageReady(ctx)
	require.NoError(t, err)
	require.True(t, ready)
	before, err := store.GetSessionUsage(ctx, usagePriceRoundID, true)
	require.NoError(t, err)

	// The rows insertDependents writes for a republished message.
	_, err = store.DB().ExecContext(ctx, `INSERT INTO messages
		SELECT * REPLACE (? AS token_usage, 0 AS output_tokens, push_version + 1 AS push_version)
		FROM messages WHERE session_id = ? AND ordinal = 0`, `{"input_tokens":100}`, usagePriceRoundID)
	require.NoError(t, err)

	after, err := store.GetSessionUsage(ctx, usagePriceRoundID, true)
	require.NoError(t, err)
	fresh, err := NewStoreFromDB(store.DB()).GetSessionUsage(ctx, usagePriceRoundID, true)
	require.NoError(t, err)
	require.NotEqual(t, before, fresh, "the interrupted push must have changed the raw rows")
	require.Equal(t, fresh, after)
}

// Session ids reach the changed-session query as an Array(String) query
// parameter; ClickHouse must parse back exactly the ids that were sent.
func TestChangedSessionParameterKeepsIDs(t *testing.T) {
	ctx := t.Context()
	store, _, _ := newPushedStore(t)
	ids := []string{"plain", `quote'd`, `back\slash`, `both\'`, "tab\tnewline\n"}
	var got []string
	require.NoError(t, store.queryRowContext(chdriver.Context(ctx, chdriver.WithParameters(chdriver.Parameters{
		"ids": chStringArrayLiteral(ids),
	})), "SELECT {ids:Array(String)}").Scan(&got))
	require.Equal(t, ids, got)
}

// A delta that reuses the kept rows of an earlier changed snapshot and
// reads a newly changed one serves what a store that kept nothing does.
func TestUsageDeltaReusesKeptSnapshotRows(t *testing.T) {
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := NewStoreFromDB(syncer.conn)
	push := func() {
		t.Helper()
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
	}
	_, err := store.DB().ExecContext(ctx, "SYSTEM STOP VIEW prepare_usage")
	require.NoError(t, err)
	push()
	for _, stmt := range []string{
		"SYSTEM START VIEW prepare_usage", "SYSTEM REFRESH VIEW prepare_usage",
		"SYSTEM WAIT VIEW prepare_usage", "SYSTEM STOP VIEW prepare_usage",
	} {
		_, err := store.DB().ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	republish := func(id, usage string) {
		t.Helper()
		session, err := local.GetSessionFull(ctx, id)
		require.NoError(t, err)
		messages, err := local.GetAllMessages(ctx, id)
		require.NoError(t, err)
		messages[0].TokenUsage = []byte(usage)
		_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
			Session: *session, Messages: messages[:1], ReplaceMessages: true, DataVersion: 1,
		}})
		require.NoError(t, err)
		push()
	}
	f := db.UsageFilter{Timezone: "UTC", Breakdowns: true}
	requireFresh := func() {
		t.Helper()
		got, err := store.GetDailyUsage(ctx, f)
		require.NoError(t, err)
		fresh, err := NewStoreFromDB(store.DB()).GetDailyUsage(ctx, f)
		require.NoError(t, err)
		require.Equal(t, fresh, got)
	}
	republish(usagePriceRoundID, `{"input_tokens":100}`)
	requireFresh()
	republish(usagePriceTierID, `{"input_tokens":2500,"output_tokens":10}`)
	state, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{usagePriceRoundID, usagePriceTierID}, state.changed)
	requireFresh()
}

// A server that cannot exchange tables, such as one whose data directory is
// on NFS, fails every refresh of a refreshable view that replaces its
// target. There, each push must leave prepared usage filled from the
// snapshots it published, and prepared reads must match the raw rows they
// replace. A mirror an earlier version set up with such a view recovers on
// the next push.
func TestPreparedUsageWithoutTableExchange(t *testing.T) {
	ctx := t.Context()
	local, _ := seedUsagePriceFixture(t)
	dsn, database := chtest.FreshDatabaseOn(t, chtest.NoExchangeServerURL(t))
	target := Target{URL: dsn, Database: database}
	earlier := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := earlier.conn.ExecContext(ctx, `CREATE MATERIALIZED VIEW IF NOT EXISTS prepare_usage
		REFRESH EVERY 15 MINUTE TO prepared_usage EMPTY AS `+chPreparedUsageQuery()+
		" SETTINGS final=1,max_threads=4")
	require.NoError(t, err)

	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := NewStoreFromDB(syncer.conn)
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
	push := func() {
		t.Helper()
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
		// The push itself refreshed prepared usage, so its rows cover every
		// session without preparing any at read time.
		state, err := store.preparedUsageState(ctx)
		require.NoError(t, err)
		require.True(t, state.ready)
		require.Empty(t, state.changed)
		require.Empty(t, state.stale)
	}
	parts := func() []string {
		t.Helper()
		var names []string
		rows, err := store.DB().QueryContext(ctx, `SELECT name FROM system.parts
			WHERE database = currentDatabase() AND table = 'prepared_usage' AND active ORDER BY name`)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			names = append(names, name)
		}
		require.NoError(t, rows.Err())
		return names
	}
	locks := func() []string {
		t.Helper()
		return preparedUsageTables(t, store.DB(), "prepared_usage_rebuild_lock")
	}
	push()
	built := parts()
	require.NotEmpty(t, built)
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	require.Equal(t, built, parts(), "a push that changed nothing must keep the prepared rows")
	require.Equal(t, []string{"prepared_usage_rebuild_lock_2 released"}, locks(),
		"a push that changed nothing must not take the rebuild lock")

	session, err := local.GetSessionFull(ctx, usagePriceRoundID)
	require.NoError(t, err)
	messages, err := local.GetAllMessages(ctx, usagePriceRoundID)
	require.NoError(t, err)
	messages[0].TokenUsage = []byte(`{"input_tokens":100}`)
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: *session, Messages: messages[:1], ReplaceMessages: true, DataVersion: 1,
	}})
	require.NoError(t, err)
	// While another push holds the rebuild lock, a push leaves the rows in
	// place, and reads prepare the republished session themselves.
	var now int64
	require.NoError(t, store.DB().QueryRowContext(ctx, "SELECT toUnixTimestamp(now())").Scan(&now))
	require.NoError(t, createRebuildLock(ctx, store.DB(), 100, rebuildLockState{Status: rebuildLockHeld, RenewedAt: now}))
	result, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	require.Equal(t, built, parts(), "a push must not rebuild while another holds the lock")
	state, err := store.preparedUsageState(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{usagePriceRoundID}, state.changed)
	// A lock not renewed for more than an hour belongs to a stopped push.
	// The next push takes over, and one released lock table remains. The
	// holder that overran may have passed its ownership check with its build
	// filled; after the takeover it can no longer publish the older rows. A
	// build of a newer lock belongs to a successor and stays.
	require.NoError(t, setRebuildLock(ctx, store.DB(), 100, rebuildLockState{Status: rebuildLockHeld, RenewedAt: now - 2*60*60}))
	for _, stmt := range []string{
		"CREATE TABLE prepared_usage_build_100 AS prepared_usage",
		"INSERT INTO prepared_usage_build_100 SELECT * FROM prepared_usage",
		"CREATE TABLE prepared_usage_build_102 AS prepared_usage",
	} {
		_, err = store.DB().ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	push()
	_, err = store.DB().ExecContext(ctx, "ALTER TABLE prepared_usage REPLACE PARTITION tuple() FROM prepared_usage_build_100")
	require.True(t, isMissingTableError(err), "the overran holder must not publish after a takeover: %v", err)
	_, err = store.DB().ExecContext(ctx, "DROP TABLE prepared_usage_build_102 SYNC")
	require.NoError(t, err, "a rebuild must leave a newer holder's build table alone")
	require.Equal(t, []string{"prepared_usage_rebuild_lock_101 released"}, locks())
	prepared := read()
	var roundRows []activity.UsageRow
	for _, row := range prepared {
		if row.SessionID == usagePriceRoundID {
			roundRows = append(roundRows, row)
		}
	}
	require.Len(t, roundRows, 1)
	require.Equal(t, 100, roundRows[0].InputTokens)

	_, err = store.DB().ExecContext(ctx, "TRUNCATE TABLE prepared_usage")
	require.NoError(t, err)
	ready, err := store.preparedUsageReady(ctx)
	require.NoError(t, err)
	require.False(t, ready)
	require.Equal(t, read(), prepared, "prepared usage must match the raw rows it replaces")
}

// A database that can exchange tables refreshes prepared usage with its
// view. The schema step leaves a database that never rebuilt without lock
// tables. After rebuilds, as when the data directory moved off NFS, it
// drops their build tables and older locks and keeps one newer retired
// lock: a rebuild that paused through the switch discards its build, and a
// push that paused before taking the lock skips its rebuild.
func TestEnsurePreparedUsageRetiresRebuildsWithTableExchange(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	conn := chtest.Open(t, target.URL, target.Database)
	rebuildTables := func() []string {
		t.Helper()
		return preparedUsageTables(t, conn, "prepared_usage_")
	}
	newTestSync(t, local, target, storage.PusherOptions{})
	require.Empty(t, rebuildTables())

	require.NoError(t, createRebuildLock(ctx, conn, 6, rebuildLockState{Status: rebuildLockReleased}))
	require.NoError(t, createRebuildLock(ctx, conn, 7, rebuildLockState{Status: rebuildLockHeld, RenewedAt: 1}))
	_, err := conn.ExecContext(ctx, "CREATE TABLE prepared_usage_build_7 AS prepared_usage")
	require.NoError(t, err)
	newTestSync(t, local, target, storage.PusherOptions{})
	require.Equal(t, []string{"prepared_usage_rebuild_lock_8 retired"}, rebuildTables())
	superseded, err := rebuildLockSuperseded(ctx, conn, 7)
	require.NoError(t, err)
	require.True(t, superseded, "a rebuild paused through the switch must find a newer lock")
	_, err = conn.ExecContext(ctx, "ALTER TABLE prepared_usage REPLACE PARTITION tuple() FROM prepared_usage_build_7")
	require.True(t, isMissingTableError(err), "a rebuild paused through the switch must not publish: %v", err)
	_, acquired, err := acquireRebuildLock(ctx, conn)
	require.NoError(t, err)
	require.False(t, acquired, "a push paused before taking the lock must not rebuild")
	newTestSync(t, local, target, storage.PusherOptions{})
	require.Equal(t, []string{"prepared_usage_rebuild_lock_8 retired"}, rebuildTables())

	// Retirement that read the locks before pushes took newer numbers can
	// create a retired lock below a newer one. That lock is not the fence,
	// and retiring again goes above the newer lock.
	require.NoError(t, createRebuildLock(ctx, conn, 10, rebuildLockState{Status: rebuildLockHeld, RenewedAt: 1}))
	newestRetired, err := createRetiredLock(ctx, conn, 9)
	require.NoError(t, err)
	require.False(t, newestRetired)
	require.NoError(t, retireRebuildLocks(ctx, conn))
	require.Equal(t, []string{"prepared_usage_rebuild_lock_11 retired"}, rebuildTables())
}

// A database that can swap tables has no rebuild lock, so a push that finds
// the view missing, as while a schema step recreates it, does not rebuild.
// One that finds the newest lock retired but no view finishes the switch to
// the view that a schema step left halfway.
func TestRefreshPreparedUsageWithoutViewFollowsRebuildLock(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	conn := chtest.Open(t, target.URL, target.Database)
	newTestSync(t, local, target, storage.PusherOptions{})
	tables := func() []string {
		t.Helper()
		return preparedUsageTables(t, conn, "prepare")
	}
	require.NoError(t, dropPreparedUsageView(ctx, conn))
	require.NoError(t, refreshPreparedUsage(ctx, conn))
	require.Empty(t, tables(), "a push must not rebuild or take a lock without one")

	require.NoError(t, createRebuildLock(ctx, conn, 3, rebuildLockState{Status: rebuildLockRetired}))
	_, err := conn.ExecContext(ctx, "CREATE TABLE prepared_usage_build_2 AS prepared_usage")
	require.NoError(t, err)
	require.NoError(t, refreshPreparedUsage(ctx, conn))
	require.Equal(t, []string{"prepare_usage ", "prepared_usage_rebuild_lock_3 retired"}, tables())
}

// A lock comment that a manual edit left unreadable stops rebuilds with an
// error that names the lock and the statement that repairs it.
func TestRebuildLockRejectsUnreadableComment(t *testing.T) {
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	for _, comment := range []string{"released", `{"status":"relased"}`} {
		_, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS prepared_usage_rebuild_lock_4 SYNC")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, "CREATE TABLE prepared_usage_rebuild_lock_4 (x UInt8) ENGINE = Memory COMMENT "+
			chSQLString(comment))
		require.NoError(t, err)
		_, acquired, err := acquireRebuildLock(ctx, conn)
		require.False(t, acquired)
		require.Error(t, err, comment)
		require.True(t, strings.HasSuffix(err.Error(),
			`: ALTER TABLE prepared_usage_rebuild_lock_4 MODIFY COMMENT '{"status":"released"}'`),
			"the error must end with the repair statement so it can be copied as is: %v", err)
		_, _, _, err = newestRebuildLock(ctx, conn)
		require.ErrorContains(t, err, "prepared_usage_rebuild_lock_4", comment)
	}
}

// A database that loses the ability to exchange tables again lets pushes
// take the rebuild lock that an earlier switch to the view retired.
func TestEnsurePreparedUsageReopensRetiredLockWithoutTableExchange(t *testing.T) {
	ctx := t.Context()
	local, _ := seedUsagePriceFixture(t)
	dsn, database := chtest.FreshDatabaseOn(t, chtest.NoExchangeServerURL(t))
	target := Target{URL: dsn, Database: database}
	conn := chtest.Open(t, dsn, database)
	newTestSync(t, local, target, storage.PusherOptions{})
	// As an earlier switch to the view left the database.
	require.NoError(t, createRebuildLock(ctx, conn, 5, rebuildLockState{Status: rebuildLockRetired}))
	require.NoError(t, createPreparedUsageView(ctx, conn))
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	hasView, err := preparedUsageViewExists(ctx, conn)
	require.NoError(t, err)
	require.False(t, hasView, "a database that cannot swap tables must not keep the view")
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	ready, err := NewStoreFromDB(syncer.conn).preparedUsageReady(ctx)
	require.NoError(t, err)
	require.True(t, ready, "the push must rebuild prepared usage")
	require.Equal(t, []string{"prepared_usage_rebuild_lock_6 released"},
		preparedUsageTables(t, conn, "prepared_usage_rebuild_lock"))
}

// A schema step that replaces prepared_usage drops the view before it
// releases a retired lock, so a push in that gap finds the newest lock
// retired and no view. In a database that cannot swap tables, the push must
// rebuild rather than create a view whose every refresh fails.
func TestRefreshPreparedUsageWithRetiredLockWithoutTableExchange(t *testing.T) {
	ctx := t.Context()
	local, _ := seedUsagePriceFixture(t)
	dsn, database := chtest.FreshDatabaseOn(t, chtest.NoExchangeServerURL(t))
	target := Target{URL: dsn, Database: database}
	conn := chtest.Open(t, dsn, database)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	// The retired lock an earlier switch to the view left behind.
	require.NoError(t, createRebuildLock(ctx, conn, 2, rebuildLockState{Status: rebuildLockRetired}))
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	ready, err := NewStoreFromDB(syncer.conn).preparedUsageReady(ctx)
	require.NoError(t, err)
	require.True(t, ready, "the push must rebuild prepared usage")
	require.Equal(t, []string{"prepared_usage_rebuild_lock_3 released"}, preparedUsageTables(t, conn, "prepare"),
		"the push must release the retired lock and rebuild without creating the view")
}

// A renewed rebuild lock stays held past its timeout. A push that stopped
// renewing must not publish once another push has taken a newer lock,
// renewing or releasing a lock a successor already dropped changes nothing,
// and a push that paused before taking a lock cannot take a retired one.
func TestPreparedUsageRebuildLockFencesSupersededHolder(t *testing.T) {
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	_, acquired, err := acquireRebuildLock(ctx, conn)
	require.NoError(t, err)
	require.False(t, acquired, "a database without a rebuild lock refreshes with its view")
	require.NoError(t, openRebuildLocks(ctx, conn))
	generation, acquired, err := acquireRebuildLock(ctx, conn)
	require.NoError(t, err)
	require.True(t, acquired)
	superseded, err := rebuildLockSuperseded(ctx, conn, generation)
	require.NoError(t, err)
	require.False(t, superseded)

	expire := func() {
		t.Helper()
		require.NoError(t, setRebuildLock(ctx, conn, generation, rebuildLockState{Status: rebuildLockHeld, RenewedAt: 1}))
	}
	expire()
	require.NoError(t, renewRebuildLock(ctx, conn, generation))
	_, acquired, err = acquireRebuildLock(ctx, conn)
	require.NoError(t, err)
	require.False(t, acquired, "a renewed lock is still held")

	expire()
	successor, acquired, err := acquireRebuildLock(ctx, conn)
	require.NoError(t, err)
	require.True(t, acquired)
	require.Equal(t, generation+1, successor)
	superseded, err = rebuildLockSuperseded(ctx, conn, generation)
	require.NoError(t, err)
	require.True(t, superseded)
	err = renewRebuildLock(ctx, conn, generation)
	require.True(t, isMissingTableError(err), "a superseded holder cannot renew: %v", err)
	require.NoError(t, releaseRebuildLock(ctx, conn, generation))
	acquired, err = takeRebuildLock(ctx, conn, generation, 1)
	require.NoError(t, err)
	require.False(t, acquired, "a retired generation must not be taken again")
	var retired uint64
	require.NoError(t, conn.QueryRowContext(ctx, fmt.Sprintf(`SELECT count() FROM system.tables
		WHERE database = currentDatabase() AND name = 'prepared_usage_rebuild_lock_%d'`, generation)).Scan(&retired))
	require.Zero(t, retired)

	// A takeover dropped the build table the superseded holder publishes.
	_, err = conn.ExecContext(ctx, "CREATE TABLE prepared_usage (x UInt8) ENGINE = MergeTree ORDER BY x")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "ALTER TABLE prepared_usage REPLACE PARTITION tuple() FROM prepared_usage_build_9")
	require.ErrorIs(t, supersededBuildError(ctx, conn, "prepared_usage_build_9", err), errRebuildSuperseded)

	_, acquired, err = acquireRebuildLock(ctx, conn)
	require.NoError(t, err)
	require.False(t, acquired, "the successor's lock is still held")
}

// preparedUsageTables lists the tables other than prepared_usage whose names
// start with prefix, each with the status in its rebuild lock comment, which
// is empty for tables that are not locks.
func preparedUsageTables(t *testing.T, conn *sql.DB, prefix string) []string {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(), `SELECT name || ' ' || JSONExtractString(comment, 'status')
		FROM system.tables WHERE database = currentDatabase() AND startsWith(name, ?) AND name != 'prepared_usage'
		ORDER BY name`, prefix)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}
