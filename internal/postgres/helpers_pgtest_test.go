//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

func testPGURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_PG_URL")
	if url == "" {
		t.Skip("TEST_PG_URL not set; skipping PG tests")
	}
	return url
}

func pgTableCount(
	t *testing.T, ctx context.Context, conn *sql.DB, table string,
) int {
	t.Helper()
	var count int
	row := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return count
}
