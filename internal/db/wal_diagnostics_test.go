package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBulkReadWaitDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancel    bool
		wait      time.Duration
		wantLog   bool
		crossTick bool
	}{
		{"admitted wait", false, 2 * time.Second, true, false},
		{"canceled wait", true, 2 * time.Second, true, false},
		{"below threshold", true, 500 * time.Millisecond, false, false},
		{"admitted wait crosses tick", false, 2 * time.Second, true, true},
		{"canceled wait crosses tick", true, 2 * time.Second, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			var diag walDiagnostics
			synctest.Test(t, func(t *testing.T) {
				var releases []func()
				for range bulkReadSlots {
					release, err := d.acquireBulkRead(t.Context())
					require.NoError(t, err)
					releases = append(releases, release)
				}
				defer func() {
					for _, release := range releases {
						release()
					}
				}()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					release, err := d.acquireBulkRead(ctx)
					if release != nil {
						release()
					}
					done <- err
				}()
				synctest.Wait()
				time.Sleep(tc.wait)
				if tc.crossTick {
					d.walMaintenanceTick(t.Context(), &diag)
					assert.Zero(t, diag.bulkStats.WaitCount)
					assert.Zero(t, diag.bulkStats.WaitDuration)
					assert.NotContains(t, output.String(), "bulk read slots")
					output.Reset()
				}
				if tc.cancel {
					cancel()
					require.ErrorIs(t, <-done, context.Canceled)
				} else {
					releases[0]()
					releases = releases[1:]
					require.NoError(t, <-done)
				}
				assert.EqualValues(t, 1, d.bulkReads.waitCount)
				assert.Equal(t, tc.wait, d.bulkReads.waitDuration)
			})
			d.walMaintenanceTick(t.Context(), &diag)
			if tc.wantLog {
				assert.Contains(t, output.String(), fmt.Sprintf("sqlite reader pool: 1 requests waited 2s in total for one of %d bulk read slots", bulkReadSlots))
			} else {
				assert.NotContains(t, output.String(), "bulk read slots")
			}
			output.Reset()
			d.walMaintenanceTick(t.Context(), &diag)
			assert.NotContains(t, output.String(), "bulk read slots")
		})
	}
}

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

// Named callers make the owner in the warning useful even after Query returns.
func holdDiagnosticTransaction(t *testing.T, d *DB) *sql.Tx {
	t.Helper()
	tx, err := d.getReader().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var n int
	require.NoError(t, tx.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sessions").Scan(&n))
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func holdDiagnosticRows(t *testing.T, d *DB) *sql.Rows {
	t.Helper()
	rows, err := d.rawReader().QueryContext(t.Context(), "SELECT id FROM sessions")
	require.NoError(t, err)
	require.True(t, rows.Next())
	t.Cleanup(func() { _ = rows.Close() })
	return rows
}

func holdDiagnosticRow(t *testing.T, d *DB) *sql.Row {
	t.Helper()
	return d.getReader().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sessions")
}

func captureDiagnosticLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &output
}

func TestWALStallLogsOpenReadOwnersAndStacks(t *testing.T) {
	output := captureDiagnosticLog(t)
	synctest.Test(t, func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "before-read", "proj")
		tx := holdDiagnosticTransaction(t, d)
		rows := holdDiagnosticRows(t, d)
		defer rows.Close()
		row := holdDiagnosticRow(t, d)
		defer func() { var n int; _ = row.Scan(&n) }()
		var diag walDiagnostics
		for attempt := range walStallLogAttempts {
			time.Sleep(time.Minute)
			insertSession(t, d, fmt.Sprintf("diagnostic-%d", attempt), "proj")
			d.walMaintenanceTick(t.Context(), &diag)
		}
		text := output.String()
		assert.Contains(t, text, "pages unmoved after 3 consecutive attempts")
		assert.Contains(t, text, "sqlite open reads: 3")
		for _, owner := range []string{"holdDiagnosticTransaction", "holdDiagnosticRows", "holdDiagnosticRow"} {
			assert.Contains(t, text, "age=3m0s owner=go.kenn.io/agentsview/internal/db."+owner+" (")
		}
		assert.Contains(t, text, "sqlite wal goroutine dump:\ngoroutine ")
		assert.Contains(t, text, "runtime/pprof.writeGoroutineStacks")

		require.NoError(t, rows.Close())
		require.NoError(t, rows.Err())
		var n int
		require.NoError(t, row.Scan(&n))
		output.Reset()
		d.walMaintenanceTick(t.Context(), &diag)
		text = output.String()
		assert.Contains(t, text, "sqlite open reads: 1")
		assert.Contains(t, text, "owner=go.kenn.io/agentsview/internal/db.holdDiagnosticTransaction (")
		assert.NotContains(t, text, "owner=go.kenn.io/agentsview/internal/db.holdDiagnosticRows (")
		assert.NotContains(t, text, "owner=go.kenn.io/agentsview/internal/db.holdDiagnosticRow (")
		assert.Contains(t, text, "sqlite wal goroutine dump:\ngoroutine ")
		require.NoError(t, tx.Rollback())
	})
}

