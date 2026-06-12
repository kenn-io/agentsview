//go:build duckdbtest && !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuackLoopbackAttachRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0001"

	server, err := sql.Open("duckdb", path)
	require.NoError(t, err, "open server DuckDB file")
	server.SetMaxOpenConns(1)
	server.SetMaxIdleConns(1)
	t.Cleanup(func() {
		require.NoError(t, server.Close(), "close server DuckDB file")
	})

	require.NoError(t, server.PingContext(ctx), "ping server DuckDB")
	require.NoError(t, server.PingContext(ctx), "ping server DuckDB")

	var version string
	require.NoError(t,
		server.QueryRowContext(ctx, "SELECT version()").Scan(&version),
		"query server DuckDB version",
	)
	t.Logf("duckdb version: %s; duckdb-go version: %s",
		version, duckDBGoModuleVersion)
	assert.NotEmpty(t, version)

	_, err = server.ExecContext(ctx, "INSTALL quack")
	require.NoError(t, err, "install quack extension")
	_, err = server.ExecContext(ctx, "LOAD quack")
	require.NoError(t, err, "load quack extension")

	_, err = server.ExecContext(ctx,
		`CREATE TABLE local_seed (id TEXT PRIMARY KEY, value INTEGER)`,
	)
	require.NoError(t, err, "create seed table")
	_, err = server.ExecContext(ctx,
		`INSERT INTO local_seed VALUES (?, ?)`,
		"seed", 41,
	)
	require.NoError(t, err, "insert seed row")

	_, err = server.ExecContext(ctx,
		`CALL quack_serve(?, token => ?)`,
		uri, token,
	)
	require.NoError(t, err, "start quack server")
	t.Cleanup(func() {
		_, stopErr := server.ExecContext(ctx, `CALL quack_stop(?)`, uri)
		require.NoError(t, stopErr, "stop quack server")
	})

	client, err := sql.Open("duckdb", "")
	require.NoError(t, err, "open client DuckDB")
	client.SetMaxOpenConns(1)
	client.SetMaxIdleConns(1)
	t.Cleanup(func() {
		require.NoError(t, client.Close(), "close client DuckDB")
	})
	require.NoError(t, client.PingContext(ctx), "ping client DuckDB")

	_, err = client.ExecContext(ctx, "LOAD quack")
	require.NoError(t, err, "load quack extension in client")

	attachSQL := fmt.Sprintf(
		`ATTACH '%s' AS remote_db (TOKEN '%s')`,
		uri, token,
	)
	_, err = client.ExecContext(ctx, attachSQL)
	require.NoError(t, err, "attach quack endpoint")

	var got int
	require.NoError(t,
		client.QueryRowContext(ctx,
			`SELECT value FROM remote_db.local_seed WHERE id = ?`,
			"seed",
		).Scan(&got),
		"query remote seed row",
	)
	assert.Equal(t, 41, got)

	_, err = client.ExecContext(ctx,
		`CREATE TABLE remote_db.remote_write
		 AS SELECT 'client'::TEXT AS id, 42::INTEGER AS value`,
	)
	require.NoError(t, err, "create remote table through attachment")

	var remoteValue int
	require.NoError(t,
		server.QueryRowContext(ctx,
			`SELECT value FROM remote_write WHERE id = ?`,
			"client",
		).Scan(&remoteValue),
		"query row written through quack attachment",
	)
	assert.Equal(t, 42, remoteValue)
}

func freeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "allocate free TCP port")
	defer func() {
		require.NoError(t, ln.Close(), "close port probe listener")
	}()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err, "parse probe listener address")
	return port
}
