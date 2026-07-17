//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const guardTestFernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="

func newCodexGuardDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := openDuckDB("")
	require.NoError(t, err, "open in-memory duckdb")
	t.Cleanup(func() { require.NoError(t, conn.Close(), "close duckdb") })
	require.NoError(t, EnsureSchema(context.Background(), conn), "ensure schema")
	return conn
}

func insertGuardSession(t *testing.T, conn *sql.DB, id string, dataVersion int) {
	t.Helper()
	_, err := conn.ExecContext(context.Background(), `
		INSERT INTO sessions (
			id, project, machine, agent, relationship_type, data_version
		) VALUES (?, 'project', 'machine', 'codex', 'subagent', ?)`,
		id, dataVersion,
	)
	require.NoError(t, err, "insert session %s", id)
}

func TestCodexGuardedQuackReadSQLPassesCleanMirror(t *testing.T) {
	conn := newCodexGuardDB(t)
	ctx := context.Background()
	insertGuardSession(t, conn, "codex-b", codexEncryptedPayloadDataVersion)
	insertGuardSession(t, conn, "codex-a", codexEncryptedPayloadDataVersion+1)
	insertGuardSession(t, conn, "codex-c", codexEncryptedPayloadDataVersion)

	rendered, err := duckSQLWithArgs(
		`SELECT id FROM sessions WHERE agent = ? ORDER BY id DESC`, "codex",
	)
	require.NoError(t, err, "render read")

	rows, err := conn.QueryContext(ctx, codexGuardedQuackReadSQL(rendered))
	require.NoError(t, err, "gated read against a current mirror")
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id), "scan id")
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err(), "iterate gated read")
	assert.Equal(t, []string{"codex-c", "codex-b", "codex-a"}, ids,
		"the guard wrapper must preserve the inner ORDER BY")
}

func TestCodexGuardedQuackReadSQLFailsClosedOnLegacyMirror(t *testing.T) {
	conn := newCodexGuardDB(t)
	ctx := context.Background()
	insertGuardSession(t, conn, "codex-old", codexEncryptedPayloadDataVersion-1)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, ordinal, role, content, content_length
		) VALUES (1, 'codex-old', 0, 'user', ?, ?)`,
		guardTestFernet, len(guardTestFernet),
	)
	require.NoError(t, err, "insert legacy ciphertext message")

	queries := map[string]string{
		"matching rows": `SELECT content FROM messages ORDER BY ordinal`,
		"empty result":  `SELECT id FROM sessions WHERE id = 'absent'`,
		"aggregate":     `SELECT COUNT(*) FROM sessions`,
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			rows, err := conn.QueryContext(ctx, codexGuardedQuackReadSQL(query))
			if err == nil {
				defer rows.Close()
				for rows.Next() {
					t.Fatal("gated read must not yield rows on a legacy mirror")
				}
				err = rows.Err()
			}
			require.Error(t, err, "gated read must fail on a legacy mirror")
			mapped := mapCodexDuckDBGuardError(err)
			require.ErrorIs(t, mapped, ErrCodexEncryptedPayloadRepairRequired,
				"guard failures must map back to the repair sentinel")
		})
	}
}

func TestMapCodexDuckDBGuardErrorPassesThroughUnrelatedErrors(t *testing.T) {
	assert.NoError(t, mapCodexDuckDBGuardError(nil))
	unrelated := errors.New("connection reset by peer")
	assert.Same(t, unrelated, mapCodexDuckDBGuardError(unrelated),
		"unrelated errors must pass through unchanged")
}

// The lazy-error fake defers the guard failure until row iteration, the way
// database/sql permits drivers to, so the test proves the sentinel contract
// holds beyond query-time errors.
type lazyGuardErrDriver struct{}

type lazyGuardErrConn struct{}

type lazyGuardErrRows struct{ delivered bool }

var lazyGuardErrRegisterOnce sync.Once

func (lazyGuardErrDriver) Open(string) (driver.Conn, error) {
	return lazyGuardErrConn{}, nil
}

func (lazyGuardErrConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (lazyGuardErrConn) Close() error { return nil }

func (lazyGuardErrConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin not implemented")
}

func (lazyGuardErrConn) QueryContext(
	context.Context, string, []driver.NamedValue,
) (driver.Rows, error) {
	return &lazyGuardErrRows{}, nil
}

func (r *lazyGuardErrRows) Columns() []string { return []string{"value"} }

func (r *lazyGuardErrRows) Close() error { return nil }

func (r *lazyGuardErrRows) Next([]driver.Value) error {
	if r.delivered {
		return io.EOF
	}
	r.delivered = true
	return errors.New(
		"Invalid Input Error: " + codexEncryptedPayloadGuardMessage,
	)
}

func TestGuardMappedRowsRestoreSentinelOnIterationErrors(t *testing.T) {
	lazyGuardErrRegisterOnce.Do(func() {
		sql.Register("agentsview_lazy_guard_err", lazyGuardErrDriver{})
	})
	conn, err := sql.Open("agentsview_lazy_guard_err", t.Name())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	raw, err := conn.QueryContext(context.Background(), "SELECT 1")
	require.NoError(t, err, "the fake driver defers the failure to iteration")
	rows := codexGuardMappedRows{Rows: raw}
	defer rows.Close()
	require.False(t, rows.Next(), "the deferred failure ends iteration")
	require.ErrorIs(t, rows.Err(), ErrCodexEncryptedPayloadRepairRequired,
		"iteration-time guard failures must map back to the repair sentinel")
}
