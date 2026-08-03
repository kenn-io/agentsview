package duckdb

import (
	"context"
	"database/sql"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
)

type quackBunQuery func(context.Context, string) (*sql.Rows, error)

type quackBunResolver struct {
	conn *quackBunConn
}

var _ bun.ConnResolver = (*quackBunResolver)(nil)

func newQuackBunResolver(
	conn bun.IConn, query quackBunQuery,
) *quackBunResolver {
	return &quackBunResolver{conn: &quackBunConn{conn: conn, query: query}}
}

func (r *quackBunResolver) ResolveConn(
	context.Context, bun.Query,
) bun.IConn {
	return r.conn
}

func (*quackBunResolver) Close() error { return nil }

type quackBunConn struct {
	conn  bun.IConn
	query quackBunQuery
}

var _ bun.IConn = (*quackBunConn)(nil)

func (c *quackBunConn) QueryContext(
	ctx context.Context, query string, _ ...any,
) (*sql.Rows, error) {
	if c.query != nil {
		return c.query(ctx, query)
	}
	return c.conn.QueryContext(
		ctx, "SELECT * FROM "+quackAttachmentName+".query(?)", query,
	)
}

func (*quackBunConn) ExecContext(
	context.Context, string, ...any,
) (sql.Result, error) {
	return nil, db.ErrReadOnly
}

func (c *quackBunConn) QueryRowContext(
	ctx context.Context, query string, _ ...any,
) *sql.Row {
	return c.conn.QueryRowContext(
		ctx, "SELECT * FROM "+quackAttachmentName+".query(?)", query,
	)
}
