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
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestQuackLoopbackAttachRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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

	var listenURI, listenURL sql.NullString
	err = server.QueryRowContext(ctx,
		`SELECT listen_uri, listen_url FROM quack_serve(?, token => ?)`,
		uri, token,
	).Scan(&listenURI, &listenURL)
	require.NoError(t, err, "start quack server")
	if listenURI.Valid && listenURI.String != "" {
		uri = listenURI.String
	}
	if listenURL.Valid {
		assert.NotContains(t, listenURL.String, token)
	}
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

	_, err = client.ExecContext(ctx, `FROM remote_db.query(?)`,
		`INSERT INTO local_seed VALUES ('seed', 43)
		 ON CONFLICT(id) DO UPDATE SET value = excluded.value`,
	)
	require.NoError(t, err, "upsert seed row through quack query")

	require.NoError(t,
		server.QueryRowContext(ctx,
			`SELECT value FROM local_seed WHERE id = ?`,
			"seed",
		).Scan(&got),
		"query upserted seed row on server",
	)
	assert.Equal(t, 43, got)

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

func TestQuackClientSyncEnsureSchemaSkipsRemoteIndexes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack-schema.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0002"

	server := openQuackMirrorServer(t, ctx, path, uri, token)
	require.NoError(t, EnsureSchema(ctx, server), "prepare server schema")

	local := newLocalDB(t)
	syncer, err := NewFromConfig(
		config.DuckDBConfig{
			URL:         uri,
			Token:       token,
			MachineName: "quack-client",
		},
		local,
		SyncOptions{},
	)
	require.NoError(t, err, "open Quack-backed sync")
	t.Cleanup(func() {
		require.NoError(t, syncer.Close(), "close Quack-backed sync")
	})

	require.NoError(t, syncer.EnsureSchema(ctx), "ensure schema through Quack")
	require.NoError(t,
		CheckSchemaCompatViaQuack(ctx, syncer.DB()),
		"check schema compatibility through Quack",
	)
	assertDuckDBIndexExists(t, server, "tool_calls", "idx_tool_calls_file_path")
}

