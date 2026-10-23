//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json/v2"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This connector delegates every statement, row and transaction to PostgreSQL.
// It observes materialized identity rows and can lose a response only after the
// real server has committed or processed the unlock. It never simulates SQL.
type activationConnectionProbe struct {
	driver.Connector
	mu                     sync.Mutex
	rows                   map[string]int
	connections            []*activationObservedConn
	acquisitions, commits  int
	loseCommit, loseUnlock bool
}

func activationObservedPool(t *testing.T, f projectionFixture, owner bool) (*sql.DB, *activationConnectionProbe) {
	t.Helper()
	dsn := f.dsn
	if owner {
		dsn = testPGURL(t)
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = `"` + f.schema + `"`
	cfg.RuntimeParams["agentsview.tenant_id"] = f.tenant
	probe := &activationConnectionProbe{Connector: stdlib.GetConnector(*cfg), rows: map[string]int{}}
	pool := sql.OpenDB(probe)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	return pool, probe
}
func (p *activationConnectionProbe) Connect(ctx context.Context) (driver.Conn, error) {
	c, err := p.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	wrapped := &activationObservedConn{Conn: c.(*stdlib.Conn), probe: p, pid: c.(*stdlib.Conn).Conn().PgConn().PID()}
	p.mu.Lock()
	p.connections = append(p.connections, wrapped)
	p.mu.Unlock()
	return wrapped, nil
}

type activationObservedConn struct {
	*stdlib.Conn
	probe  *activationConnectionProbe
	closed bool
	pid    uint32
}

func (c *activationObservedConn) Close() error {
	c.probe.mu.Lock()
	c.closed = true
	c.probe.mu.Unlock()
	return c.Conn.Close()
}
func (c *activationObservedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &activationObservedTx{Tx: tx, probe: c.probe}, nil
}
func (c *activationObservedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, "pg_advisory_lock(") {
		c.probe.mu.Lock()
		c.probe.acquisitions++
		c.probe.mu.Unlock()
	}
	return c.Conn.ExecContext(ctx, query, args)
}
func (c *activationObservedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if strings.Contains(query, "pg_advisory_unlock(") && c.probe.loseUnlock {
		// Consume the actual result before losing its response to the caller.
		values := make([]driver.Value, len(rows.Columns()))
		err = rows.Next(values)
		closeErr := rows.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		return nil, io.ErrUnexpectedEOF
	}
	return &activationObservedRows{Rows: rows, probe: c.probe}, nil
}

type activationObservedRows struct {
	driver.Rows
	probe *activationConnectionProbe
}

func (r *activationObservedRows) Next(values []driver.Value) error {
	err := r.Rows.Next(values)
	if err != nil {
		return err
	}
	columns := r.Columns()
	kind := ""
	switch {
	case len(columns) >= 4 && slices.Equal(columns[:4], []string{"id", "agent", "machine", "raw_group_id"}):
		kind = "candidates"
	case slices.Equal(columns, []string{"group_id", "device_id", "provider"}):
		kind = "eligibility"
	case slices.Equal(columns, []string{"legacy_session_id", "group_id"}):
		kind = "previous"
	}
	if kind != "" {
		r.probe.mu.Lock()
		r.probe.rows[kind]++
		r.probe.mu.Unlock()
	}
	return nil
}

type activationObservedTx struct {
	driver.Tx
	probe *activationConnectionProbe
}

func (tx *activationObservedTx) Commit() error {
	err := tx.Tx.Commit()
	if err != nil {
		return err
	}
	tx.probe.mu.Lock()
	tx.probe.commits++
	lost := tx.probe.loseCommit
	tx.probe.mu.Unlock()
	if lost {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func TestRawActivationCommittedResponseLossNeverReplays(t *testing.T) {
	for _, boundary := range []string{"commit response", "unlock response"} {
		t.Run(boundary, func(t *testing.T) {
			f, _ := supersessionFixture(t)
			require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
			_, err := f.admin.Exec(`CREATE SEQUENCE activation_audits; CREATE FUNCTION count_activation_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.parity IS DISTINCT FROM OLD.parity THEN PERFORM nextval('activation_audits'); END IF; RETURN NEW; END $$; CREATE TRIGGER count_activation_audit AFTER UPDATE ON raw_rollout_scopes FOR EACH ROW EXECUTE FUNCTION count_activation_audit()`)
			require.NoError(t, err)
			pool, probe := activationObservedPool(t, f, true)
			probe.loseCommit = boundary == "commit response"
			probe.loseUnlock = boundary == "unlock response"
			audit, err := ActivateRawRollout(t.Context(), pool, f.schema, f.tenant, "", "", activationOptions())
			require.Error(t, err)
			assert.Equal(t, 1, probe.acquisitions, "no replay after a committed outcome")
			assert.Equal(t, 1, probe.commits, "real PostgreSQL commit completed once")
			require.Len(t, probe.connections, 1)
			assert.True(t, probe.connections[0].closed, "uncertain physical session must be disposed")
			var state string
			var revision, audits int64
			var encoded string
			require.NoError(t, f.admin.QueryRow(`SELECT state,activated_corpus_revision,parity::text FROM raw_rollout_scopes`).Scan(&state, &revision, &encoded))
			assert.Equal(t, "active", state)
			assert.Equal(t, audit.ActivatedCorpusRevision, revision)
			var persisted RawActivationAudit
			require.NoError(t, json.Unmarshal([]byte(encoded), &persisted))
			assert.Equal(t, audit, persisted)
			require.NoError(t, f.admin.QueryRow(`SELECT last_value FROM activation_audits WHERE is_called`).Scan(&audits))
			assert.EqualValues(t, 1, audits, "audit written exactly once")
			var count int
			require.NoError(t, f.admin.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions WHERE legacy_session_id='codex:portable'`).Scan(&count))
			assert.Equal(t, 1, count)
			assertActivationGuardFree(t, f)
			var freshPID int
			require.NoError(t, pool.QueryRow(`SELECT pg_backend_pid()`).Scan(&freshPID))
			require.Len(t, probe.connections, 2, "pool opens a fresh usable physical session")
			assert.NotEqualValues(t, probe.connections[0].pid, freshPID)
		})
	}
}
