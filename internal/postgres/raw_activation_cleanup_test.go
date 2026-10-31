package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A lost acquisition/unlock response can leave a live driver session holding
// rollout. Unlike a dead-backend test, this catches replacing physical disposal
// with Conn.Close, which would return the held guard to the pool.
func TestRawActivationUncertainGuardDiscardsPhysicalConnection(t *testing.T) {
	for _, acquisitionFails := range []bool{true, false} {
		t.Run(map[bool]string{true: "acquisition", false: "unlock"}[acquisitionFails], func(t *testing.T) {
			probe := &activationCleanupConnector{acquisitionFails: acquisitionFails}
			database := sql.OpenDB(probe)
			defer database.Close()
			database.SetMaxOpenConns(1)
			_, err := ActivateRawRollout(t.Context(), database, "tenant_schema", "tenant", "", "", activationOptionsForCleanup())
			if acquisitionFails {
				require.ErrorContains(t, err, "acquisition response lost")
			} else {
				require.ErrorContains(t, err, "session guard cleanup failed")
			}
			require.Len(t, probe.connections, 1)
			original := probe.connections[0]
			assert.True(t, original.closed, "the driver connection must be closed, not pooled")
			assert.False(t, original.guard, "physical close releases the uncertain guard")
			assert.Equal(t, !acquisitionFails, original.rolledBack)
			fresh, err := database.Conn(t.Context())
			require.NoError(t, err)
			defer fresh.Close()
			require.Len(t, probe.connections, 2, "pool reuse must open another physical session")
			assert.NotSame(t, original, probe.connections[1])
			assert.False(t, probe.connections[1].guard)
		})
	}
}

func activationOptionsForCleanup() RawActivationOptions {
	return RawActivationOptions{Version: "parser-1", Embeddings: RawEmbeddingsDisabled}
}

type activationCleanupConnector struct {
	acquisitionFails bool
	connections      []*activationCleanupConn
}

func (p *activationCleanupConnector) Connect(context.Context) (driver.Conn, error) {
	c := &activationCleanupConn{acquisitionFails: p.acquisitionFails}
	p.connections = append(p.connections, c)
	return c, nil
}
func (*activationCleanupConnector) Driver() driver.Driver { return activationCleanupDriver{} }

type activationCleanupDriver struct{}

func (activationCleanupDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("connector required")
}

type activationCleanupConn struct {
	acquisitionFails, guard, closed, rolledBack bool
}

func (*activationCleanupConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *activationCleanupConn) Close() error { c.closed = true; c.guard = false; return nil }
func (c *activationCleanupConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *activationCleanupConn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	if options.ReadOnly || options.Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) {
		return nil, errors.New("activation did not request RR read/write")
	}
	return &activationCleanupTx{c}, nil
}

func (c *activationCleanupConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	switch query {
	case `SELECT pg_advisory_lock(hashtextextended($1,0))`:
		if len(args) != 1 || args[0].Value != "agentsview-rollout:tenant_schema" {
			return nil, errors.New("wrong guard key")
		}
		c.guard = true
		if c.acquisitionFails {
			return nil, errors.New("acquisition response lost")
		}
		return driver.RowsAffected(1), nil
	case `SELECT set_config('search_path',$1,true),set_config('agentsview.tenant_id',$2,true)`:
		if len(args) != 2 || args[0].Value != `"tenant_schema"` || args[1].Value != "tenant" {
			return nil, errors.New("wrong binding")
		}
		return driver.RowsAffected(1), nil
	default:
		return nil, errors.New("unexpected activation statement: " + query)
	}
}

func (*activationCleanupConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.HasPrefix(query, `SELECT n.nspowner=`) {
		return nil, errors.New("owner query unavailable")
	}
	if query == `SELECT pg_advisory_unlock(hashtextextended($1,0))` {
		return nil, errors.New("unlock response lost")
	}
	return nil, errors.New("unexpected activation query: " + query)
}

type activationCleanupTx struct{ conn *activationCleanupConn }

func (*activationCleanupTx) Commit() error      { return errors.New("unexpected commit") }
func (tx *activationCleanupTx) Rollback() error { tx.conn.rolledBack = true; return nil }
