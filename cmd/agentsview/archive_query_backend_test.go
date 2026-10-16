package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestDaemonArchiveQueryBackendDefaultsMappedLocalTimezone(t *testing.T) {
	t.Setenv("TZ", "America/New_York")
	oldLocal := time.Local                                         //nolint:forbidigo // Exercise report formatting and date buckets in the local calendar timezone.
	time.Local = time.FixedZone("Eastern Standard Time", -5*60*60) //nolint:forbidigo // Exercise report formatting and date buckets in the local calendar timezone.
	t.Cleanup(func() { time.Local = oldLocal })                    //nolint:forbidigo // Exercise report formatting and date buckets in the local calendar timezone.

	var queries []url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		writeJSONResponse(w, `{"timezone":"America/New_York"}`)
	}))
	t.Cleanup(ts.Close)
	backend := daemonArchiveQueryBackend{tr: transport{URL: ts.URL}}

	_, err := backend.ActivityReport(t.Context(), ActivityReportConfig{
		Preset: "day",
	})
	require.NoError(t, err)
	require.Len(t, queries, 1)
	assert.Equal(t, "America/New_York", queries[0].Get("timezone"))
	assert.Equal(t, todayIn("America/New_York"), queries[0].Get("date"))

	_, err = backend.ActivityReport(t.Context(), ActivityReportConfig{
		Preset: "day", Date: "2026-03-09", Timezone: "Europe/Berlin",
	})
	require.NoError(t, err)
	require.Len(t, queries, 2)
	assert.Equal(t, "Europe/Berlin", queries[1].Get("timezone"))
	assert.Equal(t, "2026-03-09", queries[1].Get("date"))
}

func TestResolveArchiveQueryBackendNoSyncStartsNoSyncDaemon(t *testing.T) {
	testDataDir(t)
	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, cfg *config.Config, _ time.Duration, _ bool,
	) (*DaemonRuntime, error) {
		started = true
		assert.True(t, cfg.NoSync)
		return &DaemonRuntime{Host: "127.0.0.1", Port: 12345}, nil
	})

	backend := resolveTestArchiveQueryBackend(t, defaultArchiveQueryPolicy(
		func(p *archiveQueryPolicy) { p.NoSync = true },
	))
	assert.True(t, started)
	assert.IsType(t, daemonArchiveQueryBackend{}, backend)
}

func TestResolveArchiveQueryBackendRefusesReadOnlyDaemonForFreshQueries(t *testing.T) {
	dataDir := testDataDir(t)

	var called bool
	ts := sessionUsageRuntimeServer(t, func(
		w http.ResponseWriter, r *http.Request,
	) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	})
	registerTestRuntime(t, dataDir, ts.URL, true)

	_, cleanup, err := resolveArchiveQueryBackend(
		t.Context(), defaultArchiveQueryPolicy(nil),
	)
	if cleanup != nil {
		t.Cleanup(cleanup)
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only")
	assert.NotContains(t, err.Error(), "--pg")
	assert.False(t, called)
}

func TestResolveArchiveQueryBackendUsesGeneratedAutostartToken(t *testing.T) {
	testDataDir(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer generated-token", r.Header.Get("Authorization"))
		assert.Equal(t, "/api/v1/sync", r.URL.Path)
		assert.Equal(t, "true", r.URL.Query().Get("startup_only"))
		writeJSONResponse(w, `{}`)
	}))
	t.Cleanup(ts.Close)

	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, cfg *config.Config, _ time.Duration, _ bool,
	) (*DaemonRuntime, error) {
		cfg.AuthToken = "generated-token"
		return daemonRuntimeFromTestURL(t, ts.URL), nil
	})

	backend := resolveTestArchiveQueryBackend(t, defaultArchiveQueryPolicy(
		func(p *archiveQueryPolicy) {
			p.AutoStart = true
			p.ReadOnlyDaemon = archiveQueryRejectReadOnlyDaemon
		},
	))

	daemonBackend, ok := backend.(daemonArchiveQueryBackend)
	require.True(t, ok)
	assert.Equal(t, "generated-token", daemonBackend.authToken)
}

