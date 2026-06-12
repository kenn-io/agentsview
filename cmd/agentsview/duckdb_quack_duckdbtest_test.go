//go:build duckdbtest && !(windows && arm64)

package main

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
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
)

func TestStartQuackServerAllowsAuthenticatedAttach(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "serve.duckdb")
	bind := "quack:127.0.0.1:" + freeQuackServePort(t)
	const token = "agentsview-quack-serve-test-token"

	server, err := sql.Open("duckdb", path)
	require.NoError(t, err)
	server.SetMaxOpenConns(1)
	server.SetMaxIdleConns(1)
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})
	_, err = server.ExecContext(ctx, "INSTALL quack")
	require.NoError(t, err)
	_, err = server.ExecContext(ctx, "LOAD quack")
	require.NoError(t, err)
	_, err = server.ExecContext(ctx,
		`CREATE TABLE local_seed (id TEXT PRIMARY KEY, value INTEGER)`,
	)
	require.NoError(t, err)
	_, err = server.ExecContext(ctx,
		`INSERT INTO local_seed VALUES ('seed', 41)`,
	)
	require.NoError(t, err)

	info, err := startQuackServer(ctx, server, bind, token, false)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, stopErr := server.ExecContext(context.Background(),
			`CALL quack_stop(?)`, bind)
		require.NoError(t, stopErr)
	})
	assert.NotEmpty(t, info.ListenURI)
	attachURI := bind
	if info.ListenURI != "" {
		attachURI = info.ListenURI
	}

	client, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	client.SetMaxOpenConns(1)
	client.SetMaxIdleConns(1)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})
	require.NoError(t, client.PingContext(ctx))
	_, err = client.ExecContext(ctx, "LOAD quack")
	require.NoError(t, err)
	badAttachSQL := fmt.Sprintf(
		`ATTACH '%s' AS bad_remote (TOKEN 'wrong-token')`,
		attachURI,
	)
	_, err = client.ExecContext(ctx, badAttachSQL)
	require.Error(t, err)
	attachSQL := fmt.Sprintf(
		`ATTACH '%s' AS remote_db (TOKEN '%s')`,
		attachURI, token,
	)
	_, err = client.ExecContext(ctx, attachSQL)
	require.NoError(t, err)

	var count int
	require.NoError(t,
		client.QueryRowContext(ctx,
			`SELECT value FROM remote_db.local_seed WHERE id = 'seed'`,
		).Scan(&count),
	)
	assert.Equal(t, 41, count)
}

func TestStartQuackServerServesAgentsviewMirror(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "serve-agentsview.duckdb")
	bind := "quack:127.0.0.1:" + freeQuackServePort(t)
	const token = "agentsview-quack-serve-test-token"

	server, err := duckdbsync.Open(path)
	require.NoError(t, err)
	server.SetMaxOpenConns(1)
	server.SetMaxIdleConns(1)
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})
	_, err = server.ExecContext(ctx, "INSTALL quack")
	require.NoError(t, err)
	_, err = server.ExecContext(ctx, "LOAD quack")
	require.NoError(t, err)
	require.NoError(t, duckdbsync.EnsureSchema(ctx, server))
	_, err = server.ExecContext(ctx,
		`INSERT INTO sessions (
			id, project, machine, agent, first_message,
			started_at, ended_at, message_count,
			user_message_count, relationship_type, created_at
		) VALUES (
			'quack-session', 'proj', 'machine', 'claude', 'hello',
			current_timestamp, current_timestamp, 1, 1, 'root',
			current_timestamp
		)`,
	)
	require.NoError(t, err)

	info, err := startQuackServer(ctx, server, bind, token, false)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, stopErr := server.ExecContext(context.Background(),
			`CALL quack_stop(?)`, bind)
		require.NoError(t, stopErr)
	})

	client, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	client.SetMaxOpenConns(1)
	client.SetMaxIdleConns(1)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})
	require.NoError(t, client.PingContext(ctx))
	_, err = client.ExecContext(ctx, "LOAD quack")
	require.NoError(t, err)
	attachURI := bind
	if info.ListenURI != "" {
		attachURI = info.ListenURI
	}
	badAttachSQL := fmt.Sprintf(
		`ATTACH '%s' AS bad_remote (TOKEN 'wrong-token')`,
		attachURI,
	)
	_, err = client.ExecContext(ctx, badAttachSQL)
	require.Error(t, err)
	attachSQL := fmt.Sprintf(
		`ATTACH '%s' AS remote_db (TOKEN '%s')`,
		attachURI, token,
	)
	_, err = client.ExecContext(ctx, attachSQL)
	require.NoError(t, err)

	var count int
	require.NoError(t,
		client.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM remote_db.main.sessions`,
		).Scan(&count),
	)
	assert.Equal(t, 1, count)
}

func freeQuackServePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, ln.Close())
	}()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	return port
}