func TestReadDiagnosticsReleaseAndPreparedReuse(t *testing.T) {
	for _, end := range []string{"close", "eof", "scan", "commit", "rollback", "cancel", "failed query", "failed begin", "prepared"} {
		t.Run(end, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "pinned", "proj")
			holdDiagnosticTransaction(t, d)
			output := captureDiagnosticLog(t)
			ctx := t.Context()
			switch end {
			case "commit", "rollback":
				tx, err := d.getReader().BeginTx(ctx, nil)
				require.NoError(t, err)
				if end == "commit" {
					require.NoError(t, tx.Commit())
				} else {
					require.NoError(t, tx.Rollback())
				}
			case "failed begin":
				conn, err := d.getReader().Conn(ctx)
				require.NoError(t, err)
				defer conn.Close()
				tx, err := conn.BeginTx(ctx, nil)
				require.NoError(t, err)
				_, err = conn.BeginTx(ctx, nil)
				require.Error(t, err)
				require.NoError(t, tx.Rollback())
			case "failed query":
				var n int
				err := d.getReader().QueryRowContext(ctx, "SELECT missing_column FROM sessions").Scan(&n)
				require.Error(t, err)
			case "scan":
				var n int
				require.NoError(t, holdDiagnosticRow(t, d).Scan(&n))
			case "prepared":
				stmt, err := d.rawReader().PrepareContext(ctx, "SELECT id FROM sessions")
				require.NoError(t, err)
				defer stmt.Close()
				for range 2 {
					rows, err := stmt.QueryContext(ctx)
					require.NoError(t, err)
					defer rows.Close()
					d.reads.logOpenReads()
					assert.Contains(t, output.String(), "sqlite open reads: 2")
					assert.Contains(t, output.String(), "owner=go.kenn.io/agentsview/internal/db.TestReadDiagnosticsReleaseAndPreparedReuse.func1 (")
					require.NoError(t, rows.Close())
					require.NoError(t, rows.Err())
					output.Reset()
				}
			default:
				queryCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				rows, err := d.getReader().QueryContext(queryCtx, "SELECT id FROM sessions")
				require.NoError(t, err)
				defer rows.Close()
				switch end {
				case "close":
					require.NoError(t, rows.Close())
				case "eof":
					for rows.Next() {
					}
					require.NoError(t, rows.Err())
				case "cancel":
					cancel()
					for rows.Next() {
					}
					require.ErrorIs(t, rows.Err(), context.Canceled)
				}
			}
			// Keep another snapshot pinned so each warning still fires after
			// the tested reader has completed. Idle pinned Conns are excluded.
			var diag walDiagnostics
			for attempt := range walStallLogAttempts {
				insertSession(t, d, fmt.Sprintf("release-%d", attempt), "proj")
				d.walMaintenanceTick(ctx, &diag)
			}
			assert.Contains(t, output.String(), "sqlite open reads: 1")
			assert.NotContains(t, output.String(), "owner=go.kenn.io/agentsview/internal/db.TestReadDiagnosticsReleaseAndPreparedReuse.func1 (")
		})
	}
}

func TestReadDiagnosticsAcrossReopenAndReadOnly(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "before-reopen", "proj")
	oldRows := holdDiagnosticRows(t, d)
	defer oldRows.Close()
	require.NoError(t, d.Reopen())
	newRows := holdDiagnosticRows(t, d)
	defer newRows.Close()
	output := captureDiagnosticLog(t)
	d.reads.logOpenReads()
	assert.Contains(t, output.String(), "sqlite open reads: 2")
	require.NoError(t, oldRows.Close())
	require.NoError(t, oldRows.Err())
	output.Reset()
	d.reads.logOpenReads()
	assert.Contains(t, output.String(), "sqlite open reads: 1")
	require.NoError(t, newRows.Close())
	require.NoError(t, newRows.Err())
	idle, err := d.getReader().Conn(t.Context())
	require.NoError(t, err)
	defer idle.Close()
	var n int
	require.NoError(t, idle.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sessions").Scan(&n))
	output.Reset()
	d.reads.logOpenReads()
	assert.Contains(t, output.String(), "sqlite open reads: 0")

	readOnly, err := OpenReadOnly(t.Context(), d.path)
	require.NoError(t, err)
	defer readOnly.Close()
	rows := holdDiagnosticRows(t, readOnly)
	defer rows.Close()
	output.Reset()
	readOnly.reads.logOpenReads()
	assert.Contains(t, output.String(), "sqlite open reads: 1")
	require.NoError(t, rows.Close())
	require.NoError(t, rows.Err())
}

func TestReadDiagnosticsIncludesExecutingQuery(t *testing.T) {
	d := testDB(t)
	conn, err := d.getReader().Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	require.NoError(t, conn.Raw(func(raw any) error {
		return raw.(interface{ RegisterFunc(string, any, bool) error }).RegisterFunc("wait_for_diagnostics", func() int {
			close(entered)
			<-release
			return 1
		}, false)
	}))
	done := make(chan error, 1)
	go func() {
		var value int
		done <- conn.QueryRowContext(t.Context(), "SELECT wait_for_diagnostics()").Scan(&value)
	}()
	<-entered
	output := captureDiagnosticLog(t)
	d.reads.logOpenReads()
	text := output.String()
	close(release)
	require.NoError(t, <-done)
	assert.Contains(t, text, "sqlite open reads: 1")
	assert.Contains(t, text, "owner=go.kenn.io/agentsview/internal/db.TestReadDiagnosticsIncludesExecutingQuery.func2 (")
	output.Reset()
	d.reads.logOpenReads()
	assert.Contains(t, output.String(), "sqlite open reads: 0")
}