func TestLocalArchiveQuerySessionUsageNoSyncSkipsSingleSessionSync(
	t *testing.T,
) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sessions.db")
	writer := dbtest.OpenTestDBAt(t, dbPath)
	started := "2026-06-23T12:00:00Z"
	require.NoError(t, writer.UpsertSession(t.Context(), db.Session{
		ID:                   "codex:no-sync-usage",
		Project:              "proj",
		Machine:              "local",
		Agent:                "codex",
		StartedAt:            &started,
		MessageCount:         1,
		TotalOutputTokens:    42,
		HasTotalOutputTokens: true,
	}))
	require.NoError(t, writer.Close())

	readonly, err := db.OpenReadOnly(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { readonly.Close() })

	backend := localArchiveQueryBackend{
		cfg:           config.Config{DBPath: dbPath},
		database:      readonly,
		offline:       true,
		skipFreshData: true,
	}
	stderr := captureStderr(t, func() {
		out, exitCode, err := backend.SessionUsage(
			t.Context(),
			sessionUsageQuery{SessionID: "codex:no-sync-usage"},
		)
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.Equal(t, tokenUseExitOK, exitCode)
		assert.Equal(t, 42, out.TotalOutputTokens)
	})
	assert.NotContains(t, stderr, "warning: usage refresh incomplete")
}

// TestLocalSessionUsageRefreshesSubagentTranscripts covers the freshness
// half of the subagent rollup: SyncSingleSession only knows about the named
// session's file, so the backend must also ingest the agent-*.jsonl files
// beside it. Without that, a session that just finished would report a
// combined cost missing its most recent subagents.
func TestLocalSessionUsageRefreshesSubagentTranscripts(t *testing.T) {
	dataDir := testDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, def := range parser.Registry {
		if def.EnvVar != "" {
			t.Setenv(def.EnvVar,
				filepath.Join(home, "agent-dirs", string(def.Type)))
		}
	}
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", claudeDir)

	projDir := filepath.Join(claudeDir, "-home-proj")
	require.NoError(t, os.MkdirAll(
		filepath.Join(projDir, "parent-uuid", "subagents"), 0o755))
	parentPath := filepath.Join(projDir, "parent-uuid.jsonl")
	require.NoError(t, os.WriteFile(parentPath, []byte(
		testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-05-20T10:00:00Z", "delegate this").
			AddClaudeAssistant("2026-05-20T10:00:05Z", "on it").
			String(),
	), 0o644))

	dbPath := sessionsDBPath(dataDir)
	database := dbtest.OpenTestDBAt(t, dbPath)
	backend := localArchiveQueryBackend{
		cfg: config.Config{
			DBPath: dbPath,
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentClaude: {claudeDir},
			},
		},
		database: database,
		offline:  true,
	}
	ctx := t.Context()

	// Ingest the parent, then write a subagent transcript the way Claude
	// Code does after the parent's own file was last synced.
	_, _, err := backend.SessionUsage(
		ctx, sessionUsageQuery{SessionID: "parent-uuid"})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(
		filepath.Join(projDir, "parent-uuid", "subagents",
			"agent-worker1.jsonl"),
		[]byte(testjsonl.NewSessionBuilder().
			AddClaudeUserWithSessionID(
				"2026-05-20T10:01:00Z", "do the subtask", "parent-uuid").
			AddClaudeAssistant("2026-05-20T10:01:30Z", "subtask done").
			String()),
		0o644))

	// Passive reads must leave newly written source transcripts unindexed.
	archived, _, err := backend.SessionUsage(
		ctx, sessionUsageQuery{SessionID: "parent-uuid", NoSync: true})
	require.NoError(t, err)
	require.NotNil(t, archived)
	assert.Zero(t, archived.SubagentCount)
	unsynced, err := database.GetSession(ctx, "agent-worker1")
	require.NoError(t, err)
	assert.Nil(t, unsynced)

	out, _, err := backend.SessionUsage(
		ctx, sessionUsageQuery{SessionID: "parent-uuid"})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, 1, out.SubagentCount,
		"the new subagent transcript must be ingested before the query")

	child, err := database.GetSession(ctx, "agent-worker1")
	require.NoError(t, err)
	require.NotNil(t, child, "subagent session was not synced")
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "parent-uuid", *child.ParentSessionID)

	archived, _, err = backend.SessionUsage(
		ctx, sessionUsageQuery{SessionID: "parent-uuid", NoSync: true})
	require.NoError(t, err)
	require.NotNil(t, archived)
	assert.Equal(t, out.SessionUsage, archived.SessionUsage)

	// --own-only skips the subagent refresh and the combined view.
	own, _, err := backend.SessionUsage(
		ctx, sessionUsageQuery{SessionID: "parent-uuid", OwnOnly: true})
	require.NoError(t, err)
	require.NotNil(t, own)
	assert.Zero(t, own.SubagentCount)
}

