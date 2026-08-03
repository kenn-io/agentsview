package duckdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"

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
	if c.query != nil {
		values, err := c.querySingleRow(ctx, query)
		if err != nil {
			return c.conn.QueryRowContext(
				ctx, "SELECT ?", quackBunRowError{err: err},
			)
		}
		placeholders := strings.TrimSuffix(
			strings.Repeat("?, ", len(values)), ", ",
		)
		return c.conn.QueryRowContext(ctx, "SELECT "+placeholders, values...)
	}
	return c.conn.QueryRowContext(
		ctx, "SELECT * FROM "+quackAttachmentName+".query(?)", query,
	)
}

func (c *quackBunConn) querySingleRow(
	ctx context.Context, query string,
) ([]any, error) {
	rows, err := c.query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	values := make([]any, len(columns))
	destinations := make([]any, len(columns))
	for i := range values {
		destinations[i] = &values[i]
	}
	if err := rows.Scan(destinations...); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

type quackBunRowError struct {
	err error
}

func (e quackBunRowError) Value() (driver.Value, error) {
	return nil, e.err
}
