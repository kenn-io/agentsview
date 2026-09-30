package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/ledger"
)

type ledgerQueryProbe struct {
	query string
	args  []driver.NamedValue
}

type ledgerQueryProbeConnector struct {
	probe *ledgerQueryProbe
}

func (c ledgerQueryProbeConnector) Connect(context.Context) (driver.Conn, error) {
	return ledgerQueryProbeConn(c), nil
}

func (ledgerQueryProbeConnector) Driver() driver.Driver {
	return ledgerQueryProbeDriver{}
}

type ledgerQueryProbeDriver struct{}

func (ledgerQueryProbeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("ledger query probe requires its connector")
}

type ledgerQueryProbeConn struct {
	probe *ledgerQueryProbe
}

func (ledgerQueryProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not implemented")
}

func (ledgerQueryProbeConn) Close() error { return nil }

func (ledgerQueryProbeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not implemented")
}

func (c ledgerQueryProbeConn) QueryContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	c.probe.query = query
	c.probe.args = append([]driver.NamedValue(nil), args...)
	return ledgerQueryProbeRows{}, nil
}

type ledgerQueryProbeRows struct{}

func (ledgerQueryProbeRows) Columns() []string { return []string{"event_json"} }

func (ledgerQueryProbeRows) Close() error { return nil }

func (ledgerQueryProbeRows) Next([]driver.Value) error { return io.EOF }

func TestQueryLedgerSinceMatchesStorageMicrosecondPrecision(t *testing.T) {
	probe := &ledgerQueryProbe{}
	pg := sql.OpenDB(ledgerQueryProbeConnector{probe: probe})
	t.Cleanup(func() { _ = pg.Close() })
	store := &Store{pg: pg}

	since := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	want := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	_, err := store.queryLedgerZone(t.Context(), "precision", ledger.Query{Since: since}, 10)
	require.NoError(t, err)
	require.Len(t, probe.args, 3)

	got, ok := probe.args[1].Value.(time.Time)
	require.True(t, ok, "ledger since cutoff is bound as a timestamp")
	assert.Equal(t, want, got)
}
