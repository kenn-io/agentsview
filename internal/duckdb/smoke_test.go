//go:build !(windows && arm64)

package duckdb

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const duckDBGoModuleVersion = "v2.10504.0"

func TestLocalFileSmoke(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "agentsview.duckdb")

	db, err := sql.Open("duckdb", path)
	require.NoError(err, "open DuckDB file")
	t.Cleanup(func() {
		require.NoError(db.Close(), "close DuckDB file")
	})

	ctx := t.Context()
	require.NoError(db.PingContext(ctx), "ping DuckDB")

	var version string
	require.NoError(db.QueryRowContext(ctx, "SELECT version()").Scan(&version),
		"query DuckDB version",
	)
	t.Logf("duckdb version: %s; duckdb-go version: %s",
		version, duckDBGoModuleVersion)
	assert.NotEmpty(version)

	_, err = db.ExecContext(ctx,
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, message_count INTEGER)`,
	)
	require.NoError(err, "create table")

	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions VALUES (?, ?)`,
		"duckdb-local", 3,
	)
	require.NoError(err, "insert row")

	require.NoError(db.Close(), "close first connection")

	reopened, err := sql.Open("duckdb", path)
	require.NoError(err, "reopen DuckDB file")
	t.Cleanup(func() {
		require.NoError(reopened.Close(), "close reopened DuckDB file")
	})

	var count int
	require.NoError(reopened.QueryRowContext(ctx,
		`SELECT message_count FROM sessions WHERE id = ?`,
		"duckdb-local",
	).Scan(&count),
		"query persisted row",
	)
	assert.Equal(3, count)
}
