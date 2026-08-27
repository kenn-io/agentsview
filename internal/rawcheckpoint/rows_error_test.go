package rawcheckpoint

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/rawsync"
)

var errInjectedRowIteration = errors.New("injected row iteration failure")

const rowErrorDriverName = "rawcheckpoint_row_error"

var (
	rowErrorDriverOnce sync.Once
	rowErrorSequence   atomic.Int64
	rowErrorScenarios  sync.Map
)

type rowErrorScenario struct {
	mode               string
	reservationDeletes atomic.Int64
	objectUpdates      atomic.Int64
}

type rowErrorDriver struct{}

func (rowErrorDriver) Open(name string) (driver.Conn, error) {
	value, ok := rowErrorScenarios.Load(name)
	if !ok {
		return nil, fmt.Errorf("unknown row error scenario %q", name)
	}
	return &rowErrorConn{scenario: value.(*rowErrorScenario)}, nil
}

type rowErrorConn struct {
	scenario *rowErrorScenario
}

func (*rowErrorConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*rowErrorConn) Close() error { return nil }

func (*rowErrorConn) Begin() (driver.Tx, error) {
	return nil, errors.New("driver transactions are not supported")
}

func (c *rowErrorConn) QueryContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "SELECT sha256, length, state FROM outbox_objects"):
		if c.scenario.mode == "recovery_objects" {
			return &rowErrorRows{
				columns: []string{"sha256", "length", "state"},
				values:  [][]driver.Value{{validCheckpointDigest(1), int64(1), "live"}},
				err:     errInjectedRowIteration,
			}, nil
		}
		return &rowErrorRows{columns: []string{"sha256", "length", "state"}}, nil
	case strings.Contains(query, "FROM outbox_reservations"):
		if c.scenario.mode == "recovery_reservations" {
			return &rowErrorRows{
				columns: []string{"provider", "configured_root_id", "source_key"},
				values:  [][]driver.Value{{"claude", "root-a", "source-a"}},
				err:     errInjectedRowIteration,
			}, nil
		}
		return &rowErrorRows{
			columns: []string{"provider", "configured_root_id", "source_key"},
		}, nil
	case strings.Contains(query, "WITH RECURSIVE suffix"):
		return &rowErrorRows{
			columns: []string{"capture_id"},
			values:  [][]driver.Value{{"capture-a"}},
			err:     errInjectedRowIteration,
		}, nil
	case strings.Contains(query, "FROM outbox_entry_objects"):
		return &rowErrorRows{
			columns: []string{"sha256", "length", "count"},
			values:  [][]driver.Value{{validCheckpointDigest(1), int64(1), int64(1)}},
			err:     errInjectedRowIteration,
		}, nil
	case strings.Contains(query, "state = 'garbage_pending'"):
		if c.scenario.mode == "garbage_objects" {
			return &rowErrorRows{
				columns: []string{"sha256", "length"},
				values:  [][]driver.Value{{validCheckpointDigest(1), int64(1)}},
				err:     errInjectedRowIteration,
			}, nil
		}
		return &rowErrorRows{columns: []string{"sha256", "length"}}, nil
	default:
		return nil, fmt.Errorf("unexpected query for %s: %s", c.scenario.mode, query)
	}
}

func (c *rowErrorConn) ExecContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Result, error) {
	switch {
	case query == "BEGIN IMMEDIATE", query == "COMMIT", query == "ROLLBACK":
	case strings.Contains(query, "INSERT INTO raw_coverage_failures"):
	case strings.Contains(query, "INSERT INTO raw_coverage"):
	case strings.Contains(query, "DELETE FROM outbox_reservations"):
		c.scenario.reservationDeletes.Add(1)
	case strings.Contains(query, "UPDATE outbox_objects"):
		c.scenario.objectUpdates.Add(1)
	default:
		return nil, fmt.Errorf("unexpected exec for %s: %s", c.scenario.mode, query)
	}
	return driver.RowsAffected(1), nil
}

type rowErrorRows struct {
	columns []string
	values  [][]driver.Value
	err     error
	index   int
}

func (r *rowErrorRows) Columns() []string { return r.columns }

func (*rowErrorRows) Close() error { return nil }

func (r *rowErrorRows) Next(dest []driver.Value) error {
	if r.index < len(r.values) {
		copy(dest, r.values[r.index])
		r.index++
		return nil
	}
	if r.err != nil {
		return r.err
	}
	return io.EOF
}

func openRowErrorDB(t *testing.T, mode string) (*sql.DB, *rowErrorScenario) {
	t.Helper()
	rowErrorDriverOnce.Do(func() { sql.Register(rowErrorDriverName, rowErrorDriver{}) })
	scenario := &rowErrorScenario{mode: mode}
	name := fmt.Sprintf("%s-%d", mode, rowErrorSequence.Add(1))
	rowErrorScenarios.Store(name, scenario)
	t.Cleanup(func() { rowErrorScenarios.Delete(name) })
	db, err := sql.Open(rowErrorDriverName, name)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db, scenario
}

func TestRecoverStopsBeforeSpoolSweepOnObjectIterationError(t *testing.T) {
	db, _ := openRowErrorDB(t, "recovery_objects")
	store := &Store{
		db: db, spoolDir: t.TempDir(), now: func() time.Time { return time.Unix(0, 0) },
	}
	known := rawsync.ObjectRef{SHA256: validCheckpointDigest(1), Length: 1}
	omitted := rawsync.ObjectRef{SHA256: validCheckpointDigest(2), Length: 1}
	for _, ref := range []rawsync.ObjectRef{known, omitted} {
		path := store.ObjectPath(ref)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte{1}, 0o600))
	}

	_, err := store.Recover(t.Context())

	assert.ErrorIs(t, err, errInjectedRowIteration)
	assert.FileExists(t, store.ObjectPath(omitted))
}

func TestRecoverPreservesReservationsOnIterationError(t *testing.T) {
	db, scenario := openRowErrorDB(t, "recovery_reservations")
	store := &Store{
		db: db, spoolDir: t.TempDir(), now: func() time.Time { return time.Unix(0, 0) },
	}

	_, err := store.Recover(t.Context())

	assert.ErrorIs(t, err, errInjectedRowIteration)
	assert.Zero(t, scenario.reservationDeletes.Load())
}

func TestInvalidGenerationSuffixRejectsIterationError(t *testing.T) {
	db, _ := openRowErrorDB(t, "invalid_suffix")
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	_, err = invalidGenerationSuffixConn(t.Context(), conn, []rawsync.ObjectRef{{
		SHA256: validCheckpointDigest(1), Length: 1,
	}})

	assert.ErrorIs(t, err, errInjectedRowIteration)
}

func TestReleaseGenerationObjectsStopsBeforeUpdatesOnIterationError(t *testing.T) {
	db, scenario := openRowErrorDB(t, "release_objects")
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	err = releaseGenerationObjectsConn(t.Context(), conn, "capture-a")

	assert.ErrorIs(t, err, errInjectedRowIteration)
	assert.Zero(t, scenario.objectUpdates.Load())
}

func TestCollectGarbageStopsBeforeFileRemovalOnIterationError(t *testing.T) {
	db, _ := openRowErrorDB(t, "garbage_objects")
	store := &Store{db: db, spoolDir: t.TempDir()}
	ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(1), Length: 1}
	path := store.ObjectPath(ref)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte{1}, 0o600))

	_, err := store.CollectGarbage(t.Context())

	assert.ErrorIs(t, err, errInjectedRowIteration)
	assert.FileExists(t, path)
}
