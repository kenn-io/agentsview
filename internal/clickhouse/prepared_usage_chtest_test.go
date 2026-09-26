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
func TestPreparedUsagePublicationAndPricing(t *testing.T) {
	ctx := t.Context()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	store := NewStoreFromDB(syncer.conn)
	_, err = store.DB().ExecContext(ctx, "ALTER TABLE prepare_usage MODIFY REFRESH EVERY 1 DAY")
	require.NoError(t, err)
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
	refresh := func() {
		t.Helper()
		_, err := store.DB().ExecContext(ctx, "SYSTEM REFRESH VIEW prepare_usage")
		require.NoError(t, err)
		_, err = store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
		require.NoError(t, err)
	}
	before := read()
	require.NotEmpty(t, before)
	ready, err := store.preparedUsageReady(ctx)
	require.NoError(t, err)
	require.False(t, ready)
	// The fixture has completed its backfill; use an earlier completion time
	// so the test does not depend on crossing a wall-clock second.
	require.NoError(t, writeMetadata(ctx, syncer.conn, map[string]string{syncer.archiveKey(usageSnapshotReadyKeyBase): "1"}))
	probeBefore, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
	refresh()
	ready, err = store.preparedUsageReady(ctx)
	require.NoError(t, err)
	require.True(t, ready)
	require.Equal(t, before, read())
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
	require.Equal(t, repriced, read(), "the previous complete usage remains visible during preparation")
	refresh()
	corrected := read()
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
