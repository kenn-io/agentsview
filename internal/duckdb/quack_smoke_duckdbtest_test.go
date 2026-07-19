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

func TestQuackStoreReattachesAfterServerRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack-reattach.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0009"

	server := openQuackMirrorServer(t, ctx, path, uri, token)
	_, err := server.ExecContext(ctx,
		`CREATE TABLE stale_connection_test (
			id TEXT PRIMARY KEY,
			value INTEGER
		)`,
	)
	require.NoError(t, err, "create stale connection table")
	_, err = server.ExecContext(ctx,
		`INSERT INTO stale_connection_test VALUES (?, ?)`,
		"seed", 41,
	)
	require.NoError(t, err, "insert seed row")

	store, err := NewQuackStore(uri, token, false)
	require.NoError(t, err, "open quack store")
	t.Cleanup(func() {
		require.NoError(t, store.Close(), "close quack store")
	})
	var got int
	require.NoError(t,
		store.queryRowContext(ctx,
			`SELECT value FROM stale_connection_test WHERE id = ?`,
			"seed",
		).Scan(&got),
		"query before server restart",
	)
	assert.Equal(t, 41, got)

	_, err = server.ExecContext(ctx, `CALL quack_stop(?)`, uri)
	require.NoError(t, err, "stop quack server")
	_, err = server.ExecContext(ctx, `CALL quack_serve(?, token => ?)`, uri, token)
	require.NoError(t, err, "restart quack server")

	require.NoError(t,
		store.queryRowContext(ctx,
			`SELECT value FROM stale_connection_test WHERE id = ?`,
			"seed",
		).Scan(&got),
		"query after server restart",
	)
	assert.Equal(t, 41, got)
}

func TestQuackStoreReattachesAfterFailedReattach(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack-reattach-failure.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0011"

	server := openQuackMirrorServer(t, ctx, path, uri, token)
	_, err := server.ExecContext(ctx,
		`CREATE TABLE failed_reattach_test (
			id TEXT PRIMARY KEY,
			value INTEGER
		)`,
	)
	require.NoError(t, err, "create failed reattach table")
	_, err = server.ExecContext(ctx,
		`INSERT INTO failed_reattach_test VALUES (?, ?)`,
		"seed", 41,
	)
	require.NoError(t, err, "insert seed row")

	store, err := NewQuackStore(uri, token, false)
	require.NoError(t, err, "open quack store")
	t.Cleanup(func() {
		require.NoError(t, store.Close(), "close quack store")
	})
	_, err = server.ExecContext(ctx, `CALL quack_stop(?)`, uri)
	require.NoError(t, err, "stop quack server")

	var got int
	err = store.queryRowContext(ctx,
		`SELECT value FROM failed_reattach_test WHERE id = ?`,
		"seed",
	).Scan(&got)
	require.Error(t, err, "query while server is stopped should fail")

	_, err = server.ExecContext(ctx, `CALL quack_serve(?, token => ?)`, uri, token)
	require.NoError(t, err, "restart quack server")
	_, err = store.quack.DB().ExecContext(ctx, "USE memory")
	require.NoError(t, err, "select local catalog before forced detach")
	_, err = store.quack.DB().ExecContext(ctx, "DETACH "+quackAttachmentName)
	require.NoError(t, err, "force stranded detached Quack attachment")
	require.NoError(t,
		store.queryRowContext(ctx,
			`SELECT value FROM failed_reattach_test WHERE id = ?`,
			"seed",
		).Scan(&got),
		"query after failed reattach and server restart",
	)
	assert.Equal(t, 41, got)
}

