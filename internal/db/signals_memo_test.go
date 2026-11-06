package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignalsFrustrationMemoReuse(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "test")
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "this is broken"}}))
	require.NoError(t, d.UpdateSessionSignals(ctx, "session", SessionSignalUpdate{ToolRetryCount: 1, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}}))

	examples, err := d.GetAnalyticsSignalSessions(ctx, AnalyticsFilter{}, "tool_retries", 10)
	require.NoError(t, err)
	require.Len(t, examples.Sessions, 1)
	assert.Equal(t, "session", examples.Sessions[0].SessionID)
	assert.Empty(t, d.frustrationMemo.counts, "unrelated drill-down must skip the prompt scan")

	resp, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	require.Len(t, d.frustrationMemo.counts, 1)
	// A distinctive cached count proves the next request uses the memo.
	entry := d.frustrationMemo.counts["session"]
	d.frustrationMemo.Store(d.frustrationMemo.generation, "session", entry.version, 7)
	resp, err = d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 7, resp.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestSignalsFrustrationMemoInvalidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *DB)
		want   int
	}{
		{"append", func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: 1, Role: "user", Content: "this is broken"}}))
		}, 2},
		{"replace", func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session", []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "Please explain this function."}}))
		}, 0},
		{"recreate", func(t *testing.T, d *DB) {
			t.Helper()
			deleted, err := d.DeleteParserExcludedSessions(t.Context(), []string{"session"})
			require.NoError(t, err)
			require.Equal(t, 1, deleted)
			insertSession(t, d, "session", "test")
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "Please explain this function."}}))
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "session", "test")
			require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "this is broken"}}))
			examples, err := d.GetAnalyticsSignalSessions(ctx, AnalyticsFilter{}, "frustration_marker_count", 10)
			require.NoError(t, err)
			require.Len(t, examples.Sessions, 1)
			assert.Equal(t, 1, examples.Sessions[0].SignalTotal)
			tc.change(t, d)
			rows, err := d.signalRows(ctx, AnalyticsFilter{})
			require.NoError(t, err)
			require.NoError(t, d.populateFrustrationMarkers(ctx, rows))
			require.Len(t, rows, 1)
			assert.Equal(t, tc.want, rows[0].FrustrationMarkerCount)
		})
	}
}

func TestSignalsFrustrationMemoReaderSwap(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "test")
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "this is broken"}}))
	_, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	entry := d.frustrationMemo.counts["session"]
	generation := d.frustrationMemo.generation
	require.NoError(t, d.Reopen())
	d.frustrationMemo.Store(generation, "session", entry.version, 7)
	assert.Empty(t, d.frustrationMemo.counts, "swap must clear counts and reject an in-flight store")
}

type cancelAfterScanContext struct {
	context.Context
	reader *sql.DB
	cancel context.CancelFunc
}

func (ctx cancelAfterScanContext) Err() error {
	// The driver checks Err while holding a connection. Cancel only once
	// the completed scan has closed its rows and returned that connection.
	if ctx.reader.Stats().InUse == 0 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestSignalsFrustrationMemoCanceled(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "session", "test")
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "session", Role: "user", Content: "this is broken"}}))
	rows, err := d.signalRows(t.Context(), AnalyticsFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = d.populateFrustrationMarkers(cancelAfterScanContext{Context: ctx, reader: d.rawReader(), cancel: cancel}, rows)
	require.Equal(t, context.Canceled, err, "cancellation must come from the post-scan check, rather than a wrapped query error")
	assert.Equal(t, 1, rows[0].FrustrationMarkerCount, "the scan must finish before cancellation")
	assert.Empty(t, d.frustrationMemo.counts)
}