func TestQuackClientSyncEnsureSchemaRequiresPreparedServerMetadata(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack-metadata.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0004"

	server := openQuackMirrorServer(t, ctx, path, uri, token)
	require.NoError(t, EnsureSchema(ctx, server), "prepare server schema")
	_, err := server.ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key = ?`,
		schemaVersionMetadataKey,
	)
	require.NoError(t, err, "remove server schema metadata")

	local := newLocalDB(t)
	syncer, err := NewFromConfig(
		config.DuckDBConfig{
			URL:         uri,
			Token:       token,
			MachineName: "quack-client",
		},
		local,
		SyncOptions{},
	)
	require.NoError(t, err, "open Quack-backed sync")
	t.Cleanup(func() {
		require.NoError(t, syncer.Close(), "close Quack-backed sync")
	})

	err = syncer.EnsureSchema(ctx)
	require.Error(t, err, "schema metadata should be required through Quack")
	assert.Contains(t, err.Error(), "missing "+schemaVersionMetadataKey)
	assert.NotContains(t, err.Error(), "GetStorageInfo")
}

func TestQuackClientSyncEnsureSchemaRejectsUnmigratedServerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack-unmigrated.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0005"

	server := openQuackMirrorServer(t, ctx, path, uri, token)
	require.NoError(t, EnsureSchema(ctx, server), "prepare server schema")
	recreateMessagesWithIDPrimaryKey(t, ctx, server)

	local := newLocalDB(t)
	syncer, err := NewFromConfig(
		config.DuckDBConfig{
			URL:         uri,
			Token:       token,
			MachineName: "quack-client",
		},
		local,
		SyncOptions{},
	)
	require.NoError(t, err, "open Quack-backed sync")
	t.Cleanup(func() {
		require.NoError(t, syncer.Close(), "close Quack-backed sync")
	})

	err = syncer.EnsureSchema(ctx)
	require.Error(t, err, "unmigrated server schema should be rejected through Quack")
	assert.Contains(t, err.Error(), "messages.id primary key")
	assert.NotContains(t, err.Error(), "base table")
	assert.NotContains(t, err.Error(), "GetStorageInfo")
}

func TestQuackClientSyncPushWritesThroughAttachment(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack-push.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0003"

	server := openQuackMirrorServer(t, ctx, path, uri, token)
	require.NoError(t, EnsureSchema(ctx, server), "prepare server schema")

	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	require.NoError(t, local.InsertCursorUsageEvents([]db.CursorUsageEvent{
		{
			OccurredAt:       "2026-01-10T00:03:00Z",
			Model:            "cursor-model-a",
			Kind:             "usage",
			InputTokens:      11,
			OutputTokens:     7,
			CacheWriteTokens: 5,
			CacheReadTokens:  3,
			ChargedCents:     1.25,
			CursorTokenFee:   0.75,
			UserID:           "cursor-user-a",
			UserEmail:        "cursor-a@example.invalid",
			DedupKey:         "cursor-quack-a",
		},
		{
			OccurredAt:     "2026-01-10T00:04:00Z",
			Model:          "cursor-model-b",
			Kind:           "usage",
			InputTokens:    13,
			OutputTokens:   9,
			ChargedCents:   1.50,
			CursorTokenFee: 0.50,
			UserID:         "cursor-user-b",
			UserEmail:      "cursor-b@example.invalid",
			IsHeadless:     true,
			DedupKey:       "cursor-quack-b",
		},
	}), "InsertCursorUsageEvents")
	syncer, err := NewFromConfig(
		config.DuckDBConfig{
			URL:         uri,
			Token:       token,
			MachineName: "quack-client",
		},
		local,
		SyncOptions{},
	)
	require.NoError(t, err, "open Quack-backed sync")
	t.Cleanup(func() {
		require.NoError(t, syncer.Close(), "close Quack-backed sync")
	})

	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err, "push through Quack")
	assert.Equal(t, 2, result.SessionsPushed)
	assert.Equal(t, 3, result.MessagesPushed)

	var machine string
	require.NoError(t,
		server.QueryRowContext(ctx,
			`SELECT machine FROM sessions WHERE id = ?`,
			fixture.alphaID,
		).Scan(&machine),
		"query pushed session on server",
	)
	assert.Equal(t, "quack-client", machine)
	assertDuckDBCount(t, server, "messages", 3)
	assertDuckDBCount(t, server, "cursor_usage_events", 2)
	assertDuckDBIndexExists(t, server, "tool_calls", "idx_tool_calls_file_path")

	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err, "repeat push through Quack")
	assertDuckDBCount(t, server, "sessions", 2)
	assertDuckDBCount(t, server, "messages", 3)
	assertDuckDBCount(t, server, "tool_calls", 1)
	assertDuckDBCount(t, server, "tool_result_events", 1)
	assertDuckDBCount(t, server, "cursor_usage_events", 2)

	store, err := NewQuackStore(uri, token, false)
	require.NoError(t, err, "open Quack-backed store")
	t.Cleanup(func() {
		require.NoError(t, store.Close(), "close Quack-backed store")
	})

	stats, err := store.GetStats(ctx, false, false)
	require.NoError(t, err, "read stats through Quack")
	assert.Equal(t, 2, stats.SessionCount)
	assert.Equal(t, 3, stats.MessageCount)
	assert.Equal(t, 2, stats.ProjectCount)
	assert.Equal(t, 1, stats.MachineCount)
	assert.NotNil(t, stats.EarliestSession)
}

func openQuackMirrorServer(
	t *testing.T, ctx context.Context, path, uri, token string,
) *sql.DB {
	t.Helper()
	server, err := Open(path)
	require.NoError(t, err, "open server DuckDB file")
	t.Cleanup(func() {
		require.NoError(t, server.Close(), "close server DuckDB file")
	})
	_, err = server.ExecContext(ctx, "INSTALL quack")
	require.NoError(t, err, "install quack extension")
	_, err = server.ExecContext(ctx, "LOAD quack")
	require.NoError(t, err, "load quack extension")
	_, err = server.ExecContext(ctx, `CALL quack_serve(?, token => ?)`, uri, token)
	require.NoError(t, err, "start quack server")
	t.Cleanup(func() {
		_, stopErr := server.ExecContext(context.Background(), `CALL quack_stop(?)`, uri)
		require.NoError(t, stopErr, "stop quack server")
	})
	return server
}

func assertDuckDBIndexExists(
	t *testing.T, conn *sql.DB, tableName, indexName string,
) {
	t.Helper()
	var count int
	require.NoError(t, conn.QueryRow(`
		SELECT count(*) FROM duckdb_indexes()
		WHERE table_name = ?
		  AND index_name = ?`, tableName, indexName).Scan(&count),
		"query duckdb_indexes")
	assert.Equal(t, 1, count, "%s must exist on %s", indexName, tableName)
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