func TestQuackStoreAnalyticsDashboardReads(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack-analytics.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0006"

	// Push is local-only now, so the mirror is populated before the file is
	// handed to quack_serve for read access.
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	seedDuckEdit(
		t, local, "alpha", "duck-sync-edit",
		0, 0, "src/main.go", "2026-01-10T02:00:00Z",
	)
	result, err := Push(ctx, path, local, "quack-client", SyncOptions{}, true, nil)
	require.NoError(t, err, "push analytics fixture into local mirror")
	assert.Equal(t, 3, result.SessionsPushed)
	assert.Equal(t, 4, result.MessagesPushed)

	openQuackMirrorServer(t, ctx, path, uri, token)

	store, err := NewQuackStore(uri, token, false)
	require.NoError(t, err, "open Quack-backed store")
	t.Cleanup(func() {
		require.NoError(t, store.Close(), "close Quack-backed store")
	})

	configStatus, err := ReadStatusFromConfig(ctx, config.DuckDBConfig{
		URL:         uri,
		Token:       token,
		MachineName: "quack-client",
	})
	require.NoError(t, err, "read Quack-backed status from config")
	assert.Equal(t, "quack-client", configStatus.Machine)
	assert.Equal(t, "quack-client", configStatus.LastPushMachine,
		"status should report the SERVER file's own push metadata")
	assert.NotEmpty(t, configStatus.LastPushAt)
	assert.Equal(t, SchemaVersion, configStatus.SchemaVersion)
	assert.Equal(t, db.CurrentDataVersion(), configStatus.DataVersion)
	assert.Empty(t, configStatus.Scope, "unfiltered push canonicalizes to empty scope")
	assert.Equal(t, 3, configStatus.DuckDBSessions)
	assert.Equal(t, 4, configStatus.DuckDBMessages)

	filter := db.AnalyticsFilter{
		From:     "2026-01-10",
		To:       "2026-01-11",
		Timezone: "UTC",
	}
	page, err := store.ListSessions(ctx, db.SessionFilter{Limit: 10})
	require.NoError(t, err, "list sessions through Quack")
	assert.Len(t, page.Sessions, 3)

	sidebar, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{})
	require.NoError(t, err, "read sidebar session index through Quack")
	assert.Equal(t, 3, sidebar.Total)

	search, err := store.Search(ctx, db.SearchFilter{Query: "alpha", Limit: 10})
	require.NoError(t, err, "search sessions through Quack")
	assert.NotEmpty(t, search.Results)

	ordinals, err := store.SearchSession(ctx, "duck-sync-alpha", "duck result")
	require.NoError(t, err, "search session through Quack")
	assert.NotEmpty(t, ordinals)

	msgs, err := store.GetMessages(ctx, "duck-sync-alpha", 0, 10, true)
	require.NoError(t, err, "read messages through Quack")
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "duck result", msgs[1].ToolCalls[0].ResultEvents[0].Content)

	timing, err := store.GetSessionTiming(ctx, "duck-sync-alpha")
	require.NoError(t, err, "read session timing through Quack")
	require.NotNil(t, timing)
	assert.NotEmpty(t, timing.Turns)

	stars, err := store.ListStarredSessionIDs(ctx)
	require.NoError(t, err, "read starred sessions through Quack")
	assert.Equal(t, []string{"duck-sync-alpha"}, stars)

	pins, err := store.ListPinnedMessages(ctx, "duck-sync-alpha", "")
	require.NoError(t, err, "read pinned messages through Quack")
	require.Len(t, pins, 1)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, "pin alpha", *pins[0].Note)

	findings, err := store.ListSecretFindings(ctx, db.SecretFindingFilter{
		Project: "alpha",
		Limit:   10,
	})
	require.NoError(t, err, "read secret findings through Quack")
	require.Len(t, findings.Findings, 1)
	assert.Equal(t, "test_secret", findings.Findings[0].RuleName)

	activity, err := store.GetAnalyticsActivity(ctx, filter, "day")
	require.NoError(t, err, "read activity analytics through Quack")
	assert.NotEmpty(t, activity.Series)

	hours, err := store.GetAnalyticsHourOfWeek(ctx, filter)
	require.NoError(t, err, "read hour-of-week analytics through Quack")
	assert.NotEmpty(t, hours.Cells)

	top, err := store.GetAnalyticsTopSessions(ctx, filter, "messages")
	require.NoError(t, err, "read top sessions through Quack")
	require.NotEmpty(t, top.Sessions)
	assert.Equal(t, "duck-sync-alpha", top.Sessions[0].ID)

	tools, err := store.GetAnalyticsTools(ctx, filter)
	require.NoError(t, err, "read tool analytics through Quack")
	assert.Equal(t, 2, tools.TotalCalls)

	edits, err := store.RecentEdits(ctx, db.RecentEditsParams{})
	require.NoError(t, err, "read recent edits through Quack")
	require.NotEmpty(t, edits.Files)
	assert.Equal(t, "src/main.go", edits.Files[0].FilePath)

	report, err := store.GetActivityReport(
		ctx,
		db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-01-10", "UTC"),
	)
	require.NoError(t, err, "read activity report through Quack")
	assert.GreaterOrEqual(t, report.Totals.Sessions, 1)
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
