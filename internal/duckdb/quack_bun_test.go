//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/duckdb/bundialect"
)

type recordingBunConn struct {
	query string
	args  []any
}

func (c *recordingBunConn) QueryContext(
	_ context.Context, query string, args ...any,
) (*sql.Rows, error) {
	c.query = query
	c.args = append([]any(nil), args...)
	return nil, nil
}

func (*recordingBunConn) ExecContext(
	context.Context, string, ...any,
) (sql.Result, error) {
	return nil, nil
}

func (*recordingBunConn) QueryRowContext(
	context.Context, string, ...any,
) *sql.Row {
	return nil
}

func TestQuackBunResolverForwardsGeneratedSelectAsOneQueryArgument(t *testing.T) {
	conn := &recordingBunConn{}
	resolver := newQuackBunResolver(conn, nil)
	store := bun.NewDB(nil, bundialect.New(), bun.WithConnResolver(resolver))

	rows, err := store.NewSelect().Model((*bunmodel.Session)(nil)).
		Column("id").Where("project = ?", "alpha").Rows(t.Context())
	require.NoError(t, err)
	assert.Nil(t, rows)
	assert.Equal(t, "SELECT * FROM "+quackAttachmentName+".query(?)", conn.query)
	require.Len(t, conn.args, 1)
	query, ok := conn.args[0].(string)
	require.True(t, ok)
	assert.Contains(t, query, `SELECT "session"."id" FROM "sessions" AS "session"`)
	assert.Contains(t, query, `WHERE (project = 'alpha')`)
}

func TestQuackBunResolverRejectsExec(t *testing.T) {
	resolver := newQuackBunResolver(&recordingBunConn{}, nil)
	conn := resolver.ResolveConn(t.Context(), nil)

	_, err := conn.ExecContext(t.Context(), "DELETE FROM sessions")
	assert.ErrorIs(t, err, db.ErrReadOnly)
}