func TestLocalSessionUsageStaleArchiveReturnsStoredUsage(t *testing.T) {
	database, backend, freshID := staleSessionUsageBackend(t)
	stderr := captureStderr(t, func() {
		out, code, err := backend.SessionUsage(t.Context(), sessionUsageQuery{SessionID: "saved"})
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.Equal(t, 42, out.TotalOutputTokens)
		assert.True(t, out.RefreshIncomplete)
		assert.Equal(t, tokenUseExitOK, code)
	})
	assert.True(t, database.NeedsResync())
	assert.Contains(t, stderr, "warning: archive data version changed; showing stored usage until the archive is rebuilt")
	retryStderr := captureStderr(t, func() {
		out, code, err := backend.SessionUsage(t.Context(), sessionUsageQuery{SessionID: "codex:" + freshID})
		assert.Nil(t, out)
		require.ErrorContains(t, err, "archive rebuild")
		assert.Equal(t, tokenUseExitErr, code)
	})
	assert.NotContains(t, retryStderr, "session not found")
	out, _, err := backend.SessionUsage(t.Context(), sessionUsageQuery{SessionID: "saved", NoSync: true})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.False(t, out.RefreshIncomplete)
}

func TestLocalSessionUsageReportsNotFoundWhenStale(t *testing.T) {
	_, backend, _ := staleSessionUsageBackend(t)
	missingStderr := captureStderr(t, func() {
		out, code, err := backend.SessionUsage(t.Context(), sessionUsageQuery{SessionID: "missing"})
		require.NoError(t, err)
		assert.Nil(t, out)
		assert.Equal(t, tokenUseExitNotFound, code)
	})
	assert.NotContains(t, missingStderr, "warning:")
}

func staleSessionUsageBackend(t *testing.T) (*db.DB, localArchiveQueryBackend, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	writer := dbtest.OpenTestDBAt(t, dbPath)
	root := t.TempDir()
	codexRoot := t.TempDir()
	source := filepath.Join(root, "project-a", "saved.jsonl")
	dbtest.WriteTestFile(t, source, []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "changed source").String()))
	const freshID = "123e4567-e89b-42d3-a456-426614174000"
	freshSource := filepath.Join(codexRoot,
		"rollout-2026-01-01T00-00-00-"+freshID+".jsonl")
	dbtest.WriteTestFile(t, freshSource, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(freshID, "project-a", "user", "2026-01-01T00:00:00Z"),
		testjsonl.CodexMsgJSON("user", "fresh source", "2026-01-01T00:00:00Z"),
	)))
	require.NoError(t, writer.UpsertSession(t.Context(), db.Session{ID: "saved", Agent: "claude", Project: "project-a", FilePath: new(source), TotalOutputTokens: 42, HasTotalOutputTokens: true}))
	require.NoError(t, writer.Close())
	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), "PRAGMA user_version = 0")
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	database, err := db.OpenIsolatedContext(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })
	agentDirs := map[parser.AgentType][]string{
		parser.AgentClaude: {root},
		parser.AgentCodex:  {codexRoot},
	}
	backend := localArchiveQueryBackend{database: database, offline: true, cfg: config.Config{AgentDirs: agentDirs}}
	return database, backend, freshID
}
