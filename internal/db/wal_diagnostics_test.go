package db

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWALStallReportedWhileReaderHoldsSnapshot(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	// A read transaction that has run a query keeps its snapshot of the WAL
	// until it ends, so pages written after it cannot be checkpointed.
	reader, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	readerOpen := true
	t.Cleanup(func() {
		if readerOpen {
			_ = reader.Rollback()
		}
	})
	var sessions int
	require.NoError(t,
		reader.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&sessions))

	var tracker walStallTracker
	var reported []bool
	var stall walStall
	for attempt := range walStallLogAttempts {
		insertSession(t, d, fmt.Sprintf("pinned-%d", attempt), "proj")
		progress, err := d.checkpointWALPassive(ctx)
		require.NoError(t, err)
		var ok bool
		stall, ok = tracker.observe(progress)
		reported = append(reported, ok)
	}
	assert.Equal(t, []bool{false, false, true}, reported)
	assert.Equal(t, 3, stall.attempts)
	assert.Positive(t, stall.unmovedPages)
	assert.GreaterOrEqual(t, stall.logPages, stall.unmovedPages)

	require.NoError(t, reader.Rollback())
	readerOpen = false

	progress, err := d.checkpointWALPassive(ctx)
	require.NoError(t, err)
	assert.Zero(t, progress.unmovedPages())
	_, ok := tracker.observe(progress)
	assert.False(t, ok)
}

func TestWALStallTrackerRestartsCountAfterDrain(t *testing.T) {
	pinned := walCheckpointProgress{logPages: 40, checkpointedPages: 10}
	drained := walCheckpointProgress{logPages: 40, checkpointedPages: 40}

	var tracker walStallTracker
	var reported []bool
	for _, progress := range []walCheckpointProgress{
		pinned, pinned, drained, pinned, pinned, pinned, pinned,
	} {
		_, ok := tracker.observe(progress)
		reported = append(reported, ok)
	}
	assert.Equal(t,
		[]bool{false, false, false, false, false, true, true}, reported)

	stall, ok := tracker.observe(pinned)
	require.True(t, ok)
	assert.Equal(t, walStall{attempts: 5, logPages: 40, unmovedPages: 30}, stall)
}

func TestWALCheckpointProgressOutsideWALModeHasNoUnmovedPages(t *testing.T) {
	// SQLite reports -1 for both counts when the database is not in WAL mode.
	progress := walCheckpointProgress{logPages: -1, checkpointedPages: -1}
	assert.Zero(t, progress.unmovedPages())
}

func TestReaderPoolWaitDelta(t *testing.T) {
	tests := []struct {
		name       string
		previous   sql.DBStats
		current    sql.DBStats
		wantWaits  int64
		wantWaited time.Duration
	}{
		{
			name:       "no new waits",
			previous:   sql.DBStats{WaitCount: 7, WaitDuration: 3 * time.Second},
			current:    sql.DBStats{WaitCount: 7, WaitDuration: 3 * time.Second},
			wantWaits:  0,
			wantWaited: 0,
		},
		{
			name:       "waits since the previous snapshot",
			previous:   sql.DBStats{WaitCount: 7, WaitDuration: 3 * time.Second},
			current:    sql.DBStats{WaitCount: 12, WaitDuration: 45 * time.Second},
			wantWaits:  5,
			wantWaited: 42 * time.Second,
		},
		{
			name:       "reopened pool restarts its counters",
			previous:   sql.DBStats{WaitCount: 7, WaitDuration: 3 * time.Second},
			current:    sql.DBStats{WaitCount: 2, WaitDuration: time.Second},
			wantWaits:  2,
			wantWaited: time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			waits, waited := readerPoolWaitDelta(tt.previous, tt.current)
			assert.Equal(t, tt.wantWaits, waits)
			assert.Equal(t, tt.wantWaited, waited)
		})
	}
}
