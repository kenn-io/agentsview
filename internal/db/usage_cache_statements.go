package db

import (
	"context"
	"database/sql"
	"errors"
)

// usageStatementCache prepares each distinct statement once per usage cache
// transaction. Install loops run the same few statements once per session
// or rollup row, and SQLite spent longer preparing each call than running it.
type usageStatementCache struct {
	conn  *sql.Conn
	stmts map[string]*sql.Stmt
}

func newUsageStatementCache(conn *sql.Conn) *usageStatementCache {
	return &usageStatementCache{conn: conn, stmts: make(map[string]*sql.Stmt)}
}

func (c *usageStatementCache) prepared(ctx context.Context, query string) (*sql.Stmt, error) {
	if stmt := c.stmts[query]; stmt != nil {
		return stmt, nil
	}
	stmt, err := c.conn.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	c.stmts[query] = stmt
	return stmt, nil
}

func (c *usageStatementCache) ExecContext(
	ctx context.Context, query string, args ...any,
) (sql.Result, error) {
	stmt, err := c.prepared(ctx, query) //nolint:kennlint // sqlclosecheck: Close closes every cached statement.
	if err != nil {
		return nil, err
	}
	return stmt.ExecContext(ctx, args...)
}

func (c *usageStatementCache) QueryRowContext(
	ctx context.Context, query string, args ...any,
) *sql.Row {
	stmt, err := c.prepared(ctx, query) //nolint:kennlint // sqlclosecheck: Close closes every cached statement.
	if err != nil {
		// The unprepared call fails the same way and carries the error to Scan.
		return c.conn.QueryRowContext(ctx, query, args...)
	}
	return stmt.QueryRowContext(ctx, args...)
}

func (c *usageStatementCache) Close() error {
	var errs []error
	for _, stmt := range c.stmts {
		errs = append(errs, stmt.Close())
	}
	clear(c.stmts)
	return errors.Join(errs...)
}
