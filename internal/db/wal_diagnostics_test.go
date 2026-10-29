package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
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

	reader := holdDiagnosticTransaction(t, d)

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

func TestWALStallLogsOpenReadOwnersAndStacks(t *testing.T) {
	d := testDB(t)
	reader := holdDiagnosticTransaction(t, d)
	output := captureLog(t)
	var diag walDiagnostics
	for attempt := range walStallLogAttempts {
		insertSession(t, d, fmt.Sprintf("diagnostic-%d", attempt), "proj")
		d.walMaintenanceTick(t.Context(), &diag)
	}
	text := output.String()
	assert.Contains(t, text, "pages unmoved after 3 consecutive attempts")
	assert.Contains(t, text, "sqlite open reads: 1")
	assert.Contains(t, text, "sqlite wal goroutine dump:\ngoroutine ")
	assert.Contains(t, text, "[running]:")
	assert.Equal(t, 1, strings.Count(text, "sqlite wal goroutine dump:"))

	for range 2 {
		d.walMaintenanceTick(t.Context(), &diag)
	}
	text = output.String()
	assert.Equal(t, 3, strings.Count(text, "pages unmoved after"))
	assert.Equal(t, 3, strings.Count(text, "sqlite open reads: 1"))
	assert.Equal(t, 1, strings.Count(text, "sqlite wal goroutine dump:"))

	require.NoError(t, reader.Rollback())
	d.walMaintenanceTick(t.Context(), &diag)

	holdDiagnosticTransaction(t, d)
	for attempt := range walStallLogAttempts {
		insertSession(t, d, fmt.Sprintf("diagnostic-again-%d", attempt), "proj")
		d.walMaintenanceTick(t.Context(), &diag)
	}
	text = output.String()
	assert.Equal(t, 2, strings.Count(text, "sqlite wal goroutine dump:"))
}

func TestReadDiagnostics(t *testing.T) {
	for _, state := range []string{"rows", "commit", "rollback", "query error", "reopen", "exec"} {
		t.Run(state, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := testDB(t)
				insertSession(t, d, "before-read", "proj")
				output := captureLog(t)
				switch state {
				case "commit", "rollback", "rows":
					var finish func() error
					owner := "holdDiagnosticTransaction"
					switch state {
					case "rows":
						rows := holdDiagnosticRows(t, d)
						defer rows.Close()
						require.NoError(t, rows.Err())
						finish, owner = rows.Close, "holdDiagnosticRows"
					default:
						tx := holdDiagnosticTransaction(t, d)
						finish = tx.Rollback
						if state == "commit" {
							finish = tx.Commit
						}
					}
					defer func() { _ = finish() }()
					time.Sleep(3 * time.Minute)
					d.reads.logOpenReads()
					assert.Contains(t, output.String(), "sqlite open reads: 1")
					assert.Contains(t, output.String(), "age=3m0s owner=go.kenn.io/agentsview/internal/db."+owner+" (")
					require.NoError(t, finish())
				case "query error":
					var n int
					require.Error(t, d.getReader().QueryRowContext(t.Context(), "SELECT missing_column FROM sessions").Scan(&n))
				case "reopen":
					oldRows := holdDiagnosticRows(t, d)
					defer oldRows.Close()
					require.NoError(t, d.Reopen())
					newRows := holdDiagnosticRows(t, d)
					defer newRows.Close()
					d.reads.logOpenReads()
					assert.Contains(t, output.String(), "sqlite open reads: 2")
					require.NoError(t, oldRows.Err())
					require.NoError(t, newRows.Err())
					return
				case "exec":
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
						_, err := conn.ExecContext(t.Context(), "SELECT wait_for_diagnostics()")
						done <- err
					}()
					<-entered
					d.reads.logOpenReads()
					text := output.String()
					close(release)
					require.NoError(t, <-done)
					assert.Contains(t, text, "sqlite open reads: 1")
					assert.Contains(t, text, "owner=go.kenn.io/agentsview/internal/db.TestReadDiagnostics.")
				}
				output.Reset()
				d.reads.logOpenReads()
				assert.Contains(t, output.String(), "sqlite open reads: 0")
			})
		})
	}
}
