//go:build duckdbtest && !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

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

func TestQuackStoreAnalyticsDashboardReads(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack-analytics.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0006"

	server := openQuackMirrorServer(t, ctx, path, uri, token)
	require.NoError(t, EnsureSchema(ctx, server), "prepare server schema")

	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	seedDuckEdit(
		t, local, "alpha", "duck-sync-edit",
		0, 0, "src/main.go", "2026-01-10T02:00:00Z",
	)
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
	require.NoError(t, err, "push analytics fixture through Quack")
	assert.Equal(t, 3, result.SessionsPushed)
	assert.Equal(t, 4, result.MessagesPushed)

	store, err := NewQuackStore(uri, token, false)
	require.NoError(t, err, "open Quack-backed store")
	t.Cleanup(func() {
		require.NoError(t, store.Close(), "close Quack-backed store")
	})

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
	watermark, err := local.GetSyncState(syncer.syncStateKey(lastPushStateKey))
	require.NoError(t, err, "read Quack push watermark")
	assert.NotEmpty(t, watermark, "successful Quack push must advance watermark")

	status, err := syncer.Status(ctx)
	require.NoError(t, err, "read Quack-backed sync status")
	assert.Equal(t, "quack-client", status.Machine)
	assert.Equal(t, 2, status.DuckDBSessions)
	assert.Equal(t, 3, status.DuckDBMessages)

	configStatus, err := ReadStatusFromConfig(ctx, config.DuckDBConfig{
		URL:         uri,
		Token:       token,
		MachineName: "quack-client",
	}, "2026-06-30T12:00:00.000Z")
	require.NoError(t, err, "read Quack-backed status from config")
	assert.Equal(t, "quack-client", configStatus.Machine)
	assert.Equal(t, "2026-06-30T12:00:00.000Z", configStatus.LastPushAt)
	assert.Equal(t, 2, configStatus.DuckDBSessions)
	assert.Equal(t, 3, configStatus.DuckDBMessages)

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

	alphaState := readDuckMirrorSessionState(t, ctx, server, fixture.alphaID)
	betaState := readDuckMirrorSessionState(t, ctx, server, fixture.betaID)
	result, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err, "idempotent repeat push through Quack")
	assert.Equal(t, alphaState, readDuckMirrorSessionState(t, ctx, server, fixture.alphaID))
	assert.Equal(t, betaState, readDuckMirrorSessionState(t, ctx, server, fixture.betaID))

	mutatedAt := "2026-01-10T03:04:05.123456Z"
	mutatedResult := "duck result changed\nquoted ' output\ncontains $$ delimiter"
	mutatedFilePath := "src/quoted ' file\ncontains $$ delimiter"
	_, err = local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			fixture.alphaID, "alpha", "alpha changed", mutatedAt, 2,
		),
		Messages: []db.Message{
			syncMessage(
				fixture.alphaID, 0, "user", "alpha changed", mutatedAt,
			),
			syncMessage(
				fixture.alphaID, 1, "assistant",
				"alpha changed assistant",
				"2026-01-10T03:04:06.000Z",
				db.ToolCall{
					ToolName:  "grep",
					Category:  "search",
					SkillName: "duck-grep",
					ToolUseID: "tool-alpha",
					InputJSON: `{"query":"changed"}`,
					FilePath:  mutatedFilePath,
					ResultEvents: []db.ToolResultEvent{{
						Source:        "tool",
						Status:        "complete",
						Content:       mutatedResult,
						Timestamp:     "2026-01-10T03:04:07.000Z",
						EventIndex:    0,
						ContentLength: len(mutatedResult),
					}},
				},
			),
		},
		DataVersion:     2,
		ReplaceMessages: true,
	}})
	require.NoError(t, err, "mutate local alpha session")

	mutatedMessages, err := local.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err, "read mutated local messages")
	require.Len(t, mutatedMessages, 2)
	assistantMessage := mutatedMessages[1]
	_, err = server.ExecContext(ctx, `
		INSERT INTO tool_calls (
			message_id, session_id, tool_name, category, call_index,
			tool_use_id, input_json, skill_name
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		assistantMessage.ID, fixture.alphaID, "stale-tool", "stale",
		0, "stale-tool-use", `{"query":"stale"}`, "stale-skill",
	)
	require.NoError(t, err, "seed stale matching remote tool_call")

	result, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err, "mutated repeat push through Quack")
	assert.Equal(t, 2, result.SessionsPushed)
	assert.Equal(t, 3, result.MessagesPushed)
	assertDuckDBCount(t, server, "sessions", 2)
	assertDuckDBCount(t, server, "messages", 3)
	assertDuckDBCount(t, server, "tool_calls", 1)
	assertDuckDBCount(t, server, "tool_result_events", 1)
	assertDuckDBCount(t, server, "cursor_usage_events", 2)

	var firstMessage string
	var startedAt time.Time
	require.NoError(t,
		server.QueryRowContext(ctx,
			`SELECT first_message, started_at FROM sessions WHERE id = ?`,
			fixture.alphaID,
		).Scan(&firstMessage, &startedAt),
		"query mutated session",
	)
	assert.Equal(t, "alpha changed", firstMessage)
	assert.Equal(t,
		time.Date(2026, time.January, 10, 3, 4, 5, 123456000, time.UTC),
		startedAt.UTC(),
	)

	var toolName, category, skillName, inputJSON, filePath string
	require.NoError(t,
		server.QueryRowContext(ctx, `
			SELECT tool_name, category, skill_name, input_json, file_path
			FROM tool_calls
			WHERE session_id = ? AND message_id = ? AND call_index = ?`,
			fixture.alphaID, assistantMessage.ID, 0,
		).Scan(&toolName, &category, &skillName, &inputJSON, &filePath),
		"query updated tool_call",
	)
	assert.Equal(t, "grep", toolName)
	assert.Equal(t, "search", category)
	assert.Equal(t, "duck-grep", skillName)
	assert.Equal(t, `{"query":"changed"}`, inputJSON)
	assert.Equal(t, mutatedFilePath, filePath)

	var resultStatus, resultContent string
	require.NoError(t,
		server.QueryRowContext(ctx, `
			SELECT status, content
			FROM tool_result_events
			WHERE session_id = ?
			  AND tool_call_message_ordinal = ?
			  AND call_index = ?
			  AND event_index = ?`,
			fixture.alphaID, 1, 0, 0,
		).Scan(&resultStatus, &resultContent),
		"query updated tool_result_event",
	)
	assert.Equal(t, "complete", resultStatus)
	assert.Equal(t, mutatedResult, resultContent)

	shrunkenAt := "2026-01-10T04:00:00.000Z"
	_, err = local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			fixture.alphaID, "alpha", "alpha without tool", shrunkenAt, 2,
		),
		Messages: []db.Message{
			syncMessage(
				fixture.alphaID, 0, "user", "alpha without tool", shrunkenAt,
			),
			syncMessage(
				fixture.alphaID, 1, "assistant",
				"alpha assistant without tool",
				"2026-01-10T04:00:01.000Z",
			),
		},
		DataVersion:     3,
		ReplaceMessages: true,
	}})
	require.NoError(t, err, "shrink local alpha tool set")

	result, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err, "shrunken repeat push through Quack")
	assert.Equal(t, 2, result.SessionsPushed)
	assert.Equal(t, 3, result.MessagesPushed)
	assertDuckDBCount(t, server, "sessions", 2)
	assertDuckDBCount(t, server, "messages", 3)
	assertDuckDBCount(t, server, "tool_calls", 0)
	assertDuckDBCount(t, server, "tool_result_events", 0)
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

func TestQuackClientSyncPushReattachesAfterServerRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack-push-reattach.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0010"

	server := openQuackMirrorServer(t, ctx, path, uri, token)
	require.NoError(t, EnsureSchema(ctx, server), "prepare server schema")
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
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
	require.NoError(t, syncer.EnsureSchema(ctx), "warm client schema")

	_, err = server.ExecContext(ctx, `CALL quack_stop(?)`, uri)
	require.NoError(t, err, "stop quack server")
	_, err = server.ExecContext(ctx, `CALL quack_serve(?, token => ?)`, uri, token)
	require.NoError(t, err, "restart quack server")

	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err, "push through reattached Quack client")
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
}

func TestQuackClientSyncPushFailedSessionLeavesRemoteRowsUnchanged(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack-push-failure.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0007"

	server := openQuackMirrorServer(t, ctx, path, uri, token)
	require.NoError(t, EnsureSchema(ctx, server), "prepare server schema")

	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
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
	require.NoError(t, err, "initial push through Quack")
	require.Equal(t, 0, result.Errors)
	before := readDuckMirrorSessionState(t, ctx, server, fixture.alphaID)

	_, err = local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			fixture.alphaID, "alpha", "alpha poisoned",
			"2026-01-10T05:00:00.000Z", 1,
		),
		Messages: []db.Message{
			syncMessage(
				fixture.alphaID, 0, "user", "alpha poisoned",
				"not-a-timestamp",
			),
		},
		DataVersion:     2,
		ReplaceMessages: true,
	}})
	require.NoError(t, err, "write local session with invalid message timestamp")

	result, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err, "failed session should be counted, not fatal")
	assert.Equal(t, 1, result.Errors)

	after := readDuckMirrorSessionState(t, ctx, server, fixture.alphaID)
	assert.Equal(t, before, after)
}

func TestQuackRemoteMutationBatchCoalescesAgainstServer(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack-batch.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0008"

	server := openQuackMirrorServer(t, ctx, path, uri, token)
	_, err := server.ExecContext(ctx,
		`CREATE TABLE remote_batch_test (
			id INTEGER PRIMARY KEY,
			value INTEGER
		)`,
	)
	require.NoError(t, err, "create remote batch test table")
	client := openQuackClientAttachment(t, ctx, uri, token)
	execRemote := func(ctx context.Context, sqlText string) error {
		_, err := client.ExecContext(
			ctx, "FROM "+quackAttachmentName+".query(?)", sqlText,
		)
		return err
	}

	success := &duckRemoteMutationBatch{}
	_, err = success.ExecContext(ctx,
		`INSERT INTO remote_batch_test VALUES (?, ?)`, 1, 11,
	)
	require.NoError(t, err)
	_, err = success.ExecContext(ctx,
		`INSERT INTO remote_batch_test VALUES (?, ?)`, 2, 22,
	)
	require.NoError(t, err)
	require.NoError(t, execDuckRemoteMutationBatch(
		ctx, execRemote, "quack smoke success", success, true,
	))
	assertDuckDBCount(t, server, "remote_batch_test", 2)

	failing := &duckRemoteMutationBatch{}
	_, err = failing.ExecContext(ctx,
		`INSERT INTO remote_batch_test VALUES (?, ?)`, 3, 33,
	)
	require.NoError(t, err)
	_, err = failing.ExecContext(ctx,
		`INSERT INTO remote_batch_test VALUES (?, ?)`, 4, "not-an-int",
	)
	require.NoError(t, err)
	err = execDuckRemoteMutationBatch(
		ctx, execRemote, "quack smoke failure", failing, true,
	)
	require.Error(t, err, "coalesced batch should surface server error")
	assert.Contains(t, err.Error(), "not-an-int")
	assertDuckDBCount(t, server, "remote_batch_test", 2)
}

func TestQuackRemoteMutationOversizeStatementFallbackRollsBackSession(
	t *testing.T,
) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentsview-quack-oversize.duckdb")
	uri := "quack:127.0.0.1:" + freeTCPPort(t)
	const token = "agentsview-duckdbtest-token-0009"

	server := openQuackMirrorServer(t, ctx, path, uri, token)
	_, err := server.ExecContext(ctx,
		`CREATE TABLE remote_oversize_atomic_test (
			session_id VARCHAR,
			ordinal INTEGER,
			content VARCHAR,
			PRIMARY KEY(session_id, ordinal)
		)`,
	)
	require.NoError(t, err, "create remote oversize atomic test table")
	client := openQuackClientAttachment(t, ctx, uri, token)
	execRemote := func(ctx context.Context, sqlText string) error {
		_, err := client.ExecContext(
			ctx, "FROM "+quackAttachmentName+".query(?)", sqlText,
		)
		return err
	}

	const sessionID = "oversize-atomic-session"
	_, err = server.ExecContext(ctx,
		`INSERT INTO remote_oversize_atomic_test VALUES (?, ?, ?)`,
		sessionID, 99, "stale",
	)
	require.NoError(t, err, "seed stale remote row")

	batch := &duckRemoteMutationBatch{}
	_, err = batch.ExecContext(ctx,
		`DELETE FROM remote_oversize_atomic_test WHERE session_id = ?`,
		sessionID,
	)
	require.NoError(t, err)
	deleteOnly := &duckRemoteMutationBatch{statements: batch.statements[:1]}
	_, err = batch.ExecContext(ctx,
		`INSERT INTO remote_oversize_atomic_test VALUES (?, ?, ?)`,
		sessionID, 0, "replacement",
	)
	require.NoError(t, err)
	_, err = batch.ExecContext(ctx,
		`INSERT INTO remote_oversize_atomic_test VALUES (?, ?, ?)`,
		sessionID, "not-an-int", "poison",
	)
	require.NoError(t, err)

	var coalescedCalls int
	err = execDuckRemoteMutationBatchOversizeWithStatementFallback(
		ctx,
		func(ctx context.Context, sqlText string) error {
			coalescedCalls++
			return execRemote(ctx, sqlText)
		},
		execRemote,
		"quack oversize atomic",
		batch,
		deleteOnly.transactionBytes(),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-an-int")
	assert.Equal(t, 0, coalescedCalls)

	var ordinal int
	var content string
	err = server.QueryRowContext(ctx,
		`SELECT ordinal, content
		 FROM remote_oversize_atomic_test
		 WHERE session_id = ?`,
		sessionID,
	).Scan(&ordinal, &content)
	require.NoError(t, err, "read rolled back session row")
	assert.Equal(t, 99, ordinal)
	assert.Equal(t, "stale", content)
}

type duckMirrorSessionState struct {
	FirstMessage         string
	MessageCount         int
	ToolCallCount        int
	ToolResultEventCount int
	UsageEventCount      int
	SecretFindingCount   int
}

func readDuckMirrorSessionState(
	t *testing.T, ctx context.Context, conn *sql.DB, sessionID string,
) duckMirrorSessionState {
	t.Helper()
	state := duckMirrorSessionState{
		MessageCount: duckDBCountWhere(t, ctx, conn, "messages", "session_id = ?", sessionID),
		ToolCallCount: duckDBCountWhere(
			t, ctx, conn, "tool_calls", "session_id = ?", sessionID,
		),
		ToolResultEventCount: duckDBCountWhere(
			t, ctx, conn, "tool_result_events", "session_id = ?", sessionID,
		),
		UsageEventCount: duckDBCountWhere(
			t, ctx, conn, "usage_events", "session_id = ?", sessionID,
		),
		SecretFindingCount: duckDBCountWhere(
			t, ctx, conn, "secret_findings", "session_id = ?", sessionID,
		),
	}
	require.NoError(t,
		conn.QueryRowContext(ctx,
			`SELECT first_message FROM sessions WHERE id = ?`,
			sessionID,
		).Scan(&state.FirstMessage),
		"read mirrored session state",
	)
	return state
}

func duckDBCountWhere(
	t *testing.T, ctx context.Context, conn *sql.DB, table, where string, arg any,
) int {
	t.Helper()
	var got int
	require.NoError(t, conn.QueryRowContext(
		ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+where, arg,
	).Scan(&got))
	return got
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

func openQuackClientAttachment(
	t *testing.T, ctx context.Context, uri, token string,
) *sql.DB {
	t.Helper()
	client, err := sql.Open("duckdb", "")
	require.NoError(t, err, "open Quack client DuckDB")
	client.SetMaxOpenConns(1)
	client.SetMaxIdleConns(1)
	t.Cleanup(func() {
		require.NoError(t, client.Close(), "close Quack client DuckDB")
	})
	_, err = client.ExecContext(ctx, "LOAD quack")
	require.NoError(t, err, "load quack extension in client")
	_, err = client.ExecContext(ctx, fmt.Sprintf(
		`ATTACH '%s' AS %s (TOKEN '%s')`,
		uri, quackAttachmentName, token,
	))
	require.NoError(t, err, "attach quack endpoint")
	return client
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
