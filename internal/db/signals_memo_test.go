package db

import (
	"context"
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
	d.frustrationMemo.store(d.frustrationMemo.generation, "session", entry.version, 7)
	resp, err = d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 7, resp.QualityHealth.Totals.FrustrationMarkerCount)
	examples, err = d.GetAnalyticsSignalSessions(ctx, AnalyticsFilter{}, "frustration_marker_count", 10)
	require.NoError(t, err)
	require.Len(t, examples.Sessions, 1)
	assert.Equal(t, 7, examples.Sessions[0].SignalTotal)
}

func TestSignalsFrustrationMemoInvalidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *DB)
		want   int
	}{
		{"append", func(t *testing.T, d *DB) {
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: 1, Role: "user", Content: "this is broken"}}))
		}, 2},
		{"replace", func(t *testing.T, d *DB) {
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session", []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "Please explain this function."}}))
		}, 0},
		{"truncate", func(t *testing.T, d *DB) {
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session", nil))
		}, 0},
		{"recreate", func(t *testing.T, d *DB) {
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
	for _, compact := range []bool{false, true} {
		name := "reopen"
		if compact {
			name = "compact"
		}
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "session", "test")
			require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "this is broken"}}))
			_, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
			require.NoError(t, err)
			entry := d.frustrationMemo.counts["session"]
			generation := d.frustrationMemo.generation
			if compact {
				_, err = d.Compact(ctx, CompactOptions{StagingDir: t.TempDir()})
			} else {
				err = d.Reopen()
			}
			require.NoError(t, err)
			d.frustrationMemo.store(generation, "session", entry.version, 7)
			assert.Empty(t, d.frustrationMemo.counts, "swap must clear counts and reject an in-flight store")
			examples, err := d.GetAnalyticsSignalSessions(ctx, AnalyticsFilter{}, "frustration_marker_count", 10)
			require.NoError(t, err)
			require.Len(t, examples.Sessions, 1)
			assert.Equal(t, 1, examples.Sessions[0].SignalTotal)
		})
	}
}

func TestSignalsFrustrationMemoCanceled(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "session", "test")
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "session", Role: "user", Content: "this is broken"}}))
	rows, err := d.signalRows(t.Context(), AnalyticsFilter{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, d.populateFrustrationMarkers(ctx, rows), context.Canceled)
	assert.Empty(t, d.frustrationMemo.counts)
}
