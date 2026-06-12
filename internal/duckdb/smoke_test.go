//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const duckDBGoModuleVersion = "v2.10503.1"

func TestLocalFileSmoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentsview.duckdb")

	db, err := sql.Open("duckdb", path)
	require.NoError(t, err, "open DuckDB file")
	t.Cleanup(func() {
		require.NoError(t, db.Close(), "close DuckDB file")
	})

	ctx := context.Background()
	require.NoError(t, db.PingContext(ctx), "ping DuckDB")

	var version string
	require.NoError(t,
		db.QueryRowContext(ctx, "SELECT version()").Scan(&version),
		"query DuckDB version",
	)
	t.Logf("duckdb version: %s; duckdb-go version: %s",
		version, duckDBGoModuleVersion)
	assert.NotEmpty(t, version)

	_, err = db.ExecContext(ctx,
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, message_count INTEGER)`,
	)
	require.NoError(t, err, "create table")

	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions VALUES (?, ?)`,
		"duckdb-local", 3,
	)
	require.NoError(t, err, "insert row")

	require.NoError(t, db.Close(), "close first connection")

	reopened, err := sql.Open("duckdb", path)
	require.NoError(t, err, "reopen DuckDB file")
	t.Cleanup(func() {
		require.NoError(t, reopened.Close(), "close reopened DuckDB file")
	})

	var count int
	require.NoError(t,
		reopened.QueryRowContext(ctx,
			`SELECT message_count FROM sessions WHERE id = ?`,
			"duckdb-local",
		).Scan(&count),
		"query persisted row",
	)
	assert.Equal(t, 3, count)
}
